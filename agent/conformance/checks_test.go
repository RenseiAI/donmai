package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// runSubject runs the suite and fails the test on a Subject-shape error,
// which is never what these cases are about.
func runSubject(t *testing.T, s Subject) *Report {
	t.Helper()
	report, err := Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run() returned a subject error: %v", err)
	}
	return report
}

func mustResult(t *testing.T, r *Report, id CheckID) CheckResult {
	t.Helper()
	res, ok := r.Result(id)
	if !ok {
		t.Fatalf("report has no result for %q", id)
	}
	return res
}

// TestRunConformantHarnessEarnsTiers is the positive control: a fake that
// honors the contract earns the tiers it actually demonstrates.
func TestRunConformantHarnessEarnsTiers(t *testing.T) {
	t.Parallel()
	subject := conformantSubject(fakeConfig{
		notice:        agent.NoticeDeliveryHook,
		supportInject: true,
		supportResume: true,
		inject:        injectDeliver,
	})
	report := runSubject(t, subject)

	if err := report.Err(); err != nil {
		t.Fatalf("a conformant harness failed checks: %v\n%s", err, report.Text())
	}
	for _, tier := range []Tier{TierEventContract, TierLiveNotice, TierResume} {
		if !report.Earned(tier) {
			t.Errorf("tier %q not earned by a conformant harness\n%s", tier, report.Text())
		}
	}
	// The row-10 tier has no fixture here, so it must NOT be earned — the
	// absence of evidence is never evidence.
	if report.Earned(TierAdaptationReceipt) {
		t.Errorf("adaptation-receipt tier earned with no fixture supplied\n%s", report.Text())
	}
	if res := mustResult(t, report, IDReceiptPlanValid); res.Status != StatusNotApplicable {
		t.Errorf("receipt/plan-valid = %q with no fixture, want not_applicable", res.Status)
	}
}

// TestRunRejectsNonConformantHarness is the red proof for every live check:
// each row is an adapter broken in exactly one way, and names the check that
// must catch it.
func TestRunRejectsNonConformantHarness(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		cfg          fakeConfig
		wantFail     CheckID
		wantReason   string
		deniedTier   Tier
		probeTimeout time.Duration
	}{
		{
			name:       "no init event",
			cfg:        fakeConfig{script: noInitScript},
			wantFail:   IDSingleInit,
			wantReason: "no InitEvent",
			deniedTier: TierEventContract,
		},
		{
			name:       "a spurious error after the terminal result",
			cfg:        fakeConfig{script: twoTerminalScript},
			wantFail:   IDTerminalContract,
			wantReason: "2 terminal events",
			deniedTier: TierEventContract,
		},
		{
			name:       "per-token assistant streaming",
			cfg:        fakeConfig{script: perTokenScript},
			wantFail:   IDCompleteText,
			wantReason: "per-token streaming",
			deniedTier: TierEventContract,
		},
		{
			name:         "the events channel never closes",
			cfg:          fakeConfig{holdOpen: true},
			wantFail:     IDChannelCloses,
			wantReason:   "had not closed",
			deniedTier:   TierEventContract,
			probeTimeout: 300 * time.Millisecond,
		},
		{
			name:       "Stop after close reports an error",
			cfg:        fakeConfig{stopErr: errors.New("already gone")},
			wantFail:   IDStopIdempotent,
			wantReason: "idempotent",
			deniedTier: TierEventContract,
		},
		{
			name:       "the harness cannot be spawned at all",
			cfg:        fakeConfig{spawnErr: errFakeSpawn},
			wantFail:   IDSingleInit,
			wantReason: "spawn failed",
			deniedTier: TierEventContract,
		},
		{
			name:       "the notice-delivery axis is unanswered",
			cfg:        fakeConfig{undeclaredNotice: true},
			wantFail:   IDNoticeMechanism,
			wantReason: "not one of the known mechanisms",
			deniedTier: TierLiveNotice,
		},
		{
			name:       "an injected notice is accepted and dropped",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryHook, supportInject: true, inject: injectDrop},
			wantFail:   IDNoticeLiveDelivery,
			wantReason: "never appeared",
			deniedTier: TierLiveNotice,
		},
		{
			name:       "the declared notice channel refuses the message",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryRPCSteer, supportInject: true, inject: injectUnsupported},
			wantFail:   IDNoticeLiveDelivery,
			wantReason: "Handle.Inject returned",
			deniedTier: TierLiveNotice,
		},
		{
			name:       "resume is declared but refused",
			cfg:        fakeConfig{supportResume: true, resumeErr: agent.ErrUnsupported},
			wantFail:   IDResumeContinues,
			wantReason: "Resume(",
			deniedTier: TierResume,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			subject := conformantSubject(tc.cfg)
			if tc.probeTimeout > 0 {
				subject.ProbeTimeout = tc.probeTimeout
			}
			report := runSubject(t, subject)

			res := mustResult(t, report, tc.wantFail)
			if res.Status != StatusFail {
				t.Fatalf("%s = %q, want fail\n%s", tc.wantFail, res.Status, report.Text())
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("%s reason = %q, want it to contain %q", tc.wantFail, res.Reason, tc.wantReason)
			}
			if report.Earned(tc.deniedTier) {
				t.Errorf("tier %q earned by a harness that fails %s\n%s", tc.deniedTier, tc.wantFail, report.Text())
			}
			if report.Err() == nil {
				t.Error("Report.Err() = nil for a harness with a failing check")
			}
		})
	}
}

// TestDeclaredNoticeDeliveryEarnsNothingWithoutDelivery is the case this
// tier was built for. Ten manifests declare a notice-delivery mechanism; one
// build drives it. Both shapes of that gap must deny the tier, and neither
// may render as a pass.
func TestDeclaredNoticeDeliveryEarnsNothingWithoutDelivery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfg        fakeConfig
		wantStatus Status
		wantReason string
	}{
		{
			name:       "declared, driven, delivered: the tier is earned",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryHook, supportInject: true, inject: injectDeliver},
			wantStatus: StatusPass,
		},
		{
			name:       "declared and driven but silently dropped: failure",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryHTTPSession, supportInject: true, inject: injectDrop},
			wantStatus: StatusFail,
			wantReason: "never appeared",
		},
		{
			name:       "declared but this build does not drive it: unproven, never a pass",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryMCPRPC, supportInject: false},
			wantStatus: StatusNotApplicable,
			wantReason: "does not drive it",
		},
		{
			name:       "declared none: an honest answer with nothing to certify",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryNone},
			wantStatus: StatusNotApplicable,
			wantReason: "does not deliver into a running process",
		},
		{
			name:       "declared resume-inject: real delivery, but not into a live process",
			cfg:        fakeConfig{notice: agent.NoticeDeliveryResumeInject},
			wantStatus: StatusNotApplicable,
			wantReason: "does not deliver into a running process",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := runSubject(t, conformantSubject(tc.cfg))
			res := mustResult(t, report, IDNoticeLiveDelivery)
			if res.Status != tc.wantStatus {
				t.Fatalf("notice/live-delivery = %q, want %q (reason %q)", res.Status, tc.wantStatus, res.Reason)
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			wantTier := tc.wantStatus == StatusPass
			if report.Earned(TierLiveNotice) != wantTier {
				t.Errorf("live-notice earned = %v, want %v\n%s", report.Earned(TierLiveNotice), wantTier, report.Text())
			}
			// A declaration that is merely known must never, on its own,
			// carry the tier.
			if res.Status != StatusPass && report.Earned(TierLiveNotice) {
				t.Error("a declared-but-unproven channel earned the live-notice tier")
			}
		})
	}
}

// TestSubjectDeclaredSkipNeverEarnsATier proves an author cannot buy a tier
// by declaring a check inapplicable, however good the stated reason.
func TestSubjectDeclaredSkipNeverEarnsATier(t *testing.T) {
	t.Parallel()
	subject := conformantSubject(fakeConfig{
		notice: agent.NoticeDeliveryHook, supportInject: true, inject: injectDrop,
	})
	subject.NotApplicable = map[CheckID]string{
		IDNoticeLiveDelivery: "our harness binary is not installable on this runner",
	}
	report := runSubject(t, subject)

	res := mustResult(t, report, IDNoticeLiveDelivery)
	if res.Status != StatusNotApplicable {
		t.Fatalf("status = %q, want not_applicable", res.Status)
	}
	if res.Decider != DeciderSubject {
		t.Errorf("decider = %q, want %q so a reader can see the skip was requested", res.Decider, DeciderSubject)
	}
	if !strings.Contains(res.Reason, "declared by harness author") {
		t.Errorf("reason = %q, want it to attribute the skip", res.Reason)
	}
	if report.Earned(TierLiveNotice) {
		t.Errorf("a declared skip bought the live-notice tier\n%s", report.Text())
	}
	// The skip suppresses the failure but not the verdict: Err is clean,
	// EarnedTiers is where the truth lives.
	for _, tier := range report.EarnedTiers() {
		if tier == TierLiveNotice {
			t.Error("EarnedTiers reported live-notice after a declared skip")
		}
	}
}

func TestRunRejectsMalformedSubject(t *testing.T) {
	t.Parallel()
	if _, err := Run(context.Background(), Subject{EchoPrompt: echoPrompt}); err == nil {
		t.Fatal("Run() accepted a Subject with no provider")
	}
	bad := Subject{
		Provider:      newFake(fakeConfig{}),
		EchoPrompt:    echoPrompt,
		NotApplicable: map[CheckID]string{IDSingleInit: ""},
	}
	if _, err := Run(context.Background(), bad); err == nil {
		t.Fatal("Run() accepted an unexplained not-applicable")
	}
}
