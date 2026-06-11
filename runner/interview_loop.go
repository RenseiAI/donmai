package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/tokendelta"
)

// tokenSink is the seam the interview loop forwards assistant-text token
// deltas through. Implemented by *tokendelta.Poster in production and by a
// recording fake in tests. SetTurn stamps the per-turn correlation id;
// Send enqueues one frame; Stop flushes + shuts down.
//
// Defined as an interface (mirroring activitySink) so the loop's
// control-flow tests never spin up an HTTP server.
type tokenSink interface {
	SetTurn(turnID string)
	Send(f tokenFrame)
	Stop() error
}

// tokenFrame mirrors runtime/tokendelta.Frame so the runner package does
// not import the concrete frame type into its control-flow signatures.
// The adapter in loop.go converts between the two.
type tokenFrame struct {
	Index int
	Text  string
	Done  bool
}

// noopTokenSink is used when interview mode could not construct a real
// token-delta poster (missing PlatformURL / construction error). Frames
// are dropped; the interview still functions (the authoritative transcript
// is the interview_messages row), only the live token render is lost.
type noopTokenSink struct{}

func (noopTokenSink) SetTurn(string)  {}
func (noopTokenSink) Send(tokenFrame) {}
func (noopTokenSink) Stop() error     { return nil }

// interviewClock abstracts the idle-grace timer so tests can drive it
// deterministically. It returns a single reusable interviewTimer (NOT one
// channel per park iteration) so a long interview with many turns does not
// leak a dangling timer per turn. Production wires it to a *time.Timer via
// realInterviewClock; tests substitute a fake.
type interviewClock interface {
	// NewTimer returns a stoppable, resettable timer armed for d. A
	// non-positive d returns a timer whose channel never fires, so callers
	// can treat "no idle-grace cap" uniformly.
	NewTimer(d time.Duration) interviewTimer
}

// interviewTimer is the reusable idle-grace timer the loop arms once and
// resets at the start of every park. Mirrors the *time.Timer surface the
// loop needs (Chan / Reset / Stop).
type interviewTimer interface {
	// Chan returns the fire channel. Nil when the timer is disabled
	// (idle-grace == 0).
	Chan() <-chan time.Time
	// Reset re-arms the timer for d, draining any already-fired value so a
	// stale tick from a prior park cannot trip the next select. A
	// non-positive d disarms the timer.
	Reset(d time.Duration)
	// Stop halts the timer (idempotent; safe on a disabled timer).
	Stop()
}

type realInterviewClock struct{}

func (realInterviewClock) NewTimer(d time.Duration) interviewTimer {
	if d <= 0 {
		return realInterviewTimer{} // disabled — Chan() returns nil
	}
	return realInterviewTimer{t: time.NewTimer(d)}
}

type realInterviewTimer struct {
	t *time.Timer
}

func (rt realInterviewTimer) Chan() <-chan time.Time {
	if rt.t == nil {
		return nil
	}
	return rt.t.C
}

func (rt realInterviewTimer) Reset(d time.Duration) {
	if rt.t == nil {
		return
	}
	if !rt.t.Stop() {
		// Drain a fired-but-unconsumed tick so the next park starts clean.
		select {
		case <-rt.t.C:
		default:
		}
	}
	if d > 0 {
		rt.t.Reset(d)
	}
}

func (rt realInterviewTimer) Stop() {
	if rt.t != nil {
		rt.t.Stop()
	}
}

// interviewLoopConfig carries the per-Run knobs the interview loop needs
// beyond the shared collaborators on *Runner. Pulled into a struct so the
// loop signature stays readable and tests can construct it directly.
type interviewLoopConfig struct {
	handle      agent.Handle
	worktree    string
	qw          QueuedWork
	res         *Result
	sink        activitySink // platform activity buffer (shared with headless)
	tokens      tokenSink    // batched token-delta forwarder
	injectCh    <-chan heartbeat.InjectPayload
	clock       interviewClock // idle-grace timer source
	idleGrace   time.Duration  // 0 = no idle-grace cap
	maxWallSecs int            // 0 = no wall-clock cap (enforced by ctx upstream)
}

// tokenPosterAdapter bridges *tokendelta.Poster to the loop's tokenSink
// interface, converting between runner.tokenFrame and tokendelta.Frame.
type tokenPosterAdapter struct {
	p *tokendelta.Poster
}

func (a tokenPosterAdapter) SetTurn(turnID string) { a.p.SetTurn(turnID) }
func (a tokenPosterAdapter) Send(f tokenFrame) {
	a.p.Send(tokendelta.Frame{Index: f.Index, Text: f.Text, Done: f.Done})
}
func (a tokenPosterAdapter) Stop() error { return a.p.Stop() }

// dispatchInterview is the loop.go-side entry point for interview mode. It
// derives the wall-clock-capped context from InterviewBudget, constructs +
// starts the batched token-delta poster, and hands control to
// runInterviewLoop. Called from runLoop after the shared spawn / heartbeat /
// activity setup; returns the terminal Result directly (the headless
// steering/backstop/post-session tail is skipped for interviews).
func (r *Runner) dispatchInterview(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	sink activitySink,
	injectCh <-chan heartbeat.InjectPayload,
) (*Result, error) {
	// Interview budget: wall-clock cap (ctx deadline) + idle-grace
	// (park-loop timer). Distinct from the finite-run StageBudget — an
	// interview has no token / sub-agent caps, only the two time bounds.
	var (
		idleGrace time.Duration
		wallSecs  int
	)
	if b := qw.InterviewBudget; b != nil {
		idleGrace = time.Duration(b.IdleGraceSeconds) * time.Second
		wallSecs = b.MaxWallClockSeconds
	}

	interviewCtx := ctx
	if wallSecs > 0 {
		var cancel context.CancelFunc
		interviewCtx, cancel = context.WithTimeout(ctx, time.Duration(wallSecs)*time.Second)
		defer cancel()
	}

	// Construct + start the batched token-delta poster. Failure to build it
	// (missing PlatformURL) degrades to a no-op sink — the interview still
	// works, only the live token render is lost.
	var tokens tokenSink = noopTokenSink{}
	if qw.PlatformURL != "" {
		var tdCredProvider tokendelta.CredentialProvider
		if r.credentialProvider != nil {
			tdCredProvider = func(ctx context.Context) (tokendelta.RuntimeCredentials, error) {
				creds, err := r.credentialProvider(ctx)
				return tokendelta.RuntimeCredentials{
					WorkerID:  creds.WorkerID,
					AuthToken: creds.AuthToken,
				}, err
			}
		}
		tdPoster, tdErr := tokendelta.New(tokendelta.Config{
			SessionID:          qw.SessionID,
			WorkerID:           qw.WorkerID,
			BaseURL:            qw.PlatformURL,
			AuthToken:          qw.AuthToken,
			CredentialProvider: tdCredProvider,
			HTTPClient:         r.httpClient,
			Logger:             r.logger,
		})
		if tdErr != nil {
			r.logger.Warn("token-delta poster construct failed; live render disabled",
				"sessionId", qw.SessionID, "err", tdErr)
		} else {
			_ = tdPoster.Start(interviewCtx)
			tokens = tokenPosterAdapter{p: tdPoster}
			defer func() { _ = tdPoster.Stop() }()
		}
	}

	return r.runInterviewLoop(interviewCtx, interviewLoopConfig{
		handle:      handle,
		worktree:    worktreePath,
		qw:          qw,
		res:         res,
		sink:        sink,
		tokens:      tokens,
		injectCh:    injectCh,
		clock:       realInterviewClock{},
		idleGrace:   idleGrace,
		maxWallSecs: wallSecs,
	})
}

// runInterviewLoop drives the non-terminating park-and-inject interview
// loop. It REPLACES the one-shot consumeEvents → drainMemoryInjects
// → steering → backstop → runPostSession path used by headless runs; the
// shared setup (spawn, env, worktree, kit, state.json, heartbeat, activity)
// has already run in runLoop before this is called.
//
// Control flow:
//
//  1. Stream the agent's current question turn to its terminal ResultEvent
//     (consumeInterviewTurn), forwarding assistant-text as batched token
//     deltas. Watch the assistant text for the completion sentinel.
//  2. PARK on injectCh waiting for the next inject. On:
//     - a user-turn inject (Kind == InjectKindUser): call handle.Inject
//     (claude --resume) with the user's text and go back to step 1 to
//     stream the resume turn.
//     - a memory inject (Kind == "" / memory): deliver it between turns via
//     the shared injectDirective (memory still works in interview mode),
//     stream its resume turn, then keep parking.
//  3. Exit cleanly on: the completion sentinel in assistant text, the
//     idle-grace timeout (no inject within idleGrace), or ctx cancel.
//
// claude is single-in-flight: every handle.Inject call happens on this one
// goroutine, serialised behind the prior turn's terminal event. We never
// inject while a turn is streaming.
//
// function so the turn-taking contract is readable end-to-end.
//
//nolint:gocyclo // the park/inject/exit state machine is intentionally one
func (r *Runner) runInterviewLoop(ctx context.Context, cfg interviewLoopConfig) (*Result, error) {
	res := cfg.res
	res.Status = "" // finalised at the end based on how the loop exited

	clock := cfg.clock
	if clock == nil {
		clock = realInterviewClock{}
	}
	sink := cfg.sink
	if sink == nil {
		sink = noopSink{}
	}
	tokens := cfg.tokens
	if tokens == nil {
		tokens = noopTokenSink{}
	}

	r.logger.Info("[interview] loop start",
		"sessionId", cfg.qw.SessionID,
		"interviewId", cfg.qw.IssueID,
		"idleGraceSec", int(cfg.idleGrace.Seconds()),
	)

	// 1. Stream the agent's FIRST question turn (the spawn prompt drives the
	//    opening question). The terminal turn id is the spawn turn; user
	//    turns carry their own turnId from the inject.
	turnID := "turn-0"
	complete := r.consumeInterviewTurn(ctx, cfg, sink, tokens, turnID)
	if complete {
		return r.finishInterview(res, "completed", "sentinel"), nil
	}
	if ctx.Err() != nil {
		return r.finishInterview(res, "stopped", "ctx-cancel"), ctx.Err()
	}

	// Single reusable idle-grace timer (re-armed each park) — avoids leaking
	// one dangling timer per turn for the life of the interview.
	idleTimer := clock.NewTimer(cfg.idleGrace)
	defer idleTimer.Stop()

	// 2. Park-and-inject loop.
	for {
		// Re-arm the idle timer: idle-grace measures the wait for the NEXT
		// user inject, not the resume-turn streaming time.
		idleTimer.Reset(cfg.idleGrace)

		select {
		case <-ctx.Done():
			r.logger.Info("[interview] ctx cancelled — exiting loop",
				"sessionId", cfg.qw.SessionID)
			return r.finishInterview(res, "stopped", "ctx-cancel"), ctx.Err()

		case <-idleTimer.Chan():
			r.logger.Info("[interview] idle-grace elapsed — exiting loop",
				"sessionId", cfg.qw.SessionID,
				"idleGraceSec", int(cfg.idleGrace.Seconds()))
			return r.finishInterview(res, "completed", "idle-grace"), nil

		case p, ok := <-cfg.injectCh:
			if !ok {
				// Channel closed (heartbeat torn down) — exit cleanly.
				return r.finishInterview(res, "stopped", "inject-closed"), nil
			}
			if strings.TrimSpace(p.Text) == "" {
				// Empty inject — ignore, keep parking.
				continue
			}
			kind := p.Kind
			if kind == "" {
				kind = interview.InjectKindMemory
			}

			// Deliver the inject as a follow-up message (claude --resume).
			// Non-fatal benign failures (ErrUnsupported / not-ready /
			// in-flight) are swallowed by injectDirective; a hard failure
			// stops the loop.
			if err := r.injectDirective(ctx, cfg.handle, p.Text); err != nil {
				r.logger.Warn("[interview] inject delivery failed — exiting loop",
					"sessionId", cfg.qw.SessionID,
					"deliveryId", p.DeliveryID,
					"kind", kind,
					"err", err)
				return r.finishInterview(res, "failed", "inject-failed"), err
			}

			// User turns carry a turnId; memory injects do not. The token
			// deltas for the resume turn are stamped with the user turn id
			// so the browser correlates them to the message it sent.
			nextTurn := p.TurnID
			if nextTurn == "" {
				nextTurn = "turn-mem-" + p.DeliveryID
			}
			r.logger.Info("[interview] inject delivered; streaming resume turn",
				"sessionId", cfg.qw.SessionID,
				"kind", kind,
				"turnId", nextTurn)

			complete := r.consumeInterviewTurn(ctx, cfg, sink, tokens, nextTurn)
			if complete {
				return r.finishInterview(res, "completed", "sentinel"), nil
			}
			if ctx.Err() != nil {
				return r.finishInterview(res, "stopped", "ctx-cancel"), ctx.Err()
			}
			// Loop: park again for the next user turn.
		}
	}
}

// consumeInterviewTurn drains the handle's event channel for ONE turn —
// from now until the turn's terminal ResultEvent (or channel close / ctx
// cancel) — mirroring each event to the activity sink, persisting to
// events.jsonl, and forwarding AssistantTextEvent text as batched token
// deltas stamped with turnID. Returns true when the completion sentinel was
// observed in the assistant text (the caller then exits the loop).
//
// Unlike the headless consumeEvents this does NOT run budget enforcement
// (interview budget is wall-clock + idle-grace, enforced by ctx + the
// park-loop timer) and does NOT scan for WORK_RESULT / PR URLs.
func (r *Runner) consumeInterviewTurn(
	ctx context.Context,
	cfg interviewLoopConfig,
	sink activitySink,
	tokens tokenSink,
	turnID string,
) (sentinelSeen bool) {
	tokens.SetTurn(turnID)

	// frameIndex is monotonic per turn; the browser orders frames by it.
	frameIndex := 0
	// sentinelBuf accumulates a sliding tail of assistant text so the
	// completion sentinel is detected even if it straddles two events.
	var sentinelBuf strings.Builder

	emitFrame := func(text string, done bool) {
		tokens.Send(tokenFrame{Index: frameIndex, Text: text, Done: done})
		frameIndex++
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-cfg.handle.Events():
			if !ok {
				// Channel closed without a terminal Result. The turn ended;
				// emit a terminal (done) frame so the browser closes the
				// stream, then return.
				emitFrame("", true)
				return sentinelContains(sentinelBuf.String())
			}
			r.appendInterviewJSONL(cfg.worktree, ev)
			sink.Send(ctx, ev)

			if at, isText := ev.(agent.AssistantTextEvent); isText && at.Text != "" {
				sentinelBuf.WriteString(at.Text)
				// Forward the assistant text as a (non-terminal) token-delta
				// frame. The tokendelta poster batches these per the
				// 100ms-or-20-frames contract — the runner does not batch
				// here; one AssistantTextEvent maps to one frame.
				emitFrame(at.Text, false)
			}

			if _, terminal := ev.(agent.ResultEvent); terminal {
				// Turn complete. Emit the terminal frame so the browser
				// closes the message stream for this turn.
				emitFrame("", true)
				return sentinelContains(sentinelBuf.String())
			}
		}
	}
}

// appendInterviewJSONL persists one event to <worktree>/.agent/events.jsonl,
// matching the headless audit path. Best-effort — open/marshal failures are
// logged at debug and dropped (the platform activity buffer is the primary
// record for interviews).
func (r *Runner) appendInterviewJSONL(worktreePath string, ev agent.Event) {
	jsonlPath := filepath.Join(worktreePath, state.AgentDirName, "events.jsonl")
	body, err := agent.MarshalEvent(ev)
	if err != nil {
		return
	}
	r.appendJSONLLine(jsonlPath, body)
}

// finishInterview stamps the terminal status + a structured exit reason and
// returns the result. Centralised so every exit path is consistent.
func (r *Runner) finishInterview(res *Result, status, reason string) *Result {
	res.Status = status
	r.logger.Info("[interview] loop exit",
		"sessionId", res.SessionID,
		"status", status,
		"reason", reason)
	return res
}

// sentinelContains reports whether the accumulated assistant text contains
// the interview completion sentinel.
func sentinelContains(text string) bool {
	return strings.Contains(text, interview.InterviewCompleteSentinel)
}

// appendJSONLLine is a tiny mutex-guarded append helper shared by the
// interview audit path. The headless path opens the file once per turn; the
// interview path appends per-event, so we serialise opens here. Best-effort.
func (r *Runner) appendJSONLLine(path string, body []byte) {
	interviewJSONLMu.Lock()
	defer interviewJSONLMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		r.logger.Debug("interview events.jsonl mkdir failed", "err", err)
		return
	}
	//nolint:gosec // G304: path is owned by the runner via worktree manager.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.logger.Debug("interview events.jsonl open failed", "err", err)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(body, '\n'))
}

// interviewJSONLMu serialises per-event appends to events.jsonl across the
// interview loop's turns (the loop runs on a single goroutine, but the
// mutex keeps the append atomic against any future concurrent writer).
var interviewJSONLMu sync.Mutex
