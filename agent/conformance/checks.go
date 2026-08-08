package conformance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// noticeRail names the SEAM a declared notice channel is carried over, and
// therefore which rail this suite has to drive to prove delivery over THAT
// channel.
//
// # Why the rail, and not a single injection flag
//
// Capabilities().SupportsMessageInjection is a fact about ONE rail:
// Handle.Inject. It is evidence about a declared channel only when that
// channel is the one Inject carries. Reading it as a general driven-ness proxy
// is how this suite came to certify live delivery over a channel nothing in
// this repo drives: a harness can declare `hook`, implement Handle.Inject over
// an entirely different mechanism (a fresh `--resume` invocation, which the
// axis calls resume-inject precisely because it is NOT delivery into the live
// process), and collect the live-notice tier for the channel it never wired.
//
// So driven-ness is asked per channel. The notion is the runner's — see
// noticeChannelDrivenByRunner in runner/interactive_inject.go, which answers
// "does THIS BUILD push over THIS channel" and deliberately keeps that apart
// from what the harness exposes.
type noticeRail string

const (
	// railNotLive is a legitimate declaration that does not deliver into a
	// running process at all: NoticeDeliveryNone (the durable mailbox is the
	// path) and NoticeDeliveryResumeInject (real delivery, but the running
	// session must be finished or the resumed one is a second writer). There
	// is no live channel here to certify and none is failed for it.
	railNotLive noticeRail = "not-live"

	// railInject is an application-level channel the host reaches through
	// Handle.Inject. For these — and only these — SupportsMessageInjection is
	// evidence about the declared channel, because Inject is what carries it.
	railInject noticeRail = "handle-inject"

	// railPTYNotice is a write at the terminal, reached through
	// agent.InteractiveCapable → agent.InteractiveNotifier.TryWriteNotice.
	// This build drives it: runner/interactive_inject.go is that
	// implementation, and it is the ONLY notice channel this repo drives.
	railPTYNotice noticeRail = "pty-notice"

	// railUndriven is a channel whose injection point is not on agent.Handle
	// at all, so nothing here can push over it and no probe can observe one.
	// NoticeDeliveryHook is the case: the HARNESS calls out at a lifecycle
	// point and the hook's response is the injection point, which means a
	// host-side hook responder — a thing this build does not have (see
	// noticeChannelDrivenByRunner's comment, and the claude manifest's own
	// "Nothing in this repo drives the hook yet"). Reported unproven, never
	// failed: the manifest is honest, the build is the gap.
	//
	// This placement is a statement about the BUILD, not about the mechanism,
	// and it is the one entry an outside author may find too strict: a host
	// that grows a hook responder genuinely drives the channel, and this table
	// would still call it unproven. Moving the entry is that lane's job, in
	// the same change that lands the responder — the same coupling
	// noticeChannelDrivenByRunner already imposes, so a declaration and the
	// capability behind it never drift apart. Until then, unproven is the
	// honest verdict and a borrowed pass from another rail is not.
	railUndriven noticeRail = "undriven"
)

// noticeRails places every declarable mechanism on its rail.
//
// A mechanism missing from this table is a FAILURE, not a default — see
// checkNoticeBuildDrives. TestNoticeRailIsAssignedForEveryDeclaredMechanism
// keeps the table and agent's declarable set in step.
var noticeRails = map[agent.NoticeDelivery]noticeRail{
	agent.NoticeDeliveryNone:         railNotLive,
	agent.NoticeDeliveryResumeInject: railNotLive,

	agent.NoticeDeliveryMCPRPC:      railInject,
	agent.NoticeDeliveryHTTPSession: railInject,
	agent.NoticeDeliveryACP:         railInject,
	agent.NoticeDeliveryRPCSteer:    railInject,
	agent.NoticeDeliveryInBoxLoop:   railInject,

	agent.NoticeDeliveryPTYNotice: railPTYNotice,

	agent.NoticeDeliveryHook: railUndriven,
}

// Timings for the two live notice rails.
const (
	// injectResultGrace bounds how long the suite waits for Handle.Inject to
	// return AFTER the session has drained and been stopped twice. Inject was
	// called at the InitEvent, so by then it has had the whole session to
	// answer; anything still outstanding is a stalled call, and the report
	// says so rather than dropping the outcome on the floor.
	injectResultGrace = 2 * time.Second

	// ptyNoticeRetry / ptyNoticeMaxAttempts mirror the runner's refusal
	// handling: TryWriteNotice answering (false, nil) means "not now, hold and
	// retry", never "cannot". A probe that gave up on the first refusal would
	// fail honest surfaces, and one that never gave up would hang.
	ptyNoticeRetry       = 100 * time.Millisecond
	ptyNoticeMaxAttempts = 20

	// ptyEchoPoll / ptyEchoWait bound the wait for the session's own output
	// to appear on the terminal after the notice was written.
	ptyEchoPoll = 100 * time.Millisecond
	ptyEchoWait = 10 * time.Second
)

// noticeSubmit is the byte that makes a terminal notice arrive as a SUBMITTED
// turn rather than text parked in a line editor. CR (0x0D) is what a terminal
// sends for Return; LF is the Ctrl-J chord, and a raw-mode reader maps the two
// to different keys. The runner's interactive supervisor writes CR for exactly
// this reason (interactiveNoticeSubmit in runner/interactive_inject.go) and a
// probe that framed it differently would not be exercising the same rail.
const noticeSubmit = '\r'

// ptyEchoOccurrences is how many times the nonce must appear on the terminal
// before the PTY rail counts as DELIVERED.
//
// Writing the notice puts the nonce on screen once all by itself: a terminal
// at a prompt echoes what is typed at it, so one occurrence proves only that
// bytes left this process. The second occurrence is the session's own output —
// the thing that proves it ACTED on the line rather than merely displaying it.
// Requiring two is the same distinction the Handle.Inject rail draws by
// demanding the nonce back out of the event stream: "we wrote" is not
// "it arrived".
//
// It is conservative in one direction on purpose. A session running with
// terminal echo off produces one occurrence for a delivery that really
// happened, and this check calls that unproven rather than passing on it. The
// failure names the case so the reader is not left guessing.
const ptyEchoOccurrences = 2

// requiredMaterializationChannels mirrors the closed channel set
// agent.ValidatePreparedHarness requires of any adaptation authority. The
// agent package keeps its copy unexported, so this one is pinned by
// TestRequiredMaterializationChannelsMatchAgentPackage: if the contract gains
// or loses a channel, that test goes red rather than this suite silently
// failing every subject's row-10 tier.
var requiredMaterializationChannels = []string{
	"worktree", "environment", "credentials", "config",
	"endpoint_delivery", "services", "child_process", "runtime", "cleanup",
}

// checks is the registry. Order is report order.
var checks = []Check{
	{
		ID: IDSingleInit, Tier: TierEventContract, Row: 6,
		What: "the session emits exactly one InitEvent and it is first",
		run:  checkSingleInit,
	},
	{
		ID: IDTerminalContract, Tier: TierEventContract, Row: 6,
		What: "the session emits exactly one terminal event and it is last",
		run:  checkTerminalContract,
	},
	{
		ID: IDCompleteText, Tier: TierEventContract, Row: 6,
		What: "assistant text arrives as complete messages, never per-token",
		run:  checkCompleteText,
	},
	{
		ID: IDChannelCloses, Tier: TierEventContract, Row: 6,
		What: "the events channel closes after the terminal event",
		run:  checkChannelCloses,
	},
	{
		ID: IDStopIdempotent, Tier: TierEventContract, Row: 6,
		What: "Stop after channel close is idempotent and returns nil",
		run:  checkStopIdempotent,
	},
	{
		ID: IDNoticeMechanism, Tier: TierLiveNotice, Row: 9,
		What: "the manifest declares a known notice-delivery mechanism",
		run:  checkNoticeMechanism,
	},
	{
		ID: IDNoticeBuildDrives, Tier: TierLiveNotice, Row: 9,
		What: "this build drives the notice channel the manifest declares",
		run:  checkNoticeBuildDrives,
	},
	{
		ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Row: 6,
		What: "a message injected mid-session reaches the agent",
		run:  checkNoticeLiveDelivery,
	},
	{
		ID: IDResumeContinues, Tier: TierResume, Row: 6,
		What: "a resumed session re-announces and re-terminates conformantly",
		run:  checkResumeContinues,
	},
	{
		ID: IDReceiptPlanValid, Tier: TierAdaptationReceipt, Row: 10,
		What: "the pre-spawn adaptation authority compiles and validates ready",
		run:  checkReceiptPlanValid,
	},
	{
		ID: IDReceiptModeProfiles, Tier: TierAdaptationReceipt, Row: 10,
		What: "every session mode the harness claims carries its own adaptation profile",
		run:  checkReceiptModeProfiles,
	},
	{
		ID: IDReceiptSecretFree, Tier: TierAdaptationReceipt, Row: 10,
		What: "the serialized authority carries no secret values",
		run:  checkReceiptSecretFree,
	},
}

var checkByID = func() map[CheckID]Check {
	out := make(map[CheckID]Check, len(checks))
	for _, c := range checks {
		out[c.ID] = c
	}
	return out
}()

// Checks returns every check the suite runs, in report order.
func Checks() []Check {
	return append([]Check(nil), checks...)
}

// ChecksForTier returns the checks that make up one tier.
func ChecksForTier(t Tier) []Check {
	var out []Check
	for _, c := range checks {
		if c.Tier == t {
			out = append(out, c)
		}
	}
	return out
}

// run carries the subject and the lazily-computed probes across checks, so a
// tier's checks share one spawn instead of one spawn each.
type run struct {
	subject  Subject
	manifest agent.HarnessManifest

	base    *probe
	notice  *noticeProbe
	pty     *ptyProbe
	resumed *probe
	receipt *receiptProbe
}

// probe is one spawn-and-drain observation.
type probe struct {
	events []agent.Event
	closed bool // the events channel was observed closed
	// initObserved records that the drain saw an InitEvent, and therefore that
	// an onInit callback was launched. It is recorded by the drain rather than
	// inferred from whether the callback answered, so a callback that answers
	// late is never mistaken for one that never ran.
	initObserved bool
	spawnErr     error
	stopErrs     []error
	sessionID    string
}

// spawnFailure renders the shared reason every event-contract check reports
// when the probe never produced a stream to judge.
func (p *probe) spawnFailure() (string, bool) {
	if p.spawnErr != nil {
		return "spawn failed: " + p.spawnErr.Error(), true
	}
	return "", false
}

func (r *run) baseProbe(ctx context.Context) *probe {
	if r.base == nil {
		spec := r.subject.BaseSpec
		spec.Prompt = r.subject.EchoPrompt(newNonce())
		r.base = r.spawn(ctx, spec, nil)
	}
	return r.base
}

// spawn runs one probe to completion: spawn, drain to channel close or
// timeout, then Stop twice. onInit fires once, on its own goroutine, as soon
// as the InitEvent is observed, so a check can inject into a session that is
// still draining.
func (r *run) spawn(ctx context.Context, spec agent.Spec, onInit func(context.Context, agent.Handle)) *probe {
	pctx, cancel := context.WithTimeout(ctx, r.subject.timeout())
	defer cancel()

	handle, err := r.subject.Provider.Spawn(pctx, spec)
	if err != nil {
		return &probe{spawnErr: err}
	}
	if handle == nil {
		return &probe{spawnErr: fmt.Errorf("Spawn returned a nil Handle and a nil error")}
	}
	p := &probe{}
drain:
	for {
		select {
		case ev, ok := <-handle.Events():
			if !ok {
				p.closed = true
				break drain
			}
			p.events = append(p.events, ev)
			if _, isInit := ev.(agent.InitEvent); isInit && !p.initObserved {
				p.initObserved = true
				if onInit != nil {
					go onInit(pctx, handle)
				}
			}
		case <-pctx.Done():
			break drain
		}
	}
	p.sessionID = handle.SessionID()

	// Stop is contractually safe after close, so it gets its own deadline:
	// the drain may have ended by timeout, and a Stop bounded by an
	// already-expired context would prove nothing.
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer scancel()
	p.stopErrs = []error{handle.Stop(sctx), handle.Stop(sctx)}
	return p
}

func checkSingleInit(ctx context.Context, r *run) (Status, string) {
	p := r.baseProbe(ctx)
	if reason, failed := p.spawnFailure(); failed {
		return StatusFail, reason
	}
	if err := CheckSingleInit(p.events); err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, ""
}

func checkTerminalContract(ctx context.Context, r *run) (Status, string) {
	p := r.baseProbe(ctx)
	if reason, failed := p.spawnFailure(); failed {
		return StatusFail, reason
	}
	if err := CheckTerminalContract(p.events); err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, ""
}

func checkCompleteText(ctx context.Context, r *run) (Status, string) {
	p := r.baseProbe(ctx)
	if reason, failed := p.spawnFailure(); failed {
		return StatusFail, reason
	}
	if err := CheckCompleteAssistantTexts(p.events); err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, ""
}

func checkChannelCloses(ctx context.Context, r *run) (Status, string) {
	p := r.baseProbe(ctx)
	if reason, failed := p.spawnFailure(); failed {
		return StatusFail, reason
	}
	if !p.closed {
		return StatusFail, fmt.Sprintf(
			"the events channel had not closed %s after spawn (%d events drained); the Handle contract closes it after the terminal event",
			r.subject.timeout(), len(p.events))
	}
	return StatusPass, ""
}

func checkStopIdempotent(ctx context.Context, r *run) (Status, string) {
	p := r.baseProbe(ctx)
	if reason, failed := p.spawnFailure(); failed {
		return StatusFail, reason
	}
	for i, err := range p.stopErrs {
		if err != nil {
			return StatusFail, fmt.Sprintf("Stop call %d after channel close returned %v; the Handle contract makes Stop idempotent and nil-returning once the session is done", i+1, err)
		}
	}
	return StatusPass, ""
}

func checkNoticeMechanism(_ context.Context, r *run) (Status, string) {
	nd := r.manifest.Caps.NoticeDelivery
	if !nd.Declared() {
		return StatusFail, fmt.Sprintf(
			"the manifest declares notice delivery %q, which is not one of the known mechanisms; an unanswered axis is denied rather than defaulted, so upstream callers cannot reach this harness at all",
			string(nd))
	}
	return StatusPass, ""
}

// railFor resolves the declared mechanism onto its rail, failing closed. A
// mechanism the agent package declares but this table has not placed is a gap
// in the SUITE, and the suite says so rather than falling through to a verdict
// it has no basis for.
func railFor(nd agent.NoticeDelivery) (noticeRail, Status, string) {
	if !nd.Declared() {
		return "", StatusFail, "the manifest declares no known notice-delivery mechanism"
	}
	rail, ok := noticeRails[nd]
	if !ok {
		return "", StatusFail, fmt.Sprintf(
			"mechanism %q is declarable but this suite has placed it on no delivery rail, so it has no way to judge whether the channel is driven; the rail table (noticeRails) has to name it before any verdict here means anything",
			string(nd))
	}
	return rail, StatusPass, ""
}

// checkNoticeBuildDrives asks the per-channel question: does THIS BUILD push
// over the channel the manifest declares?
//
// The answer differs by rail, and that is the whole point. For a channel
// Handle.Inject carries, Capabilities().SupportsMessageInjection is the right
// evidence. For the terminal channel the runner drives it directly. For a
// channel whose injection point is not on agent.Handle at all, nothing here
// can push over it and the honest answer is "unproven" — never a pass borrowed
// from a different channel's rail.
func checkNoticeBuildDrives(_ context.Context, r *run) (Status, string) {
	nd := r.manifest.Caps.NoticeDelivery
	rail, status, reason := railFor(nd)
	if status != StatusPass {
		return status, reason
	}
	switch rail {
	case railNotLive:
		return StatusNotApplicable, fmt.Sprintf(
			"mechanism %q does not deliver into a running process (it is a legitimate declaration, and there is no live channel here to certify)",
			string(nd))

	case railUndriven:
		return StatusNotApplicable, fmt.Sprintf(
			"the manifest declares live mechanism %q, whose injection point is the harness calling out to a host-side responder — not anything on agent.Handle. This build has no such responder, so it does not drive %q and live delivery over it is unproven. Capabilities().SupportsMessageInjection is NOT evidence here: it is a fact about the Handle.Inject rail, which for this channel carries some other mechanism entirely",
			string(nd), string(nd))

	case railPTYNotice:
		// This build drives the terminal channel — runner/interactive_inject.go
		// is that implementation. Whether the subject's own session accepts one
		// is a separate question, and notice/live-delivery answers it.
		return StatusPass, ""

	case railInject:
		if !r.subject.Provider.Capabilities().SupportsMessageInjection {
			return StatusNotApplicable, fmt.Sprintf(
				"the manifest declares live mechanism %q, which is carried by Handle.Inject, but this build does not drive that rail (Capabilities().SupportsMessageInjection is false), so live delivery is unproven and the live-notice tier is not earned",
				string(nd))
		}
		return StatusPass, ""

	default:
		return StatusFail, fmt.Sprintf("mechanism %q resolved to unknown rail %q", string(nd), string(rail))
	}
}

func checkNoticeLiveDelivery(ctx context.Context, r *run) (Status, string) {
	nd := r.manifest.Caps.NoticeDelivery
	rail, status, reason := railFor(nd)
	if status != StatusPass {
		return status, reason
	}
	switch rail {
	case railNotLive:
		return StatusNotApplicable, fmt.Sprintf("mechanism %q does not deliver into a running process", string(nd))
	case railUndriven:
		return StatusNotApplicable, fmt.Sprintf(
			"nothing in this build pushes over mechanism %q, so there is no delivery to observe; a probe that injected on some other rail would be certifying a channel it never touched",
			string(nd))
	case railPTYNotice:
		return checkPTYNoticeDelivery(ctx, r, nd)
	case railInject:
		return checkInjectNoticeDelivery(ctx, r, nd)
	default:
		return StatusFail, fmt.Sprintf("mechanism %q resolved to unknown rail %q", string(nd), string(rail))
	}
}

// checkInjectNoticeDelivery certifies the Handle.Inject rail: a nonce injected
// mid-session has to come back out of the event stream.
func checkInjectNoticeDelivery(ctx context.Context, r *run, nd agent.NoticeDelivery) (Status, string) {
	if !r.subject.Provider.Capabilities().SupportsMessageInjection {
		return StatusNotApplicable, fmt.Sprintf(
			"the manifest declares live mechanism %q but this build does not drive the Handle.Inject rail that carries it (Capabilities().SupportsMessageInjection is false)",
			string(nd))
	}

	np := r.noticeProbe(ctx)
	if reason, failed := np.probe.spawnFailure(); failed {
		return StatusFail, reason
	}
	if !np.probe.initObserved {
		return StatusFail, fmt.Sprintf(
			"the session never emitted an InitEvent, so there was no live session to inject into (%d events drained)",
			len(np.probe.events))
	}
	if np.injectStalled {
		return StatusFail, fmt.Sprintf(
			"Handle.Inject had not returned %s after the session drained and was stopped; a caller that queued this notice can never learn whether it was accepted, which is the acked-and-never-delivered shape this tier exists to catch",
			injectResultGrace)
	}
	if np.injectErr != nil {
		return StatusFail, fmt.Sprintf(
			"the manifest declares live mechanism %q but Handle.Inject returned %v",
			string(nd), np.injectErr)
	}
	if !containsText(np.probe.events, np.nonce) {
		return StatusFail, fmt.Sprintf(
			"Handle.Inject accepted the notice but nonce %q never appeared in the %d drained events: either the adapter dropped it, or Subject.EchoPrompt did not keep the session open long enough to receive it",
			np.nonce, len(np.probe.events))
	}
	return StatusPass, ""
}

// checkPTYNoticeDelivery certifies the terminal rail: the notice goes in
// through agent.InteractiveNotifier.TryWriteNotice — the same seam the runner
// uses, and one the host refuses outright for any harness that has not
// declared pty-notice — and the session's own output has to come back on the
// screen.
func checkPTYNoticeDelivery(ctx context.Context, r *run, nd agent.NoticeDelivery) (Status, string) {
	pp := r.ptyProbe(ctx)
	if pp.spawnErr != nil {
		return StatusFail, "spawn failed: " + pp.spawnErr.Error()
	}
	if pp.surfaceReason != "" {
		return StatusFail, pp.surfaceReason
	}
	if pp.writeErr != nil {
		return StatusFail, fmt.Sprintf(
			"the manifest declares %q but TryWriteNotice failed: %v", string(nd), pp.writeErr)
	}
	if !pp.written {
		if pp.writeAborted {
			return StatusFail, fmt.Sprintf(
				"the probe budget (%s) ran out while the live surface was still refusing the notice, after %d attempts over %s; nothing was delivered and a longer Subject.ProbeTimeout is the only thing that could change that verdict",
				r.subject.timeout(), pp.attempts, pp.writeWaited.Round(time.Millisecond))
		}
		return StatusFail, fmt.Sprintf(
			"the live surface refused the notice on all %d attempts over %s and never took it; a refusal is the surface saying \"not now\", and a session that says that forever has no live channel however the manifest reads",
			pp.attempts, pp.writeWaited.Round(time.Millisecond))
	}
	if pp.occurrences >= ptyEchoOccurrences {
		return StatusPass, ""
	}
	if pp.occurrences == 0 {
		return StatusFail, fmt.Sprintf(
			"TryWriteNotice reported the notice written but nonce %q never reached the terminal at all in %s",
			pp.nonce, pp.echoWaited.Round(time.Millisecond))
	}
	return StatusFail, fmt.Sprintf(
		"nonce %q appeared on the terminal exactly once in %s — that occurrence is the line this suite typed being echoed back, not the session acting on it, so nothing was delivered. (A session running with terminal echo OFF would look the same; this check calls that unproven rather than passing on one occurrence.)",
		pp.nonce, pp.echoWaited.Round(time.Millisecond))
}

// noticeProbe is a spawn whose InitEvent triggers a mid-session injection.
type noticeProbe struct {
	probe     *probe
	nonce     string
	injectErr error
	// injectStalled records that Handle.Inject had still not returned
	// injectResultGrace after the drain ended.
	injectStalled bool
}

func (r *run) noticeProbe(ctx context.Context) *noticeProbe {
	if r.notice != nil {
		return r.notice
	}
	np := &noticeProbe{nonce: newNonce()}
	// Buffered so the injecting goroutine never blocks, whatever this function
	// does with the result.
	results := make(chan error, 1)
	spec := r.subject.BaseSpec
	spec.Prompt = r.subject.EchoPrompt(newNonce())
	spec.RequiresLiveNotice = true

	np.probe = r.spawn(ctx, spec, func(ictx context.Context, h agent.Handle) {
		results <- h.Inject(ictx, r.subject.EchoPrompt(np.nonce))
	})
	// Whether Inject was CALLED is a fact the drain recorded (initObserved);
	// whether it ANSWERED is this wait. Reading the result with a non-blocking
	// select instead — the shape this probe shipped with — silently discards
	// the error of any Inject that outlives the drain, and then misreports the
	// failure as a session that never announced itself.
	if np.probe.initObserved {
		select {
		case err := <-results:
			np.injectErr = err
		case <-time.After(injectResultGrace):
			np.injectStalled = true
		}
	}
	r.notice = np
	return np
}

// ptyProbe is one terminal-rail observation: spawn interactively, hand the
// live surface a notice, and watch the screen for the session's answer.
type ptyProbe struct {
	nonce    string
	spawnErr error
	// surfaceReason is set when the spawned session exposes no surface that
	// could take a notice. It is a defect of the adapter, not of the suite:
	// the manifest declared a terminal channel and the session has no terminal
	// to write into.
	surfaceReason string
	writeErr      error
	written       bool
	// writeAborted marks that the probe budget expired mid-retry rather than
	// the surface exhausting its attempts. The two look identical in the
	// written flag and read very differently in a report.
	writeAborted bool
	attempts     int
	writeWaited  time.Duration
	occurrences  int
	echoWaited   time.Duration
}

func (r *run) ptyProbe(ctx context.Context) *ptyProbe {
	if r.pty != nil {
		return r.pty
	}
	pp := &ptyProbe{nonce: newNonce()}
	r.pty = pp

	pctx, cancel := context.WithTimeout(ctx, r.subject.timeout())
	defer cancel()

	spec := r.subject.BaseSpec
	spec.Prompt = r.subject.EchoPrompt(newNonce())
	spec.RequiresLiveNotice = true

	handle, err := r.subject.Provider.Spawn(pctx, spec)
	if err != nil {
		pp.spawnErr = err
		return pp
	}
	if handle == nil {
		pp.spawnErr = fmt.Errorf("Spawn returned a nil Handle and a nil error")
		return pp
	}
	defer func() {
		// An interactive session has no terminal event of its own to wait for
		// — the child sits at a prompt until something stops it — so the probe
		// owns the teardown.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer scancel()
		_ = handle.Stop(sctx)
	}()

	sess, notifier, reason := noticeSurface(handle)
	if reason != "" {
		pp.surfaceReason = reason
		return pp
	}

	notice := formatNotice(r.subject.EchoPrompt(pp.nonce))
	if len(notice) == 0 {
		pp.surfaceReason = "Subject.EchoPrompt rendered an empty notice, so there was nothing to deliver"
		return pp
	}
	started := time.Now()
	pp.written, pp.attempts, pp.writeAborted, pp.writeErr = writeNoticeWithRetry(pctx, notifier, notice)
	pp.writeWaited = time.Since(started)
	if !pp.written || pp.writeErr != nil {
		return pp
	}
	started = time.Now()
	pp.occurrences = awaitTerminalEcho(pctx, sess, pp.nonce)
	pp.echoWaited = time.Since(started)
	return pp
}

// noticeSurface narrows a Handle to the live terminal that can take a notice,
// naming precisely which link in the chain is missing. Both structural
// refusals here are the runner's own — its noticeDeadSurfaceNotNotifier
// dead-letter is this second one.
func noticeSurface(handle agent.Handle) (agent.InteractiveSession, agent.InteractiveNotifier, string) {
	capable, ok := handle.(agent.InteractiveCapable)
	if !ok {
		return nil, nil, "the manifest declares the terminal notice channel but Spawn returned a Handle that is not agent.InteractiveCapable, so there is no live terminal to write a notice into"
	}
	sess := capable.InteractiveSession()
	if sess == nil {
		return nil, nil, "the manifest declares the terminal notice channel but InteractiveSession() is nil, so there is no live terminal to write a notice into"
	}
	notifier, ok := sess.(agent.InteractiveNotifier)
	if !ok {
		return nil, nil, "the manifest declares the terminal notice channel but the live surface does not implement agent.InteractiveNotifier, so it cannot accept TryWriteNotice at all"
	}
	return sess, notifier, ""
}

// writeNoticeWithRetry makes the refusable write, retrying a (false, nil)
// refusal the way the runner's supervisor does. A refusal is the surface
// saying "not now"; only an error or an exhausted budget is an answer. The
// aborted return separates "the budget ran out" from "the surface said no
// every time", which the report must not conflate.
func writeNoticeWithRetry(ctx context.Context, notifier agent.InteractiveNotifier, notice []byte) (written bool, attempts int, aborted bool, err error) {
	for attempt := 1; attempt <= ptyNoticeMaxAttempts; attempt++ {
		ok, writeErr := notifier.TryWriteNotice(notice)
		if writeErr != nil {
			return ok, attempt, false, writeErr
		}
		if ok {
			return true, attempt, false, nil
		}
		select {
		case <-ctx.Done():
			return false, attempt, true, nil
		case <-time.After(ptyNoticeRetry):
		}
	}
	return false, ptyNoticeMaxAttempts, false, nil
}

// awaitTerminalEcho polls the screen until the nonce has appeared often enough
// to prove the session acted on the notice, and reports how many occurrences
// it ever saw so the failure can distinguish "nothing arrived" from "only the
// echo of what we typed".
func awaitTerminalEcho(ctx context.Context, sess agent.InteractiveSession, nonce string) int {
	deadline := time.Now().Add(ptyEchoWait)
	best := 0
	for {
		if text, err := snapshotText(sess); err == nil {
			if n := strings.Count(text, nonce); n > best {
				best = n
			}
		}
		if best >= ptyEchoOccurrences || time.Now().After(deadline) {
			return best
		}
		select {
		case <-ctx.Done():
			return best
		case <-time.After(ptyEchoPoll):
		}
	}
}

// snapshotText renders the current screen as searchable text: the scrollback
// tail first, then the visible grid split on the reported column count.
//
// It reads the snapshot through type inference rather than naming the frame
// types, which keeps this package's import set at agent + the standard library
// — the property that lets any provider test package consume it without a
// dependency cycle.
func snapshotText(sess agent.InteractiveSession) (string, error) {
	screen, _, err := sess.Snapshot()
	if err != nil {
		return "", fmt.Errorf("snapshot the terminal: %w", err)
	}
	var b strings.Builder
	for _, line := range screen.Scrollback {
		for _, cell := range line {
			b.Write(cell.RuneBytes)
		}
		b.WriteByte('\n')
	}
	cols := int(screen.Cols) //nolint:gosec // a terminal column count is small and host-reported
	for i, cell := range screen.Primary {
		if cols > 0 && i > 0 && i%cols == 0 {
			b.WriteByte('\n')
		}
		b.Write(cell.RuneBytes)
	}
	return b.String(), nil
}

// formatNotice renders one notice as the exact bytes to hand to
// TryWriteNotice. It follows the runner's framing rules (flattened to a single
// line, self-submitting with a trailing CR) because a probe that framed the
// bytes differently would not be exercising the rail the runner drives.
func formatNotice(text string) []byte {
	flat := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\v', '\f':
			return ' '
		default:
			return r
		}
	}, text))
	if flat == "" {
		return nil
	}
	return append([]byte(flat), noticeSubmit)
}

func checkResumeContinues(ctx context.Context, r *run) (Status, string) {
	if !r.manifest.Caps.SupportsSessionResume {
		return StatusNotApplicable, "the manifest does not declare session resume"
	}
	base := r.baseProbe(ctx)
	if reason, failed := base.spawnFailure(); failed {
		return StatusFail, "the session to resume could not be started: " + reason
	}
	sessionID := base.sessionID
	if sessionID == "" {
		sessionID = initSessionID(base.events)
	}
	if sessionID == "" {
		return StatusFail, "the manifest declares session resume but the first session announced no session id (Handle.SessionID and the InitEvent were both empty), so there is nothing to resume"
	}

	if r.resumed == nil {
		r.resumed = r.resumeProbe(ctx, sessionID)
	}
	if r.resumed.spawnErr != nil {
		return StatusFail, fmt.Sprintf("the manifest declares session resume but Resume(%q) returned %v", sessionID, r.resumed.spawnErr)
	}
	if err := CheckEventContract(r.resumed.events); err != nil {
		return StatusFail, "the resumed session violated the event contract: " + err.Error()
	}
	if !r.resumed.closed {
		return StatusFail, fmt.Sprintf("the resumed session's events channel had not closed %s after Resume", r.subject.timeout())
	}
	return StatusPass, ""
}

// resumeProbe continues a prior session and drains it. It mirrors spawn but
// calls Resume, and it holds the probe context open across the whole drain —
// cancelling it at the Resume call would kill the very session under test.
func (r *run) resumeProbe(ctx context.Context, sessionID string) *probe {
	pctx, cancel := context.WithTimeout(ctx, r.subject.timeout())
	defer cancel()

	spec := r.subject.BaseSpec
	spec.Prompt = r.subject.EchoPrompt(newNonce())
	handle, err := r.subject.Provider.Resume(pctx, sessionID, spec)
	if err != nil {
		return &probe{spawnErr: err}
	}
	if handle == nil {
		return &probe{spawnErr: fmt.Errorf("Resume returned a nil Handle and a nil error")}
	}
	p := &probe{}
drain:
	for {
		select {
		case ev, ok := <-handle.Events():
			if !ok {
				p.closed = true
				break drain
			}
			p.events = append(p.events, ev)
		case <-pctx.Done():
			break drain
		}
	}
	p.sessionID = handle.SessionID()
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer scancel()
	p.stopErrs = []error{handle.Stop(sctx)}
	return p
}

// receiptProbe compiles the row-10 adaptation authority once.
type receiptProbe struct {
	plan       *agent.PreparedHarness
	compileErr error
	digest     string
	spec       agent.Spec
}

func (r *run) receiptProbe() *receiptProbe {
	if r.receipt != nil {
		return r.receipt
	}
	fixture := r.subject.Adaptation
	rp := &receiptProbe{}
	rp.spec = fixture.Spec
	rp.digest = fixture.OperationalDigest
	if strings.TrimSpace(rp.digest) == "" {
		rp.digest = stableDigest(fixture.Spec)
	}
	materializations := make([]agent.HarnessMaterialization, 0, len(requiredMaterializationChannels))
	for _, channel := range requiredMaterializationChannels {
		materializations = append(materializations, agent.HarnessMaterialization{
			Channel: channel, SourceDigest: rp.digest, Required: true,
		})
	}
	rp.plan, rp.compileErr = agent.CompilePreparedHarness(
		fixture.Spec, r.manifest, rp.digest, fixture.RuntimeMCPNames, materializations)
	r.receipt = rp
	return rp
}

// noFixture is the shared not-applicable for the row-10 tier when the author
// supplied no adaptation evidence.
const noFixture = "no Subject.Adaptation fixture was supplied, so there is no pre-spawn adaptation authority to check"

func checkReceiptPlanValid(_ context.Context, r *run) (Status, string) {
	if r.subject.Adaptation == nil {
		return StatusNotApplicable, noFixture
	}
	rp := r.receiptProbe()
	if rp.compileErr != nil {
		return StatusFail, "compiling the adaptation authority failed: " + rp.compileErr.Error()
	}
	if err := agent.ValidatePreparedHarness(rp.plan, rp.digest); err != nil {
		return StatusFail, "the compiled adaptation authority does not validate: " + err.Error()
	}
	return StatusPass, ""
}

// checkReceiptModeProfiles enforces row 10's "headless and PTY modes carry
// separate evidence" at the manifest: a harness that claims a session mode
// must declare the adaptation profiles that mode admits under. A manifest
// missing one does not merely lose evidence — it cannot be admitted in that
// mode at all, and the failure surfaces at spawn time instead of here.
func checkReceiptModeProfiles(_ context.Context, r *run) (Status, string) {
	modes := []agent.PromptSessionMode{agent.PromptModeAutonomous}
	if r.manifest.Caps.SupportsInteractivePTY {
		modes = append(modes, agent.PromptModeHumanControlled)
	}
	for _, mode := range modes {
		if _, ok := r.manifest.PromptProfile(mode); !ok {
			return StatusFail, fmt.Sprintf(
				"the manifest claims session mode %q but declares no prompt-delivery profile for it, so no prompt can be admitted in that mode",
				mode)
		}
		if _, ok := r.manifest.ToolLifecycleProfile(mode); !ok {
			return StatusFail, fmt.Sprintf(
				"the manifest claims session mode %q but declares no tool/lifecycle profile for it, so no tool policy can be admitted in that mode",
				mode)
		}
	}
	return StatusPass, ""
}

func checkReceiptSecretFree(_ context.Context, r *run) (Status, string) {
	if r.subject.Adaptation == nil {
		return StatusNotApplicable, noFixture
	}
	secrets := nonEmpty(r.subject.Adaptation.SecretValues)
	if len(secrets) == 0 {
		return StatusNotApplicable, "the fixture declares no AdaptationFixture.SecretValues, so there is nothing whose absence could be proven; a vacuous pass here would be worse than no check"
	}
	rp := r.receiptProbe()
	if rp.compileErr != nil {
		return StatusFail, "compiling the adaptation authority failed: " + rp.compileErr.Error()
	}
	raw, err := json.Marshal(rp.plan)
	if err != nil {
		return StatusFail, "serializing the adaptation authority failed: " + err.Error()
	}
	serialized := string(raw)
	for i, secret := range secrets {
		if strings.Contains(serialized, secret) {
			return StatusFail, fmt.Sprintf("AdaptationFixture.SecretValues[%d] appears verbatim in the serialized adaptation authority; the authority is persisted before spawn and must carry references and digests only", i)
		}
	}
	return StatusPass, ""
}

// containsText reports whether needle appears in any agent-authored text in
// the sequence. The needle is a fresh nonce, so any occurrence is proof the
// message reached the agent and came back out.
func containsText(events []agent.Event, needle string) bool {
	if needle == "" {
		return false
	}
	for _, ev := range events {
		var haystack string
		switch e := ev.(type) {
		case agent.AssistantTextEvent:
			haystack = e.Text
		case agent.ToolUseEvent:
			haystack = fmt.Sprint(e.Input)
		case agent.ToolResultEvent:
			haystack = e.Content
		case agent.ResultEvent:
			haystack = e.Message
		case agent.SystemEvent:
			haystack = e.Message
		default:
			continue
		}
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// initSessionID returns the session id announced on the InitEvent, if any.
func initSessionID(events []agent.Event) string {
	for _, ev := range events {
		if init, ok := ev.(agent.InitEvent); ok {
			return init.SessionID
		}
	}
	return ""
}

// newNonce returns a fresh probe token. It is deliberately unguessable rather
// than a counter: the token's whole job is to make "this text came back from
// the agent" unfalsifiable by coincidence.
func newNonce() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read is documented never to fail on the platforms this
		// binary targets; if it ever does, a constant token still yields a
		// working (if guessable) probe rather than a panic in a library.
		return "donmai-conformance-fallback-nonce"
	}
	return "donmai-conformance-" + hex.EncodeToString(buf[:])
}

func stableDigest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(fmt.Sprint(v))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
