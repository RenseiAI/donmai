package conformance

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestNewResultRefusesUnexplainedNotApplicable pins the honest-output rule at
// its narrowest point: the one constructor every emitted result passes
// through. A skip with no reason is not a skip — it is a failure — so a
// future check that forgets its reason cannot ship a silent pass.
func TestNewResultRefusesUnexplainedNotApplicable(t *testing.T) {
	t.Parallel()
	check := Check{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Row: 6}
	cases := []struct {
		name       string
		status     Status
		reason     string
		decider    Decider
		wantStatus Status
		wantReason string
	}{
		{
			name:   "not-applicable with no reason becomes a failure",
			status: StatusNotApplicable, reason: "", decider: DeciderSuite,
			wantStatus: StatusFail, wantReason: "without a declared reason",
		},
		{
			name:   "not-applicable with only whitespace becomes a failure",
			status: StatusNotApplicable, reason: "   \n\t ", decider: DeciderSuite,
			wantStatus: StatusFail, wantReason: "without a declared reason",
		},
		{
			name:   "not-applicable with a reason is preserved",
			status: StatusNotApplicable, reason: "manifest declares delivery none", decider: DeciderSuite,
			wantStatus: StatusNotApplicable, wantReason: "manifest declares delivery none",
		},
		{
			name:   "failure keeps its reason",
			status: StatusFail, reason: "inject dropped", decider: DeciderSuite,
			wantStatus: StatusFail, wantReason: "inject dropped",
		},
		{
			name:   "pass needs no reason",
			status: StatusPass, reason: "", decider: DeciderSuite,
			wantStatus: StatusPass, wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newResult(check, tc.status, tc.reason, tc.decider)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (reason %q)", got.Status, tc.wantStatus, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.wantReason)
			}
			if got.Status == StatusNotApplicable && got.Decider == "" {
				t.Fatal("a not-applicable result must record who decided it")
			}
			if got.Status != StatusNotApplicable && got.Decider != "" {
				t.Fatalf("decider %q set on a %s result", got.Decider, got.Status)
			}
		})
	}
}

// TestTierEarnedOnlyWhenEveryCheckPassed pins the second honest-output rule:
// a not-applicable never earns its tier, so "we could not test it" and "it
// works" cannot render the same.
func TestTierEarnedOnlyWhenEveryCheckPassed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		results    []CheckResult
		wantEarned bool
		wantReason string
	}{
		{
			name: "all pass earns the tier",
			results: []CheckResult{
				{ID: IDNoticeMechanism, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeBuildDrives, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Status: StatusPass},
			},
			wantEarned: true,
		},
		{
			name: "one failure denies the tier",
			results: []CheckResult{
				{ID: IDNoticeMechanism, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Status: StatusFail, Reason: "nonce never arrived"},
			},
			wantEarned: false,
			wantReason: "nonce never arrived",
		},
		{
			name: "a not-applicable denies the tier just as a failure does",
			results: []CheckResult{
				{ID: IDNoticeMechanism, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Status: StatusNotApplicable, Reason: "build does not drive it", Decider: DeciderSuite},
			},
			wantEarned: false,
			wantReason: "build does not drive it",
		},
		{
			name: "an author-declared not-applicable does not buy the tier either",
			results: []CheckResult{
				{ID: IDNoticeMechanism, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeBuildDrives, Tier: TierLiveNotice, Status: StatusPass},
				{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Status: StatusNotApplicable, Reason: "declared by harness author: no binary in CI", Decider: DeciderSubject},
			},
			wantEarned: false,
			wantReason: "no binary in CI",
		},
		{
			name:       "a tier with no results at all is not earned",
			results:    nil,
			wantEarned: false,
			wantReason: "no checks registered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tierResults(tc.results)
			var found *TierResult
			for i := range got {
				if got[i].Tier == TierLiveNotice {
					found = &got[i]
				}
			}
			if found == nil {
				t.Fatal("tierResults omitted the live-notice tier")
			}
			if found.Earned != tc.wantEarned {
				t.Fatalf("earned = %v, want %v (reason %q)", found.Earned, tc.wantEarned, found.Reason)
			}
			if !strings.Contains(found.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", found.Reason, tc.wantReason)
			}
		})
	}
}

func TestSubjectValidate(t *testing.T) {
	t.Parallel()
	provider := newFake(fakeConfig{})
	cases := []struct {
		name    string
		subject Subject
		wantErr string
	}{
		{
			name:    "complete subject",
			subject: Subject{Provider: provider, EchoPrompt: echoPrompt},
		},
		{
			name:    "missing provider",
			subject: Subject{EchoPrompt: echoPrompt},
			wantErr: "Subject.Provider is required",
		},
		{
			name:    "missing echo prompt",
			subject: Subject{Provider: provider},
			wantErr: "Subject.EchoPrompt is required",
		},
		{
			name: "not-applicable naming an unknown check",
			subject: Subject{
				Provider: provider, EchoPrompt: echoPrompt,
				NotApplicable: map[CheckID]string{"event/does-not-exist": "because"},
			},
			wantErr: "unknown check",
		},
		{
			name: "not-applicable with no reason is refused outright",
			subject: Subject{
				Provider: provider, EchoPrompt: echoPrompt,
				NotApplicable: map[CheckID]string{IDNoticeLiveDelivery: "  "},
			},
			wantErr: "declares no reason",
		},
		{
			name: "not-applicable with a reason is accepted",
			subject: Subject{
				Provider: provider, EchoPrompt: echoPrompt,
				NotApplicable: map[CheckID]string{IDNoticeLiveDelivery: "no harness binary on this runner"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.subject.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestReportErrCountsOnlyFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		results []CheckResult
		wantErr []string
	}{
		{
			name: "passes and explained skips are not errors",
			results: []CheckResult{
				{ID: IDSingleInit, Status: StatusPass},
				{ID: IDNoticeLiveDelivery, Status: StatusNotApplicable, Reason: "declares none", Decider: DeciderSuite},
			},
		},
		{
			name: "each failure is named",
			results: []CheckResult{
				{ID: IDSingleInit, Status: StatusFail, Reason: "no InitEvent"},
				{ID: IDNoticeLiveDelivery, Status: StatusFail, Reason: "nonce never arrived"},
			},
			wantErr: []string{"failed 2 conformance check", "event/single-init: no InitEvent", "notice/live-delivery: nonce never arrived"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := &Report{Harness: "fake", Results: tc.results}
			err := report.Err()
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Err() = nil, want error containing %v", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Err() = %v, missing %q", err, want)
				}
			}
		})
	}
}

// TestChecksRegistryIsWellFormed guards the registry itself: duplicate ids
// would make Subject.NotApplicable ambiguous, a tier with no checks would be
// awardable for free, and a check with no run function would pass silently.
func TestChecksRegistryIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[CheckID]bool{}
	perTier := map[Tier]int{}
	for _, c := range Checks() {
		if seen[c.ID] {
			t.Errorf("duplicate check id %q", c.ID)
		}
		seen[c.ID] = true
		if c.run == nil {
			t.Errorf("check %q has no run function, so it can never fail", c.ID)
		}
		if strings.TrimSpace(c.What) == "" {
			t.Errorf("check %q states no invariant", c.ID)
		}
		if c.Row == 0 {
			t.Errorf("check %q is not traceable to a checklist row", c.ID)
		}
		perTier[c.Tier]++
	}
	for _, tier := range Tiers() {
		if perTier[tier] == 0 {
			t.Errorf("tier %q has no checks, so it would be earned for free", tier)
		}
		if len(ChecksForTier(tier)) != perTier[tier] {
			t.Errorf("ChecksForTier(%q) disagrees with the registry", tier)
		}
	}
}

// TestUnverifiedClaimsNamesTheGaps pins the third honesty mechanism: every
// report states what it did NOT check, including the checklist rows this
// suite deliberately does not implement.
func TestUnverifiedClaimsNamesTheGaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		manifest agent.HarnessManifest
		wantRows []int
		wantText []string
	}{
		{
			name:     "a bare manifest still names the unconditional rows",
			manifest: agent.HarnessManifest{},
			wantRows: []int{1, 2, 3, 4, 5, 7, 8, 11},
			wantText: []string{"binary pin", "child conformance"},
		},
		{
			name:     "declaring subagent events adds the child claim",
			manifest: agent.HarnessManifest{Caps: agent.HarnessCaps{EmitsSubagentEvents: true}},
			wantText: []string{"subagent events are emitted"},
		},
		{
			name:     "declaring MCP adds the delivery-activation claim",
			manifest: agent.HarnessManifest{Caps: agent.HarnessCaps{AcceptsMcpServerSpec: true}},
			wantText: []string{"tool plugins / MCP server delivery"},
		},
		{
			name:     "declaring interactive PTY adds the mode-evidence claim",
			manifest: agent.HarnessManifest{Caps: agent.HarnessCaps{SupportsInteractivePTY: true}},
			wantText: []string{"interactive PTY spawn mode"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := UnverifiedClaims(tc.manifest)
			rows := map[int]bool{}
			var joined strings.Builder
			for _, claim := range claims {
				rows[claim.Row] = true
				joined.WriteString(claim.Claim)
				joined.WriteString("\n")
				if strings.TrimSpace(claim.Why) == "" {
					t.Errorf("claim %q states no reason", claim.Claim)
				}
			}
			for _, row := range tc.wantRows {
				if !rows[row] {
					t.Errorf("checklist row %d is unverified but unnamed", row)
				}
			}
			for _, want := range tc.wantText {
				if !strings.Contains(joined.String(), want) {
					t.Errorf("claims %q missing %q", joined.String(), want)
				}
			}
		})
	}
}

func TestReportTextShowsVerdictsAndGaps(t *testing.T) {
	t.Parallel()
	report := &Report{
		Harness:     "fake",
		ContractABI: "harness/v2",
		Results: []CheckResult{
			{ID: IDSingleInit, Tier: TierEventContract, Row: 6, Status: StatusPass},
			{ID: IDNoticeLiveDelivery, Tier: TierLiveNotice, Row: 6, Status: StatusNotApplicable, Reason: "declares none", Decider: DeciderSuite},
		},
		Unverified: UnverifiedClaims(agent.HarnessManifest{}),
	}
	report.TierResults = tierResults(report.Results)

	text := report.Text()
	for _, want := range []string{
		"harness: fake",
		"EARNED",
		"NOT EARNED",
		"N/A",
		"declares none",
		"[suite]",
		"NOT verified by this suite",
		"child conformance",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Text() missing %q:\n%s", want, text)
		}
	}
}
