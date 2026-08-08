package conformance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// DefaultProbeTimeout bounds each live probe (one spawn-and-drain). A harness
// that has not reached its terminal event by then fails the checks that
// depend on that probe rather than hanging the suite.
const DefaultProbeTimeout = 2 * time.Minute

// stopTimeout bounds the two post-drain Stop calls. Stop is documented as
// idempotent and safe after channel close, so it should return immediately;
// this only stops a wedged adapter from hanging the run.
const stopTimeout = 15 * time.Second

// Status is the outcome of one check.
type Status string

// The three outcomes a check can report. There is no fourth: an error
// reaching the harness is a failure of the check that needed it, never a
// separate "inconclusive" that a reader can round down to a pass.
const (
	// StatusPass means the invariant was observed to hold.
	StatusPass Status = "pass"
	// StatusFail means the invariant was observed to be violated, or the
	// evidence needed to judge it could not be obtained.
	StatusFail Status = "fail"
	// StatusNotApplicable means the check does not apply to this harness.
	// It always carries a reason and it NEVER earns a tier.
	StatusNotApplicable Status = "not_applicable"
)

// Decider records who ruled a check not applicable.
type Decider string

// The two deciders. The distinction is load-bearing in a report: a skip the
// suite derived from the manifest is a fact about the harness, and a skip the
// author asked for is a fact about the run.
const (
	// DeciderSuite marks a not-applicable the suite derived itself, from the
	// manifest or from the shape of the subject.
	DeciderSuite Decider = "suite"
	// DeciderSubject marks a not-applicable the harness author declared via
	// Subject.NotApplicable.
	DeciderSubject Decider = "subject"
)

// CheckID names one check. IDs are stable: they appear in reports, in
// Subject.NotApplicable keys, and in whatever a CI lane greps for.
type CheckID string

// The checks this suite runs, grouped by the tier they belong to.
const (
	// IDSingleInit — exactly one InitEvent, first.
	IDSingleInit CheckID = "event/single-init"
	// IDTerminalContract — exactly one terminal event, last.
	IDTerminalContract CheckID = "event/terminal-contract"
	// IDCompleteText — assistant text is complete messages, not tokens.
	IDCompleteText CheckID = "event/complete-assistant-text"
	// IDChannelCloses — the events channel closes after the terminal event.
	IDChannelCloses CheckID = "event/channel-closes"
	// IDStopIdempotent — Stop after close is a no-op that returns nil, twice.
	IDStopIdempotent CheckID = "session/stop-idempotent"

	// IDNoticeMechanism — the manifest answers the notice-delivery question.
	IDNoticeMechanism CheckID = "notice/mechanism-declared"
	// IDNoticeBuildDrives — this build drives the declared channel.
	IDNoticeBuildDrives CheckID = "notice/build-drives-channel"
	// IDNoticeLiveDelivery — a message injected mid-session actually arrives.
	IDNoticeLiveDelivery CheckID = "notice/live-delivery"

	// IDResumeContinues — a resumed session re-announces and re-terminates.
	IDResumeContinues CheckID = "resume/session-continues"

	// IDReceiptPlanValid — the compiled adaptation authority validates ready.
	IDReceiptPlanValid CheckID = "receipt/plan-valid"
	// IDReceiptModeProfiles — every session mode the harness claims carries
	// its own prompt and tool/lifecycle profile.
	IDReceiptModeProfiles CheckID = "receipt/mode-profiles"
	// IDReceiptSecretFree — the persisted authority carries no secret values.
	IDReceiptSecretFree CheckID = "receipt/secret-free"
)

// Checks deliberately absent from the row-10 tier
//
// Three properties row 10 asks for — that applying an authority twice yields
// identical receipts, that a Spec which drifted from its authority is
// refused, and that every downgrade in a receipt is named — are enforced by
// the shared compiler in the agent package (CompilePreparedHarness /
// ApplyPreparedHarness), which no adapter can override and every adapter
// inherits. A check here could not be made to fail by any conformant OR
// non-conformant subject, and a check nothing can fail is not a check. They
// are covered where they live, by the agent package's own tests.
//
// The remaining half of row 10 — that the artifacts an adapter MATERIALIZES
// (config files, argv, environment) carry no secrets — is invisible from
// in-process and is named in Report.Unverified instead.

// Tier is a capability level a harness EARNS by passing checks. A tier is
// never read off a manifest.
type Tier string

// The tiers this suite awards.
const (
	// TierEventContract is the floor every production harness must reach:
	// the Provider event contract holds end to end on a live session.
	TierEventContract Tier = "event-contract"
	// TierLiveNotice is earned by delivering a message INTO a running
	// session — not by declaring a mechanism that could.
	TierLiveNotice Tier = "live-notice"
	// TierResume is earned by continuing a prior session.
	TierResume Tier = "resume"
	// TierAdaptationReceipt is earned when the harness's pre-spawn adaptation
	// authority compiles, validates, re-applies identically, refuses a
	// drifted Spec, names its downgrades, and carries no secret values.
	TierAdaptationReceipt Tier = "adaptation-receipt"
)

// Tiers returns every tier this suite can award, weakest first.
func Tiers() []Tier {
	return []Tier{TierEventContract, TierLiveNotice, TierResume, TierAdaptationReceipt}
}

// Check describes one check: what it proves, which tier it belongs to, and
// which harness-addition checklist row it enforces.
type Check struct {
	// ID is the stable identifier.
	ID CheckID `json:"id"`
	// Tier is the capability tier this check contributes to.
	Tier Tier `json:"tier"`
	// Row is the harness-addition checklist row this check enforces (0 when
	// the check is not traceable to a single row).
	Row int `json:"checklistRow"`
	// What states the invariant in one line.
	What string `json:"what"`

	// run performs the check. It is unexported so the check set is closed:
	// a subject cannot substitute a check with one that always passes.
	run func(ctx context.Context, r *run) (Status, string)
}

// CheckResult is one check's outcome.
type CheckResult struct {
	// ID identifies the check.
	ID CheckID `json:"id"`
	// Tier is the tier the check contributes to.
	Tier Tier `json:"tier"`
	// Row is the checklist row the check enforces.
	Row int `json:"checklistRow"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Reason carries the failure detail on StatusFail and the REQUIRED
	// justification on StatusNotApplicable. It is empty only on a pass.
	Reason string `json:"reason,omitempty"`
	// Decider is set only on StatusNotApplicable and records who ruled it so.
	Decider Decider `json:"decider,omitempty"`
}

// newResult builds a result and enforces the honest-output rule: a
// not-applicable with no reason becomes a failure. A silent skip is not
// expressible through this constructor, and every result the suite emits goes
// through it.
func newResult(c Check, status Status, reason string, decider Decider) CheckResult {
	if status == StatusNotApplicable && strings.TrimSpace(reason) == "" {
		return CheckResult{
			ID:     c.ID,
			Tier:   c.Tier,
			Row:    c.Row,
			Status: StatusFail,
			Reason: "check reported not-applicable without a declared reason; an unexplained skip reads as a pass and is refused",
		}
	}
	res := CheckResult{ID: c.ID, Tier: c.Tier, Row: c.Row, Status: status, Reason: strings.TrimSpace(reason)}
	if status == StatusNotApplicable {
		res.Decider = decider
	}
	return res
}

// AdaptationFixture is the row-10 evidence a harness author supplies: the
// exact Spec their host would compile a pre-spawn adaptation authority from.
// The suite compiles it against the live manifest, then validates, re-applies
// and tampers with it.
type AdaptationFixture struct {
	// Spec is the source Spec. It should exercise whatever the harness
	// actually uses — base instructions, initial context, MCP servers, tool
	// policy — because the receipt can only be checked over channels the Spec
	// puts in play.
	Spec agent.Spec

	// OperationalDigest is the admitted operational-payload digest the
	// authority binds to. Empty means the suite derives a stable stand-in;
	// supply the real one when certifying against a real admission.
	OperationalDigest string

	// RuntimeMCPNames are the MCP servers materialized by the runtime rather
	// than carried in the Spec.
	RuntimeMCPNames []string

	// SecretValues are literal secret values present in Spec (tokens, keys,
	// passwords). IDReceiptSecretFree proves none of them survives into the
	// serialized authority. Leave empty and the check reports not-applicable
	// rather than passing vacuously.
	SecretValues []string
}

// Subject is everything a harness author supplies to run the suite: the
// adapter, a Spec template, the one piece of harness-specific glue the suite
// cannot write (EchoPrompt), and any declared not-applicables.
type Subject struct {
	// Provider is the adapter under certification. Required.
	Provider agent.HarnessProvider

	// BaseSpec is the Spec template every live probe clones. The suite
	// overwrites Prompt; everything else (Cwd, Env, model, endpoint, tool
	// policy) is the author's. Keep it as close to a production Spec as the
	// certification environment allows.
	BaseSpec agent.Spec

	// EchoPrompt renders a prompt instructing this harness's agent to
	// reproduce nonce verbatim in its output. Required.
	//
	// It is the one thing only the author can write, and it carries two
	// obligations. First, the agent must echo the nonce — that echo is how
	// the suite proves a message ARRIVED rather than trusting that Inject
	// returned nil. Second, for the live-notice probe the prompt must leave
	// the session open long enough to accept a follow-up (typically: echo,
	// then wait for further instructions), because a session that has already
	// terminated cannot receive a notice and the check cannot tell that apart
	// from an adapter that dropped one.
	EchoPrompt func(nonce string) string

	// Adaptation is the optional row-10 fixture. Without it the
	// adaptation-receipt tier is reported not-applicable, never earned.
	Adaptation *AdaptationFixture

	// NotApplicable declares, per check, why the author believes it cannot
	// run in this environment. A reason is mandatory — an entry with an empty
	// reason is a Run error, not a skip — and a declared not-applicable never
	// earns its tier. It buys an honest report, not a pass.
	NotApplicable map[CheckID]string

	// ProbeTimeout bounds each live probe. Zero means DefaultProbeTimeout.
	ProbeTimeout time.Duration
}

func (s Subject) validate() error {
	if s.Provider == nil {
		return errors.New("conformance: Subject.Provider is required")
	}
	if s.EchoPrompt == nil {
		return errors.New("conformance: Subject.EchoPrompt is required — the suite cannot prove a message arrived without a harness-specific way to make the agent echo it")
	}
	for id, reason := range s.NotApplicable {
		if _, ok := checkByID[id]; !ok {
			return fmt.Errorf("conformance: Subject.NotApplicable names unknown check %q", id)
		}
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("conformance: Subject.NotApplicable[%q] declares no reason; an unexplained skip reads as a pass and is refused", id)
		}
	}
	return nil
}

func (s Subject) timeout() time.Duration {
	if s.ProbeTimeout > 0 {
		return s.ProbeTimeout
	}
	return DefaultProbeTimeout
}

// TierResult reports whether a tier was earned and, when it was not, the
// first check that stood in the way.
type TierResult struct {
	// Tier is the tier.
	Tier Tier `json:"tier"`
	// Earned is true only when every check in the tier passed.
	Earned bool `json:"earned"`
	// Reason names the first non-passing check and its reason. Empty when
	// the tier was earned.
	Reason string `json:"reason,omitempty"`
}

// Report is the outcome of a suite run.
type Report struct {
	// Harness is the manifest's harness name.
	Harness string `json:"harness"`
	// ContractABI is the manifest's declared contract ABI.
	ContractABI string `json:"contractAbi"`
	// Results are the per-check outcomes, in registry order.
	Results []CheckResult `json:"results"`
	// TierResults are the tier verdicts, weakest tier first.
	TierResults []TierResult `json:"tiers"`
	// Unverified names the claims this suite does NOT check, so a green
	// report cannot be read as full certification. See UnverifiedClaims.
	Unverified []Claim `json:"unverified"`
}

// Result returns the outcome for one check.
func (r *Report) Result(id CheckID) (CheckResult, bool) {
	for _, res := range r.Results {
		if res.ID == id {
			return res, true
		}
	}
	return CheckResult{}, false
}

// Earned reports whether a tier was earned.
func (r *Report) Earned(t Tier) bool {
	for _, tr := range r.TierResults {
		if tr.Tier == t {
			return tr.Earned
		}
	}
	return false
}

// EarnedTiers returns the tiers earned, weakest first.
func (r *Report) EarnedTiers() []Tier {
	var out []Tier
	for _, tr := range r.TierResults {
		if tr.Earned {
			out = append(out, tr.Tier)
		}
	}
	return out
}

// Err returns a non-nil error when any check FAILED, listing each failure. A
// not-applicable is not an error — it is a tier that was not earned, which
// EarnedTiers reports. This is what a certification test asserts on.
func (r *Report) Err() error {
	var failures []error
	for _, res := range r.Results {
		if res.Status == StatusFail {
			failures = append(failures, fmt.Errorf("%s: %s", res.ID, res.Reason))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("harness %q failed %d conformance check(s): %w", r.Harness, len(failures), errors.Join(failures...))
}

// Run drives subject through every check and returns the report.
//
// The returned error is reserved for a malformed Subject — a missing
// provider, an unknown or unexplained NotApplicable entry. A harness that
// fails checks yields a nil error and a Report whose Err reports the
// failures; that split keeps "you configured the suite wrong" distinct from
// "your harness is not conformant".
func Run(ctx context.Context, subject Subject) (*Report, error) {
	if err := subject.validate(); err != nil {
		return nil, err
	}
	manifest := subject.Provider.Manifest()
	state := &run{subject: subject, manifest: manifest}

	report := &Report{
		Harness:     string(manifest.Name),
		ContractABI: manifest.ContractABI,
		Unverified:  UnverifiedClaims(manifest),
	}
	for _, check := range Checks() {
		report.Results = append(report.Results, state.evaluate(ctx, check))
	}
	report.TierResults = tierResults(report.Results)
	return report, nil
}

// evaluate runs one check, honoring a subject-declared not-applicable first.
func (r *run) evaluate(ctx context.Context, c Check) CheckResult {
	if reason, ok := r.subject.NotApplicable[c.ID]; ok {
		return newResult(c, StatusNotApplicable, "declared by harness author: "+reason, DeciderSubject)
	}
	status, reason := c.run(ctx, r)
	return newResult(c, status, reason, DeciderSuite)
}

func tierResults(results []CheckResult) []TierResult {
	out := make([]TierResult, 0, len(Tiers()))
	for _, tier := range Tiers() {
		tr := TierResult{Tier: tier, Earned: true}
		seen := false
		for _, res := range results {
			if res.Tier != tier {
				continue
			}
			seen = true
			if res.Status == StatusPass {
				continue
			}
			// A not-applicable never earns a tier: "we could not test it"
			// and "it works" must not render the same.
			if tr.Earned {
				tr.Earned = false
				tr.Reason = fmt.Sprintf("%s (%s): %s", res.ID, res.Status, res.Reason)
			}
		}
		if !seen {
			tr.Earned = false
			tr.Reason = "no checks registered for this tier"
		}
		out = append(out, tr)
	}
	return out
}
