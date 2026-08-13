package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// ─── Lane-B handle (07 §2, §6, §9) ───────────────────────────────────────────
//
// serverHandle is the agent.Handle backed by an `opencode serve` child (or an
// attached external server). It:
//   - subscribes to the SSE /api/event feed and maps frames to agent.Events
//     (events_sse.go), enforcing the terminal contract;
//   - runs the permission pump (permission.go) on a ticker, adjudicating pending
//     requests and surfacing permission_request / permission_decision
//     SystemEvents (§5.2);
//   - Inject posts a follow-up prompt (SupportsMessageInjection);
//   - Stop aborts the turn, stops the SSE subscription, and tears down the child.
//
// Terminal handling distinguishes an ORDINARY completed/failed turn (a
// ResultEvent mapped while the SSE stream is still open — Map() never emits
// an ErrorEvent while the stream is alive) from a FATAL one (the stream
// itself ending: a crashed child or a dropped feed, mapped to ErrorEvent).
// An ordinary terminal does NOT release the owned child/config or stop the
// SSE subscription — forward keeps pumping so a later Handle.Inject still
// has a live server and session to land the follow-up prompt on; that
// cleanup happens lazily, in doStop, once a caller actually calls Stop. A
// FATAL terminal still tears everything down synchronously, same as before.

// permPollInterval is how often the pump lists pending permissions while the
// session is active. The v2 feed does not (in the pinned binary) carry a
// permission SSE frame the adapter can key on, so a short poll is the reliable
// trigger; each request is replied to at most once (permPump.done). The handle
// copies it into a per-instance field so tests can shorten it without racing a
// shared var.
const permPollInterval = 250 * time.Millisecond

type serverHandle struct {
	child   *serveChild // nil in attach mode
	client  serverClient
	mapper  *sseMapper
	pump    *permPump
	logger  *slog.Logger
	sessID  atomic.Pointer[string]
	spec    agent.Spec
	stopSSE func() error

	// releaseOwned stops the provider-owned serve child and then destroys its
	// isolated config. nil in external-attach mode.
	releaseOwned func() error
	releaseOnce  sync.Once
	releaseErr   error

	// permInterval is the pump poll cadence (defaults to permPollInterval;
	// tests shorten it per-instance to avoid racing a shared var).
	permInterval time.Duration

	events chan agent.Event

	stopOnce sync.Once
	stopErr  error
	stopping atomic.Bool
	// lifecycleMu orders permission observability batches before terminal
	// cleanup so a terminal cannot split an already-adjudicated batch.
	lifecycleMu sync.Mutex

	cleanupReported atomic.Bool

	// injectClosed signals "no live session left for Inject to land on".
	// Closed BEFORE a fatal terminal event becomes observable on Events()
	// (see finishWithCleanup) and at the top of doStop — never after. A
	// channel SEND only happens-before the matching RECEIVE completes;
	// nothing orders what the sender does NEXT against what the receiver
	// does next, so a caller that reacts to a fatal event (or to Stop's own
	// side effects) the instant it observes it on Events() must already find
	// injectClosed closed, not race it. An ordinary completed/failed turn
	// does NOT close this — Inject must keep working past it.
	injectClosed     chan struct{}
	injectClosedOnce sync.Once

	shutdown chan struct{}
	done     chan struct{} // closed when the forwarder exits

	eventsClosed atomic.Bool
	eventsMu     sync.RWMutex
}

func newServerHandle(child *serveChild, client serverClient, sessionID string, spec agent.Spec, logger *slog.Logger) *serverHandle {
	h := &serverHandle{
		child:        child,
		client:       client,
		mapper:       newSSEMapper(sessionID),
		pump:         newPermPump(client, newPermEngine(spec), sessionID),
		logger:       logger,
		spec:         spec,
		events:       make(chan agent.Event, eventBufferSize),
		injectClosed: make(chan struct{}),
		shutdown:     make(chan struct{}),
		done:         make(chan struct{}),
		permInterval: permPollInterval,
	}
	id := sessionID
	h.sessID.Store(&id)
	return h
}

// start subscribes to events and launches the forwarder + permission pump.
func (h *serverHandle) start(ctx context.Context) error {
	ch, stop, err := h.client.Events(ctx)
	if err != nil {
		return fmt.Errorf("%w: subscribe opencode events: %v", agent.ErrSpawnFailed, err)
	}
	h.stopSSE = stop
	go h.forward(ch)   //nolint:gosec // G118: session-lifetime goroutine; honors h.shutdown
	go h.pumpLoop()    //nolint:gosec // G118: session-lifetime goroutine; honors h.shutdown
	go h.watchCtx(ctx) //nolint:gosec // G118: honors ctx + h.shutdown
	return nil
}

func (h *serverHandle) SessionID() string {
	if v := h.sessID.Load(); v != nil {
		return *v
	}
	return ""
}

func (h *serverHandle) Events() <-chan agent.Event { return h.events }

// Inject posts a follow-up prompt onto the live session (opencode queues it via
// its own admission model). Available because Lane B declares
// SupportsMessageInjection.
//
// Gated on injectClosed, not done/shutdown: an ordinary completed/failed turn
// (a soft terminal — see forward) closes neither of those, precisely so a
// post-settle Inject like this one still has a live server and session to
// post to. Only a fatal terminal or an explicit Stop closes injectClosed.
func (h *serverHandle) Inject(ctx context.Context, text string) error {
	select {
	case <-h.injectClosed:
		return fmt.Errorf("provider/opencode: Inject after session end: %w", agent.ErrUnsupported)
	default:
	}
	return h.client.Prompt(ctx, h.SessionID(), promptReq{Prompt: promptInput{Text: text}, Delivery: "steer"})
}

// Stop aborts the session, stops the SSE subscription, tears down the child,
// and closes the events channel. Idempotent.
func (h *serverHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() { h.stopErr = h.doStop(ctx) })
	return h.stopErr
}

func (h *serverHandle) doStop(ctx context.Context) error {
	// First thing, unconditionally: Stop always closes injectClosed,
	// whichever path (this one or a fatal finishWithCleanup) gets there
	// first — see the field's doc comment for the ordering argument.
	h.signalInjectClosed()

	h.lifecycleMu.Lock()
	h.stopping.Store(true)
	h.lifecycleMu.Unlock()

	// (1) best-effort abort so opencode finalizes its session file.
	abortCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_ = h.client.Abort(abortCtx, h.SessionID())
	cancel()

	// (2) stop the SSE subscription (unblocks the forwarder).
	if h.stopSSE != nil {
		_ = h.stopSSE()
	}

	// (3) tear down the serve child, then destroy its config. Cleanup errors
	// are both returned and emitted before the event channel closes.
	cleanupErr := h.releaseOwnedResource()
	if cleanupErr != nil {
		h.reportCleanupFailure()
	}
	close(h.shutdown)

	// (4) wait for the forwarder to drain, then close events.
	select {
	case <-h.done:
	case <-time.After(stopGracePeriod):
	}
	h.closeEvents()
	return cleanupErr
}

func (h *serverHandle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second)
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
	}
}

// forward consumes SSE frames, maps them, and enforces the terminal contract.
//
// A mapped terminal reached while the stream is still open (ok == true) is
// always ORDINARY: Map() only reaches m.terminal=true from a completed or
// failed TURN (ResultEvent) — it never emits an ErrorEvent while the stream
// is alive. That case falls through to the plain emitAll below and the loop
// keeps going: the owned child/config stay up and the SSE subscription stays
// open, so a later Handle.Inject still has a live session to post the
// follow-up prompt to. Only a genuinely FATAL end — the stream itself
// closing (crash, dropped feed, or the child exiting once idle after an
// earlier ordinary terminal) — calls finishWithCleanup and returns.
func (h *serverHandle) forward(ch <-chan serverEvent) {
	defer close(h.done)
	for {
		select {
		case <-h.shutdown:
			return
		case ev, ok := <-ch:
			if !ok {
				if h.stopping.Load() {
					return
				}
				// SSE stream ended. If we already emitted an ordinary
				// terminal, this is the clean end (terminalOnStreamEnd
				// returns nil — see its doc). Otherwise the child crashed or
				// the stream dropped — surface a fatal terminal so the
				// runner is not left hanging.
				h.finishWithCleanup(h.terminalOnStreamEnd())
				return
			}
			if h.stopping.Load() {
				return
			}
			out := h.mapper.Map(ev)
			h.emitAll(out)
		}
	}
}

// finishWithCleanup handles a FATAL end: the SSE stream itself has closed, so
// the session is genuinely over. The owned child/config is released and
// Inject is closed off BEFORE the mapped terminal (if any) is published —
// see injectClosed's doc comment for why that order, not the reverse, is
// required. A cleanup failure replaces the terminal with one bounded
// ErrorEvent rather than letting a caller believe secrets were scrubbed when
// they were not.
//
// This is never called for an ORDINARY completed/failed turn observed while
// the stream is open (forward's ok==true branch) — that path keeps the pump,
// child, and config alive and defers cleanup to doStop, once a caller
// actually calls Stop.
func (h *serverHandle) finishWithCleanup(events []agent.Event) {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()
	if !h.stopping.CompareAndSwap(false, true) {
		return
	}
	h.signalInjectClosed()
	cleanupErr := h.releaseOwnedResource()
	for _, ev := range events {
		switch ev.Kind() {
		case agent.EventResult, agent.EventError:
			if cleanupErr != nil {
				h.reportCleanupFailure()
			} else {
				h.emit(ev)
			}
		default:
			h.emit(ev)
		}
	}
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod)
		defer cancel()
		_ = h.Stop(stopCtx)
	}()
}

// signalInjectClosed closes injectClosed exactly once.
func (h *serverHandle) signalInjectClosed() {
	h.injectClosedOnce.Do(func() { close(h.injectClosed) })
}

func (h *serverHandle) releaseOwnedResource() error {
	h.releaseOnce.Do(func() {
		if h.releaseOwned != nil {
			h.releaseErr = h.releaseOwned()
		}
	})
	return h.releaseErr
}

func (h *serverHandle) reportCleanupFailure() {
	if h.cleanupReported.CompareAndSwap(false, true) {
		h.emit(openCodeConfigCleanupEvent())
	}
}

// terminalOnStreamEnd builds the terminal event for an SSE stream that closed
// without one: server_crashed when the child is gone, else a stream-closed
// error.
func (h *serverHandle) terminalOnStreamEnd() []agent.Event {
	if h.mapper.terminal {
		return nil
	}
	if h.child != nil && h.child.exited() {
		return h.mapper.serverCrashed("serve process exited")
	}
	// Attached server or child still alive but the feed dropped.
	h.mapper.terminal = true
	return []agent.Event{agent.ErrorEvent{
		Message: "opencode event stream closed before a terminal result",
		Code:    "event_stream_closed",
	}}
}

// pumpLoop adjudicates pending permissions on a ticker until shutdown or the
// session terminates.
func (h *serverHandle) pumpLoop() {
	interval := h.permInterval
	if interval <= 0 {
		interval = permPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.shutdown:
			return
		case <-h.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			records, err := h.pump.Adjudicate(ctx)
			cancel()
			if err != nil {
				h.logger.Debug("provider/opencode: permission adjudicate", "err", err)
				continue
			}
			h.lifecycleMu.Lock()
			if h.stopping.Load() {
				h.lifecycleMu.Unlock()
				return
			}
			for _, rec := range records {
				h.emit(agent.SystemEvent{
					Subtype: "permission_request",
					Message: fmt.Sprintf("permission requested: action=%s", rec.Request.Action),
					Raw:     rec.Request,
				})
				h.emit(agent.SystemEvent{
					Subtype: "permission_decision",
					Message: fmt.Sprintf("permission %s: %s", rec.Decision.Reply, rec.Decision.Reason),
					Raw:     rec.Request,
				})
			}
			h.lifecycleMu.Unlock()
		}
	}
}

func (h *serverHandle) emitAll(events []agent.Event) {
	for _, ev := range events {
		h.emit(ev)
	}
}

func (h *serverHandle) emit(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
	}
}

func (h *serverHandle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

var _ agent.Handle = (*serverHandle)(nil)
