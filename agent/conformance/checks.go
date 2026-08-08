package conformance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// liveInjectMechanisms are the declared NoticeDelivery mechanisms that promise
// delivery INTO a session that is still running, through Handle.Inject.
//
// The two declared mechanisms deliberately absent are not lesser: they simply
// do not deliver into a live process. NoticeDeliveryResumeInject requires the
// running session to have finished (a second writer otherwise), and
// NoticeDeliveryPTYNotice writes at a terminal with no agent behind it. This
// suite has nothing to certify for either, and says so rather than failing
// them for a promise they never made.
var liveInjectMechanisms = map[agent.NoticeDelivery]struct{}{
	agent.NoticeDeliveryHook:        {},
	agent.NoticeDeliveryMCPRPC:      {},
	agent.NoticeDeliveryHTTPSession: {},
	agent.NoticeDeliveryACP:         {},
	agent.NoticeDeliveryRPCSteer:    {},
	agent.NoticeDeliveryInBoxLoop:   {},
}

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
	resumed *probe
	receipt *receiptProbe
}

// probe is one spawn-and-drain observation.
type probe struct {
	events    []agent.Event
	closed    bool // the events channel was observed closed
	spawnErr  error
	stopErrs  []error
	sessionID string
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
	fired := false
drain:
	for {
		select {
		case ev, ok := <-handle.Events():
			if !ok {
				p.closed = true
				break drain
			}
			p.events = append(p.events, ev)
			if _, isInit := ev.(agent.InitEvent); isInit && onInit != nil && !fired {
				fired = true
				go onInit(pctx, handle)
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

func checkNoticeBuildDrives(_ context.Context, r *run) (Status, string) {
	nd := r.manifest.Caps.NoticeDelivery
	if !nd.Declared() {
		return StatusFail, "the manifest declares no known notice-delivery mechanism"
	}
	if _, live := liveInjectMechanisms[nd]; !live {
		return StatusNotApplicable, fmt.Sprintf(
			"mechanism %q does not deliver into a running process (it is a legitimate declaration, and there is no live channel here to certify)",
			string(nd))
	}
	// A declaration says the HARNESS exposes the channel; SupportsMessageInjection
	// says THIS BUILD drives it. They are separate facts on purpose, and this
	// is where the pair is judged: a manifest naming a live mechanism that the
	// build cannot drive leaves an upstream caller holding an unreachable
	// session, so it earns nothing.
	if !r.subject.Provider.Capabilities().SupportsMessageInjection {
		return StatusNotApplicable, fmt.Sprintf(
			"the manifest declares live mechanism %q but this build does not drive it (Capabilities().SupportsMessageInjection is false), so live delivery is unproven and the live-notice tier is not earned",
			string(nd))
	}
	return StatusPass, ""
}

func checkNoticeLiveDelivery(ctx context.Context, r *run) (Status, string) {
	nd := r.manifest.Caps.NoticeDelivery
	if !nd.Declared() {
		return StatusFail, "the manifest declares no known notice-delivery mechanism"
	}
	if _, live := liveInjectMechanisms[nd]; !live {
		return StatusNotApplicable, fmt.Sprintf("mechanism %q does not deliver into a running process", string(nd))
	}
	if !r.subject.Provider.Capabilities().SupportsMessageInjection {
		return StatusNotApplicable, fmt.Sprintf(
			"the manifest declares live mechanism %q but this build does not drive it (Capabilities().SupportsMessageInjection is false)",
			string(nd))
	}

	np := r.noticeProbe(ctx)
	if reason, failed := np.probe.spawnFailure(); failed {
		return StatusFail, reason
	}
	if np.injectErr != nil {
		return StatusFail, fmt.Sprintf(
			"the manifest declares live mechanism %q but Handle.Inject returned %v",
			string(nd), np.injectErr)
	}
	if !np.injectCalled {
		return StatusFail, fmt.Sprintf(
			"the session never emitted an InitEvent, so there was no live session to inject into (%d events drained)",
			len(np.probe.events))
	}
	if !containsText(np.probe.events, np.nonce) {
		return StatusFail, fmt.Sprintf(
			"Handle.Inject accepted the notice but nonce %q never appeared in the %d drained events: either the adapter dropped it, or Subject.EchoPrompt did not keep the session open long enough to receive it",
			np.nonce, len(np.probe.events))
	}
	return StatusPass, ""
}

// noticeProbe is a spawn whose InitEvent triggers a mid-session injection.
type noticeProbe struct {
	probe        *probe
	nonce        string
	injectErr    error
	injectCalled bool
}

func (r *run) noticeProbe(ctx context.Context) *noticeProbe {
	if r.notice != nil {
		return r.notice
	}
	np := &noticeProbe{nonce: newNonce()}
	// Buffered so the injecting goroutine never blocks on a drain that has
	// already finished, and so nothing is written after this function returns.
	results := make(chan error, 1)
	spec := r.subject.BaseSpec
	spec.Prompt = r.subject.EchoPrompt(newNonce())
	spec.RequiresLiveNotice = true

	np.probe = r.spawn(ctx, spec, func(ictx context.Context, h agent.Handle) {
		results <- h.Inject(ictx, r.subject.EchoPrompt(np.nonce))
	})
	select {
	case err := <-results:
		np.injectCalled = true
		np.injectErr = err
	default:
	}
	r.notice = np
	return np
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
