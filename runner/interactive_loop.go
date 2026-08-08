package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/state"
)

// Relay-attach env var names. Deliberately generic + brand-neutral so any
// relay provisioner (the local daemon, an e2b sandbox metadata endpoint, …)
// injects them identically — the OSS attach client never learns a platform
// hostname. The composing daemon injects BOTH to drive the outbound relay
// attach, or NEITHER for a valid OSS-standalone local-only session; exactly
// one is a deployment misconfiguration the runner fails loud on.
//
// ATTACH_TOKEN_FILE is the optional token-refresh rail: a provisioner that
// maintains a fresh short-exp attach token (re-minted before each expiry) may
// point this at a file it atomically rewrites; the runner then re-reads it
// before each carrier attempt and on degraded-lane refreshes, so a reconnect
// AFTER the initial token's exp still presents a live token. It is meaningful
// only alongside the attach pair — ATTACH_TOKEN remains the initial value and
// the fallback — and a stray ATTACH_TOKEN_FILE with no attach pair is ignored.
const (
	envAttachURL   = "ATTACH_URL"
	envAttachToken = "ATTACH_TOKEN"
	// #nosec G101 -- an env var NAME (the runner reads a path from it), not a credential.
	envAttachTokenFile = "ATTACH_TOKEN_FILE"

	// maxInitialPromptBytes leaves one byte for the appended newline so the
	// complete first PTY input stays within the conservative 1,024-byte
	// canonical-mode boundary shared by the supported host environments.
	maxInitialPromptBytes = 1023

	// maxAttachTokenFileBytes is deliberately generous for a compact host JWT
	// while bounding a provisioner-controlled file read to a small allocation.
	maxAttachTokenFileBytes = 16 << 10
)

var (
	errAttachTokenFileOversized = errors.New("attach token file exceeds the JWT size limit")
	errAttachTokenFileEmpty     = errors.New("attach token file is empty")
	errAttachTokenFileMalformed = errors.New("attach token file does not contain a syntactically valid compact JWT")
)

// interactiveSessionClass returns the sessionClass discriminator the runner
// stamps on the heartbeat lock-refresh body: "interactive" for the
// PTY-hosted interactive dispatch, "" for every other mode (the heartbeat
// wire's omitempty tag then keeps the body byte-identical for existing
// headless / interview sessions). Named so the constant is written once and
// the loop.go heartbeat wiring reads it declaratively (W4 amendment 4).
//
// Returns heartbeat.SessionClassInteractive (== interactiveRunMode) rather
// than the local run-mode constant: the pulser keys its NON-FATAL loss
// posture (degrade + keep beating instead of LostOwnership on heartbeat
// loss / refreshed=false) off exact equality with that constant, so the
// stamp and the gate must never drift apart.
func interactiveSessionClass(qw QueuedWork) string {
	if qw.isInteractive() {
		return heartbeat.SessionClassInteractive
	}
	return ""
}

// sessAdapter bridges an agent.InteractiveSession to the structurally-
// identical attachclient.Session. The two surfaces match method-for-method
// EXCEPT Subscribe's return type (attachclient.Subscription vs
// agent.InteractiveSubscription), and Go interface satisfaction is nominal
// for method return types — so the host leg needs this ~5-line shim
// (documented verbatim in attachclient/session.go). The returned
// agent.InteractiveSubscription already satisfies attachclient.Subscription
// structurally, so no per-call wrapping is required.
type sessAdapter struct{ agent.InteractiveSession }

func (a sessAdapter) Subscribe(from attachwire.HostSeq) (attachclient.Subscription, error) {
	return a.InteractiveSession.Subscribe(from)
}

// dispatchInteractive drives a mode:"interactive" session: it attaches the
// spawned PTY surface's live byte stream OUTBOUND to the relay and
// supervises the session until the child exits, ctx cancel / wall-clock
// cap, or operator stop. It is the loop.go-side sibling of dispatchInterview
// and is called after the shared spawn / heartbeat (with the sessionClass
// stamp) / activity setup; it returns the terminal Result directly. The
// headless steering / backstop / post-session tail is SKIPPED — an
// interactive session produces no PR and drives no issue-tracker state
// transition (its lifecycle belongs to the human at the terminal).
//
// Budget: interactive sessions honor a WALL-CLOCK cap only. Human think-time
// between keystrokes must NEVER be read as an idle stall — that is the whole
// point of the sessionClass reaper exemption — so the interview loop's
// idle-grace timer is deliberately NOT reused. The wall bound rides on
// InterviewBudget.MaxWallClockSeconds (the same "time-bound, no token /
// sub-agent caps" budget shape); zero/absent means no runner-side wall cap
// (the enclosing ctx, which already carries Options.MaxSessionDuration,
// remains the backstop).
//
// injectCh is the runtime-inject rail (the same channel the interview loop
// parks on). An interactive session CONSUMES it — the defect this closes is
// that it used to receive payloads it never read, report them accepted, and
// ack them. Passing nil is valid and simply disables the rail (a nil channel
// never becomes ready).
//
// noticeDelivery is the selected harness's DECLARED notice-delivery mechanism
// (agent.HarnessCaps.NoticeDelivery), read from the live manifest — never
// inferred from the harness's name. It decides both WHETHER a payload may be
// delivered and BY WHICH SHAPE:
//
//   - agent.NoticeDeliveryPTYNotice writes into this PTY, because only there is
//     the terminal the agent's actual input surface.
//   - agent.NoticeDeliveryHook is collected from the harness's own channel
//     (agent.NoticeChannel on the handle) and never touches the terminal.
//   - everything else is refused, reported to the producer, and left for a
//     channel that can carry it.
//
// See interactive_inject.go's noticeChannelDrivenByRunner / noticeChannelIsPull.
func (r *Runner) dispatchInteractive(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	sink activitySink,
	pulser interactivePulser,
	injectCh <-chan heartbeat.InjectPayload,
	noticeDelivery agent.NoticeDelivery,
) (*Result, error) {
	if sink == nil {
		sink = noopSink{}
	}
	if promptBytes := len(qw.InitialPrompt); qw.isInteractive() && promptBytes > maxInitialPromptBytes {
		err := fmt.Errorf(
			"interactive initial prompt is %d UTF-8 bytes; limit is %d bytes",
			promptBytes,
			maxInitialPromptBytes,
		)
		res.Status = "failed"
		res.FailureMode = FailureInteractiveInput
		res.Error = err.Error()
		return res, err
	}

	// The spawned handle MUST expose a live PTY surface. A harness without
	// PTY transport ignores Spec.Interactive and returns an ordinary handle;
	// classify that as a config/capability failure (route to a PTY-capable
	// harness), NOT a crash.
	capable, ok := handle.(agent.InteractiveCapable)
	var isess agent.InteractiveSession
	if ok {
		isess = capable.InteractiveSession()
	}
	if isess == nil {
		res.Status = "failed"
		res.FailureMode = FailureInteractiveUnsupported
		res.Error = fmt.Sprintf(
			"interactive mode: provider %q spawned a non-interactive handle — harness does not declare PTY transport, so Spec.Interactive was ignored (no live PTY surface to attach)",
			res.ProviderName,
		)
		r.logger.Error("[interactive] handle is not InteractiveCapable",
			"sessionId", qw.SessionID, "provider", res.ProviderName)
		return res, errors.New(res.Error)
	}

	// Wall-clock cap (idle-grace deliberately absent — see the doc comment).
	interactiveCtx := ctx
	if wall := wallCapSeconds(qw); wall > 0 {
		var cancel context.CancelFunc
		interactiveCtx, cancel = context.WithTimeout(ctx, time.Duration(wall)*time.Second)
		defer cancel()
	}

	// Env attach contract: both → relay attach; neither → local-only; exactly
	// one → fail loud (a half-configured attach is a misconfiguration).
	attachURL := strings.TrimSpace(os.Getenv(envAttachURL))
	attachToken := strings.TrimSpace(os.Getenv(envAttachToken))
	switch {
	case attachURL != "" && attachToken != "":
		// Relay attach — wired below.
	case attachURL == "" && attachToken == "":
		// Valid OSS-standalone local-only session: no outbound attach. The
		// PTY still runs (a local dashboard could attach via ptyhost's own
		// local API); the runner just supervises the lifecycle.
		r.logger.Info("[interactive] no ATTACH_URL/ATTACH_TOKEN — running local-only interactive session",
			"sessionId", qw.SessionID)
	default:
		res.Status = "failed"
		res.FailureMode = FailureInteractiveConfig
		res.Error = fmt.Sprintf(
			"interactive mode: half-configured relay attach — exactly one of %s/%s is set (both or neither required)",
			envAttachURL, envAttachToken,
		)
		r.logger.Error("[interactive] half-configured attach env",
			"sessionId", qw.SessionID, "hasURL", attachURL != "", "hasToken", attachToken != "")
		return res, errors.New(res.Error)
	}

	r.logger.Info("[interactive] session start",
		"sessionId", qw.SessionID, "attach", attachURL != "", "wallCapSec", wallCapSeconds(qw))
	r.postInteractiveActivity(interactiveCtx, worktreePath, sink, "interactive-session-started",
		"interactive PTY session started")

	// Operator-stop signal. Because this session's heartbeat carries
	// sessionClass=interactive, the pulser NEVER closes LostOwnership on
	// heartbeat loss or a refused refresh — it degrades and keeps beating
	// (see heartbeat.SessionClassInteractive). In practice this channel
	// therefore fires only for the platform's explicit deterministic cancel
	// ({"stop": true}); the FailureLostOwnership classification below is
	// retained as defense in depth. Prompt delivery already completed inside
	// Provider.Spawn, so this channel governs only the live attached session.
	var lost <-chan struct{}
	if pulser != nil {
		lost = pulser.LostOwnership()
	}

	// Provider.Spawn has already compiled and delivered any InitialPrompt from
	// the typed prompt plan onto the profile's native first-turn surface. This
	// branch records that successful precondition but intentionally writes no
	// bytes: replaying QueuedWork.InitialPrompt here would bypass the receipt
	// and duplicate Claude/Codex positional seeds and shell's PTY seed.
	if qw.isInteractive() && qw.InitialPrompt != "" {
		r.postInteractiveActivity(interactiveCtx, worktreePath, sink, "interactive-initial-prompt-delivered",
			"interactive initial prompt delivered by native harness surface")
	}

	// Start the outbound relay attach when configured. RunHost dials OUT only
	// (no inbound listener) and blocks until the session ends or a terminal
	// relay condition; run it in a goroutine and treat any terminal return as
	// attach-LOSS, never as session death — the PTY is the product, so attach
	// loss degrades observation but the local session keeps running (unless
	// the relay killed us via the Kill hook, which arrives as Done anyway).
	var attachDone chan error
	if attachURL != "" {
		attachCtx, attachCancel := context.WithCancel(interactiveCtx)
		defer attachCancel()
		attachDone = make(chan error, 1)
		hostCfg := attachclient.HostConfig{
			AttachURL: attachURL,
			// Token re-mint rail: the provisioner may maintain a fresh
			// short-exp token at ATTACH_TOKEN_FILE (atomically rewritten
			// before each expiry); the source re-reads it before carrier
			// attempts and degraded-lane refreshes so a reconnect after the
			// initial token's exp presents a live token.
			// The static ATTACH_TOKEN is the initial value and the fallback —
			// without the file (or on any file failure) its exp bounds
			// reconnectability exactly as before.
			TokenSource: attachTokenSource(attachToken,
				strings.TrimSpace(os.Getenv(envAttachTokenFile)), r.logger),
			Session: sessAdapter{isess},
			// Kill hook = handle.Stop so a relay kill drives the normal
			// drain→Exit flow (SIGTERM→grace→SIGKILL). Idempotent.
			Kill: func(kctx context.Context, _, _ string) error {
				return handle.Stop(kctx)
			},
			Logger: r.logger,
		}
		go func() { attachDone <- attachclient.RunHost(attachCtx, hostCfg) }()
	}

	// Runtime-inject consumer. One held notice at a time (see
	// interactiveNoticeQueue): while a notice is pending the supervisor stops
	// reading injectCh, which preserves order and pushes back on the producer
	// instead of accepting messages it cannot deliver.
	//
	// The retry timer is created armed (the clock contract has no "disabled"
	// constructor that can later be re-armed) and immediately stopped; it is
	// reset only while a notice is held, so an idle session never wakes up.
	//
	// The PULL channel, when the harness exposes one, comes off the HANDLE and
	// not off the PTY surface: a channel like Claude Code's Stop hook belongs
	// to the spawned session as a whole (its drop, its hook script, its
	// transcript), and delivery over it never touches the terminal. A harness
	// that declares a pull mechanism but exposes no channel yields nil here,
	// which the queue reports per message instead of silently downgrading.
	var noticeChannel agent.NoticeChannel
	if nc, ok := handle.(agent.NoticeChannelCapable); ok {
		noticeChannel = nc.NoticeChannel()
	}
	notices := &interactiveNoticeQueue{channel: noticeDelivery, sink: pulser}
	retry := r.noticeRetryClock().NewTimer(interactiveNoticeRetry)
	retry.Stop()
	defer retry.Stop()
	defer func() {
		// Held/buffered at session end is NOT dead-lettered: the payload is
		// still unacked, so the producer re-offers it to a session that can
		// take it. Only a shortfall is reported, never a loss.
		if held := notices.undelivered(len(injectCh)); held > 0 {
			r.logger.Warn("[interactive] session ended with buffered notices undelivered — left unacked for requeue",
				"sessionId", qw.SessionID, "count", held)
		}
	}()

	for {
		// A nil source parks the inject case while a notice is held.
		var noticeSrc <-chan heartbeat.InjectPayload
		if notices.idle() {
			noticeSrc = injectCh
		}

		select {
		case p, ok := <-noticeSrc:
			if !ok {
				// The heartbeat tore the channel down — stop listening, keep
				// supervising the PTY (the human owns this session's life).
				injectCh = nil
				continue
			}
			// Blank payloads are dropped BEFORE the slot is taken, and the
			// emptiness test is on the producer's raw text rather than on any
			// one transport's rendering: what is worth delivering cannot
			// depend on which harness happens to be running.
			if strings.TrimSpace(p.Text) == "" {
				r.logger.Debug("[interactive] dropping empty inject payload",
					"sessionId", qw.SessionID, "deliveryId", p.DeliveryID)
				continue
			}
			notices.hold(p.Text, p.DeliveryID)
			if notices.attempt(r, qw, res, isess, noticeChannel) == noticeHeld {
				retry.Reset(interactiveNoticeRetry)
			}
			continue

		case <-retry.Chan():
			if notices.idle() {
				continue
			}
			if notices.attempt(r, qw, res, isess, noticeChannel) == noticeHeld {
				retry.Reset(interactiveNoticeRetry)
			}
			continue

		case <-isess.Done():
			// Child exited and the PTY drained to EOF (Exit emitted).
			return r.finishInteractive(worktreePath, qw, res, sink, isess), nil

		case <-interactiveCtx.Done():
			// Runner stop / cancel / wall-clock cap. The deferred handle.Stop
			// in runLoop tears down the PTY; a cancelled interactive session
			// is "stopped", not a failure (mirrors the interview ctx-cancel
			// exit). Terminal activity posts on a background ctx so a
			// cancelled interactiveCtx does not drop the session-ended signal.
			res.Status = "stopped"
			if res.Error == "" {
				res.Error = interactiveStopReason(interactiveCtx, ctx)
			}
			r.postInteractiveActivity(context.Background(), worktreePath, sink, "interactive-session-ended",
				"interactive session stopped: "+res.Error)
			r.logger.Info("[interactive] ctx done — stopping session",
				"sessionId", qw.SessionID, "reason", res.Error)
			return res, interactiveCtx.Err()

		case <-lost:
			return r.finishInteractiveOwnershipLoss(worktreePath, qw, res, sink, pulser)

		case err := <-attachDone:
			// The attach leg terminated (epoch-stale, a non-retryable relay
			// stop, or a clean end). This must NOT kill the local session —
			// the PTY is the product. Record a session-level warning and
			// CONTINUE supervising; nil the channel so we don't spin on it.
			attachDone = nil
			r.recordAttachLoss(qw, res, err)
		}
	}
}

// finishInteractiveOwnershipLoss classifies both ownership-loss observation
// points: the steady-state supervisor and a blocked initial-prompt write.
func (r *Runner) finishInteractiveOwnershipLoss(
	worktreePath string,
	qw QueuedWork,
	res *Result,
	sink activitySink,
	pulser interactivePulser,
) (*Result, error) {
	// Operator cancel ({"stop":true}) is terminal and non-retryable; a
	// heartbeat fuse or hand-off retains the generic lost-ownership mode.
	res.Status = "failed"
	if pulser != nil && pulser.StopRequested() {
		res.FailureMode = FailureOperatorCancelled
		res.Error = "operator cancelled interactive session (lock-refresh stop=true)"
	} else {
		res.FailureMode = FailureLostOwnership
		res.Error = heartbeat.ErrLostOwnership.Error()
	}
	r.postInteractiveActivity(context.Background(), worktreePath, sink, "interactive-session-ended",
		"interactive session ownership lost: "+res.FailureMode)
	r.logger.Info("[interactive] ownership lost — stopping session",
		"sessionId", qw.SessionID, "failureMode", res.FailureMode)
	return res, heartbeat.ErrLostOwnership
}

// finishInteractive builds the terminal Result from the PTY child's Exit
// payload (spec § 12.2): a clean exit 0 → completed; a nonzero exit or
// signal death → failed, carrying the exit detail. Follows the interview
// loop's finish convention (status + a short structured reason).
func (r *Runner) finishInteractive(
	worktreePath string,
	qw QueuedWork,
	res *Result,
	sink activitySink,
	isess agent.InteractiveSession,
) *Result {
	exit, ok := isess.Exit()
	var detail string
	switch {
	case !ok:
		// Done closed but Exit not reported — should not happen (ptyhost
		// closes Done strictly after Exit is set). Treat defensively.
		detail = "no exit payload"
	case exit.BySignal():
		detail = fmt.Sprintf("signal %s (exit %d)", exit.Signal, exit.ExitCode)
	default:
		detail = fmt.Sprintf("exit %d", exit.ExitCode)
	}

	if ok && !exit.BySignal() && exit.ExitCode == 0 {
		res.Status = "completed"
	} else {
		res.Status = "failed"
		if res.Error == "" {
			res.Error = "interactive session ended: " + detail
		}
	}
	res.Summary = "interactive session ended (" + detail + ")"
	r.postInteractiveActivity(context.Background(), worktreePath, sink, "interactive-session-ended",
		"interactive session ended: "+detail)
	r.logger.Info("[interactive] session end",
		"sessionId", qw.SessionID, "status", res.Status, "exit", detail)
	return res
}

// recordAttachLoss surfaces a terminal attach-leg return as a session-level
// warning WITHOUT terminating the local session (attach loss must not kill
// the work). A clean end or our own ctx teardown is not a loss.
func (r *Runner) recordAttachLoss(qw QueuedWork, res *Result, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		r.logger.Debug("[interactive] attach leg ended cleanly", "sessionId", qw.SessionID)
		return
	}
	var warn string
	switch {
	case errors.Is(err, attachclient.ErrEpochStale):
		warn = "interactive attach: epoch-stale — a newer host process owns the room; local session continues"
	default:
		// Includes *attachclient.RelayStopError and any other terminal cause.
		warn = fmt.Sprintf("interactive attach lost (local session continues): %v", err)
	}
	res.PostSessionWarnings = append(res.PostSessionWarnings, warn)
	r.logger.Warn("[interactive] attach lost", "sessionId", qw.SessionID, "err", err)
}

// postInteractiveActivity emits ONE coarse, low-cadence lifecycle signal:
// session-started / session-ended and marker-worthy transitions. It is
// deliberately NOT per-byte or per-frame — the live terminal bytes ride the
// attach stream to the relay, never the activity buffer. The event is pushed
// to the platform activity buffer (best-effort, non-blocking) AND mirrored
// to the session's events.jsonl for audit parity with the headless /
// interview paths. Both legs are best-effort.
func (r *Runner) postInteractiveActivity(ctx context.Context, worktreePath string, sink activitySink, subtype, message string) {
	ev := agent.SystemEvent{Subtype: subtype, Message: message}
	if sink != nil {
		sink.Send(ctx, ev)
	}
	if body, err := agent.MarshalEvent(ev); err == nil {
		r.appendJSONLLine(filepath.Join(worktreePath, state.AgentDirName, "events.jsonl"), body)
	}
}

// attachTokenSource builds the host leg's attachclient.TokenSource. RunHost
// resolves it before each top-level carrier attempt; degraded-lane 401 recovery
// and the WSS upgrade probe may additionally call it concurrently. That is the
// seam the token-refresh rail rides:
//
//   - tokenFilePath == "": today's behavior, unchanged — the static token is
//     re-presented on every attempt and its exp bounds the session's
//     reconnectability.
//   - tokenFilePath != "": the file is re-read on every resolution (the
//     provisioner rewrites it atomically with a fresh short-exp token), so a
//     reconnect after the static token's exp still presents a live token.
//     Missing/unreadable files retain the static-token fallback for startup and
//     refresh races. A present file must be bounded, non-empty, and a syntactic
//     compact JWT; invalid content fails the attempt rather than silently
//     presenting a stale bearer.
func attachTokenSource(staticToken, tokenFilePath string, logger *slog.Logger) attachclient.TokenSource {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if tokenFilePath == "" {
		return func(context.Context) (string, error) { return staticToken, nil }
	}
	// TokenSource is a concurrent-use contract: the degraded carrier may re-mint
	// from POST-up, SSE, and upgrade-probe paths at the same time. Protect the
	// warning-state transition while leaving the bounded file reads concurrent.
	var warnMu sync.Mutex
	lastWarn := ""
	warningChanged := func(warn string) bool {
		warnMu.Lock()
		defer warnMu.Unlock()
		if warn == lastWarn {
			return false
		}
		lastWarn = warn
		return true
	}
	return func(context.Context) (string, error) {
		// #nosec G304 G703 -- tokenFilePath is the provisioner-injected ATTACH_TOKEN_FILE
		// contract: the same trusted process that already injects ATTACH_TOKEN itself.
		f, err := os.Open(tokenFilePath)
		if err != nil {
			if warningChanged("read: " + err.Error()) {
				logger.Warn("[interactive] attach token file unreadable — falling back to static token",
					"path", tokenFilePath, "err", err)
			}
			return staticToken, nil
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, maxAttachTokenFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			if warningChanged("read: " + readErr.Error()) {
				logger.Warn("[interactive] attach token file unreadable — falling back to static token",
					"path", tokenFilePath, "err", readErr)
			}
			return staticToken, nil
		}
		if closeErr != nil {
			if warningChanged("close: " + closeErr.Error()) {
				logger.Warn("[interactive] attach token file unreadable — falling back to static token",
					"path", tokenFilePath, "err", closeErr)
			}
			return staticToken, nil
		}
		if len(raw) > maxAttachTokenFileBytes {
			warningChanged("oversized")
			return "", fmt.Errorf("%w (maximum %d bytes)", errAttachTokenFileOversized, maxAttachTokenFileBytes)
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			warningChanged("empty")
			return "", errAttachTokenFileEmpty
		}
		if !isCompactJWT(tok) {
			warningChanged("malformed")
			return "", errAttachTokenFileMalformed
		}
		warningChanged("")
		return tok, nil
	}
}

// isCompactJWT performs syntax validation only. The relay remains the
// signature/claim authority; the runner verifies that a token-file value has
// three non-empty base64url segments, JSON-object header/payload segments, and
// a non-empty signature so malformed files fail before a carrier dial.
func isCompactJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	decoded := make([][]byte, len(parts))
	for i, part := range parts {
		if part == "" {
			return false
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
		if err != nil || len(raw) == 0 {
			return false
		}
		decoded[i] = raw
	}
	for _, raw := range decoded[:2] {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return false
		}
	}
	return true
}

// wallCapSeconds returns the interactive wall-clock cap (seconds) from the
// shared time-bound budget, or 0 for "no runner-side wall cap".
func wallCapSeconds(qw QueuedWork) int {
	if b := qw.InterviewBudget; b != nil {
		return b.MaxWallClockSeconds
	}
	return 0
}

// interactiveStopReason distinguishes a wall-clock cap (interactiveCtx timed
// out while the parent is still live) from a plain runner/daemon cancel, for
// the operator log.
func interactiveStopReason(interactiveCtx, parentCtx context.Context) string {
	if errors.Is(interactiveCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil {
		return "interactive session wall-clock cap reached"
	}
	return "interactive session cancelled"
}
