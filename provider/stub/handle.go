package stub

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// stubPullRequestURL is the synthetic PR URL the canonical
// BehaviorSucceedWithPR sequence references. F.4 smoke harness asserts
// on this exact value.
const stubPullRequestURL = "stub://pr/123"

// handle is the concrete agent.Handle returned by provider.Spawn /
// Provider.Resume. It owns the events channel and a pair of cancel /
// inject channels used to coordinate Stop and Inject with the scripting
// goroutine.
type handle struct {
	sessionID string
	behavior  Behavior
	spec      agent.Spec
	resumed   bool

	events  chan agent.Event
	injects chan string
	stopped chan struct{}
	done    chan struct{} // closed by run() after the events channel closes

	once    sync.Once // guards close(stopped) so Stop is idempotent
	closeMu sync.Mutex
	closed  bool // true once events has been closed
}

// newHandle constructs an unstarted handle. Callers should invoke
// (*handle).run on a fresh goroutine to begin emitting events.
func newHandle(sessionID string, b Behavior, spec agent.Spec, resumed bool) *handle {
	return &handle{
		sessionID: sessionID,
		behavior:  b,
		spec:      spec,
		resumed:   resumed,
		// Buffer = 32 lets the scripting goroutine emit a full canonical
		// sequence (~7 events) without blocking on a slow consumer.
		events:  make(chan agent.Event, 32),
		injects: make(chan string, 16),
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// SessionID returns the stub's pre-assigned session id. Unlike real
// providers the stub assigns the id eagerly so callers can read it
// without first consuming an InitEvent.
func (h *handle) SessionID() string { return h.sessionID }

// Events returns the read-only event channel. The provider closes the
// channel when the scripted sequence ends, when ctx cancels, or when
// Stop is invoked.
func (h *handle) Events() <-chan agent.Event { return h.events }

// Inject sends a follow-up user message into the scripting goroutine.
// Behaviors that do not consume injects ignore the message; the only
// behavior that observes injects is BehaviorInjectTest.
//
// Returns agent.ErrUnsupported if the parent provider's capability
// matrix has SupportsMessageInjection turned off (set via
// WithCapabilities). The check uses spec.ProviderConfig as a side
// channel so handles created via Resume continue to honor the same
// capability matrix without referencing the parent provider.
func (h *handle) Inject(ctx context.Context, text string) error {
	// The stub itself supports injection by construction; the
	// capability gate lives on the provider so tests that flip it off
	// read the unsupported error from here. We surface it via a
	// per-handle flag captured at construction time. To keep handle
	// independent of the provider we re-derive support from the spec:
	// when a test wants the gate off it sets
	// ProviderConfig["stub.injectUnsupported"] = true.
	if v, ok := h.spec.ProviderConfig["stub.injectUnsupported"]; ok {
		if b, ok := v.(bool); ok && b {
			return agent.ErrUnsupported
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.stopped:
		return errors.New("stub: handle stopped")
	case h.injects <- text:
		return nil
	}
}

// Stop terminates the scripted sequence. Subsequent calls are no-ops.
// On the first invocation Stop signals the scripting goroutine to
// emit a terminal ResultEvent with ErrorSubtype "stopped" and close
// the events channel. Honors ctx for the close-grace deadline.
func (h *handle) Stop(ctx context.Context) error {
	h.once.Do(func() { close(h.stopped) })
	// Wait for the scripting goroutine to drain. The done channel is
	// closed at the very end of (*handle).run, after the events
	// channel has been closed.
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run executes the scripted sequence for the handle's behavior. It is
// the single writer to h.events and is responsible for closing the
// channel exactly once when the script ends or the session aborts.
//
// On Stop the script bails out via select-on-h.stopped inside emit;
// run then writes a terminal ResultEvent{ErrorSubtype: "stopped"}
// before closing so consumers always see a terminal event. After the
// events channel is closed run signals the done channel so Stop can
// unblock its caller.
func (h *handle) run(ctx context.Context) {
	defer close(h.done)
	defer h.closeEvents()

	if !h.emitInit(ctx) {
		h.maybeEmitStoppedResult(ctx)
		return
	}

	switch h.behavior {
	case BehaviorSucceedWithPR:
		h.scriptSucceedWithPR(ctx)
	case BehaviorFailOnClone:
		h.scriptFailOnClone(ctx)
	case BehaviorHangThenTimeout:
		h.scriptHangThenTimeout(ctx)
	case BehaviorSilentFail:
		// Init already emitted; close immediately, no terminal Result.
		return
	case BehaviorSlowTool:
		h.scriptSlowTool(ctx)
	case BehaviorCostOverrun:
		h.scriptCostOverrun(ctx)
	case BehaviorMidStreamError:
		h.scriptMidStreamError(ctx)
	case BehaviorInjectTest:
		h.scriptInjectTest(ctx)
	default:
		// Unknown behaviors should have been remapped at Spawn; if
		// one slipped through, fall back to the canonical success path
		// so tests that misconfigure the behavior name fail loudly via
		// the assertion rather than silently hanging.
		h.scriptSucceedWithPR(ctx)
	}

	h.maybeEmitStoppedResult(ctx)
}

// maybeEmitStoppedResult writes a terminal ResultEvent with
// ErrorSubtype "stopped" iff Stop was invoked before the script
// completed naturally. The events channel is unbuffered-on-this-write
// so we use a non-blocking send: scripts that already emitted a
// terminal Result naturally (and have Stop called after) will see the
// channel-close shortly without an extra event.
func (h *handle) maybeEmitStoppedResult(ctx context.Context) {
	select {
	case <-h.stopped:
	default:
		return
	}
	// Best-effort send; we are about to close the channel via the
	// run() defer. The buffer is sized for the canonical sequence so
	// this rarely blocks; ctx provides the upper bound.
	select {
	case h.events <- agent.ResultEvent{
		Success:      false,
		Message:      "stopped",
		Errors:       []string{"stub: stopped by caller"},
		ErrorSubtype: "stopped",
	}:
	case <-ctx.Done():
	}
}

// closeEvents closes the events channel exactly once. Safe to call
// from multiple defer paths.
func (h *handle) closeEvents() {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.events)
}

// emit pushes an event onto the channel, honoring ctx and Stop.
// Returns false when the session has been aborted; the caller should
// then return immediately.
func (h *handle) emit(ctx context.Context, ev agent.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case <-h.stopped:
		return false
	case h.events <- ev:
		return true
	}
}

// emitInit emits the canonical InitEvent that every behavior begins
// with. Returns false if the session was aborted before the event
// landed on the channel.
func (h *handle) emitInit(ctx context.Context) bool {
	subtype := "starting"
	if h.resumed {
		subtype = "resumed"
	}
	if !h.emit(ctx, agent.InitEvent{SessionID: h.sessionID}) {
		return false
	}
	// SystemEvent is emitted by every behavior except the immediate
	// FailOnClone path; we conditionally include it here so the
	// scripts below stay focused on their distinguishing events.
	if h.behavior == BehaviorFailOnClone {
		return true
	}
	return h.emit(ctx, agent.SystemEvent{Subtype: subtype})
}

// scriptSucceedWithPR emits the canonical successful sequence per
// F.1.1 §3.3. The smoke harness asserts on this byte-exact sequence.
func (h *handle) scriptSucceedWithPR(ctx context.Context) {
	if !h.emit(ctx, agent.AssistantTextEvent{Text: "Stub agent: starting deterministic run."}) {
		return
	}
	if !h.emit(ctx, agent.ToolUseEvent{
		ToolName:  "Bash",
		ToolUseID: "tu-1",
		Input:     map[string]any{"cmd": "git status"},
	}) {
		return
	}
	if !h.emit(ctx, agent.ToolResultEvent{
		ToolName:  "Bash",
		ToolUseID: "tu-1",
		Content:   "clean",
		IsError:   false,
	}) {
		return
	}
	if !h.emit(ctx, agent.AssistantTextEvent{Text: "WORK_RESULT:passed"}) {
		return
	}
	h.emit(ctx, agent.ResultEvent{
		Success: true,
		Message: fmt.Sprintf("Stub run complete (PR: %s)", stubPullRequestURL),
		Cost: &agent.CostData{
			InputTokens:  10,
			OutputTokens: 5,
			TotalCostUsd: 0.001,
			NumTurns:     1,
		},
	})
}

// scriptFailOnClone emits an immediate ErrorEvent and terminates.
func (h *handle) scriptFailOnClone(ctx context.Context) {
	h.emit(ctx, agent.ErrorEvent{
		Message: "stub: worktree clone failed",
		Code:    "clone_failed",
	})
}

// scriptHangThenTimeout blocks on ctx.Done after the lifecycle
// preamble. The runner's MaxDuration timeout is the expected trigger
// for unblocking this script. Stop is also handled — the run() defer
// emits the terminal stopped Result via maybeEmitStoppedResult.
func (h *handle) scriptHangThenTimeout(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-h.stopped:
		return
	}
}

// scriptSlowTool emits a Bash ToolUse, N progress ticks, and a final
// ToolResult + Result. Tick count is taken from
// Spec.ProviderConfig["stub.progressTicks"] (default 3).
func (h *handle) scriptSlowTool(ctx context.Context) {
	ticks := defaultProgressTicks
	if raw, ok := h.spec.ProviderConfig[progressTicksConfigKey]; ok {
		switch v := raw.(type) {
		case int:
			if v >= 0 {
				ticks = v
			}
		case float64:
			if v >= 0 {
				ticks = int(v)
			}
		}
	}
	if !h.emit(ctx, agent.ToolUseEvent{
		ToolName:  "Bash",
		ToolUseID: "tu-slow",
		Input:     map[string]any{"cmd": "sleep 5"},
	}) {
		return
	}
	for i := 1; i <= ticks; i++ {
		if !h.emit(ctx, agent.ToolProgressEvent{
			ToolName:       "Bash",
			ElapsedSeconds: float64(i),
		}) {
			return
		}
	}
	if !h.emit(ctx, agent.ToolResultEvent{
		ToolName:  "Bash",
		ToolUseID: "tu-slow",
		Content:   "done",
	}) {
		return
	}
	h.emit(ctx, agent.ResultEvent{
		Success: true,
		Message: "slow-tool complete",
	})
}

// scriptCostOverrun emits a successful Result with a deliberately
// huge TotalCostUsd to exercise cost-cap warnings.
func (h *handle) scriptCostOverrun(ctx context.Context) {
	if !h.emit(ctx, agent.AssistantTextEvent{Text: "Stub: cost overrun scenario."}) {
		return
	}
	h.emit(ctx, agent.ResultEvent{
		Success: true,
		Message: "completed (cost overrun)",
		Cost: &agent.CostData{
			InputTokens:  1_000_000,
			OutputTokens: 500_000,
			TotalCostUsd: 999.99,
			NumTurns:     42,
		},
	})
}

// scriptMidStreamError emits Init + System (already done by emitInit),
// an assistant message, then crashes with an ErrorEvent.
func (h *handle) scriptMidStreamError(ctx context.Context) {
	if !h.emit(ctx, agent.AssistantTextEvent{Text: "Stub: about to crash."}) {
		return
	}
	h.emit(ctx, agent.ErrorEvent{
		Message: "stub: provider crashed mid-stream",
		Code:    "provider_crash",
	})
}

// scriptInjectTest blocks on the inject channel. Each inject becomes
// an AssistantTextEvent echoing the injected text. The first inject
// also emits the terminal ResultEvent and ends the script. Stop and
// ctx-cancel exit the script; the run() defer takes care of emitting
// the terminal stopped Result.
func (h *handle) scriptInjectTest(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-h.stopped:
		return
	case text := <-h.injects:
		if !h.emit(ctx, agent.AssistantTextEvent{
			Text: "injected: " + text,
		}) {
			return
		}
		h.emit(ctx, agent.ResultEvent{
			Success: true,
			Message: "inject-test complete",
		})
	}
}
