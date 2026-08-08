package conformance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestNoticeRailIsAssignedForEveryDeclaredMechanism pins the rail table
// against the agent package's declarable set. A mechanism with no rail cannot
// be judged, and the suite must say so loudly rather than fall through to a
// default — a default is how a channel nothing drives ends up certified.
func TestNoticeRailIsAssignedForEveryDeclaredMechanism(t *testing.T) {
	t.Parallel()
	declared := []agent.NoticeDelivery{
		agent.NoticeDeliveryNone,
		agent.NoticeDeliveryHook,
		agent.NoticeDeliveryMCPRPC,
		agent.NoticeDeliveryHTTPSession,
		agent.NoticeDeliveryACP,
		agent.NoticeDeliveryRPCSteer,
		agent.NoticeDeliveryResumeInject,
		agent.NoticeDeliveryInBoxLoop,
		agent.NoticeDeliveryPTYNotice,
	}
	for _, nd := range declared {
		if !nd.Declared() {
			t.Errorf("%q is listed here but agent no longer declares it", nd)
		}
		if _, ok := noticeRails[nd]; !ok {
			t.Errorf("mechanism %q has no rail; the suite cannot judge a channel it has not placed", nd)
		}
	}
	for nd := range noticeRails {
		if !nd.Declared() {
			t.Errorf("rail table names %q, which agent does not declare", nd)
		}
	}
	// An undeclared mechanism must fail the check rather than fall through.
	r := &run{
		subject:  conformantSubject(fakeConfig{}),
		manifest: agent.HarnessManifest{Caps: agent.HarnessCaps{NoticeDelivery: agent.NoticeDelivery("carrier-pigeon")}},
	}
	if status, _ := checkNoticeBuildDrives(context.Background(), r); status != StatusFail {
		t.Errorf("an unknown mechanism gave %q, want fail", status)
	}
}

// TestHookChannelIsNotCertifiedByTheInjectRail is the case the suite inverted.
//
// This is the claude shape exactly: the manifest declares
// NoticeDeliveryHook, and Capabilities().SupportsMessageInjection is true —
// but it is true because of a DIFFERENT channel (claude's Handle.Inject is a
// `claude --resume` invocation, i.e. resume-inject). Nothing in this build
// drives the Stop hook. Reading the Inject rail's flag and crediting the hook
// channel is how the suite awarded live-notice to the one channel it was
// built to catch.
func TestHookChannelIsNotCertifiedByTheInjectRail(t *testing.T) {
	t.Parallel()
	report := runSubject(t, conformantSubject(fakeConfig{
		notice:        agent.NoticeDeliveryHook,
		supportInject: true,
		inject:        injectDeliver,
	}))

	drives := mustResult(t, report, IDNoticeBuildDrives)
	if drives.Status == StatusPass {
		t.Errorf("notice/build-drives-channel passed for the hook channel on the strength of the Handle.Inject rail\n%s", report.Text())
	}
	for _, want := range []string{"hook", "Handle.Inject"} {
		if !strings.Contains(drives.Reason, want) {
			t.Errorf("build-drives reason = %q, want it to name %q", drives.Reason, want)
		}
	}

	delivery := mustResult(t, report, IDNoticeLiveDelivery)
	if delivery.Status == StatusPass {
		t.Errorf("notice/live-delivery passed for a channel this build does not drive\n%s", report.Text())
	}
	if report.Earned(TierLiveNotice) {
		t.Errorf("the live-notice tier was EARNED for the hook channel\n%s", report.Text())
	}
}

// TestInjectRailChannelsStillCertify guards the other direction: the fix must
// not deny the channels Handle.Inject genuinely carries.
func TestInjectRailChannelsStillCertify(t *testing.T) {
	t.Parallel()
	for _, nd := range []agent.NoticeDelivery{
		agent.NoticeDeliveryMCPRPC,
		agent.NoticeDeliveryHTTPSession,
		agent.NoticeDeliveryRPCSteer,
		agent.NoticeDeliveryInBoxLoop,
		agent.NoticeDeliveryACP,
	} {
		t.Run(string(nd), func(t *testing.T) {
			t.Parallel()
			report := runSubject(t, conformantSubject(fakeConfig{
				notice: nd, supportInject: true, inject: injectDeliver,
			}))
			if !report.Earned(TierLiveNotice) {
				t.Errorf("live-notice not earned by a harness that delivered over %q\n%s", nd, report.Text())
			}
		})
	}
}

// TestPTYNoticeRailIsCertifiable is the other half of the inversion:
// pty-notice is the ONE live-notice mechanism this build actually implements
// (runner/interactive_inject.go), and it was excluded from the certifiable
// set, so the only working implementation in the tree could never earn the
// tier while an unimplemented channel could.
func TestPTYNoticeRailIsCertifiable(t *testing.T) {
	t.Parallel()
	report := runSubject(t, ptySubject(fakePTYConfig{mode: ptyDeliver}))

	if res := mustResult(t, report, IDNoticeBuildDrives); res.Status != StatusPass {
		t.Errorf("notice/build-drives-channel = %q for pty-notice, want pass (reason %q)\n%s", res.Status, res.Reason, report.Text())
	}
	if res := mustResult(t, report, IDNoticeLiveDelivery); res.Status != StatusPass {
		t.Errorf("notice/live-delivery = %q for a terminal that took the notice, want pass (reason %q)\n%s", res.Status, res.Reason, report.Text())
	}
	if !report.Earned(TierLiveNotice) {
		t.Errorf("live-notice not earned by the one mechanism this build drives\n%s", report.Text())
	}
}

// TestPTYNoticeRailRejectsEveryNonDelivery is the red proof for the terminal
// rail: each row is a surface broken in exactly one way, and none may pass.
func TestPTYNoticeRailRejectsEveryNonDelivery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfg        fakePTYConfig
		wantReason string
		// timeout overrides the probe budget. The refusal case needs one that
		// outlasts ptyNoticeMaxAttempts*ptyNoticeRetry, or it lands on the
		// budget-expired branch instead of the exhausted-attempts one.
		timeout time.Duration
	}{
		{
			name:       "the line is echoed but the session never acts on it",
			cfg:        fakePTYConfig{mode: ptyEchoOnly},
			wantReason: "echoed",
		},
		{
			name:       "the surface refuses every attempt",
			cfg:        fakePTYConfig{mode: ptyRefuse},
			wantReason: "refused the notice on all 20 attempts",
			timeout:    2*ptyNoticeMaxAttempts*ptyNoticeRetry + time.Second,
		},
		{
			name:       "the surface is still refusing when the budget runs out",
			cfg:        fakePTYConfig{mode: ptyRefuse},
			wantReason: "probe budget",
			timeout:    500 * time.Millisecond,
		},
		{
			name:       "the write fails outright",
			cfg:        fakePTYConfig{mode: ptyWriteErr},
			wantReason: "write failed",
		},
		{
			name:       "the live surface cannot accept notices",
			cfg:        fakePTYConfig{mode: ptyNotNotifier},
			wantReason: "TryWriteNotice",
		},
		{
			name:       "the harness declares pty-notice but spawns no terminal",
			cfg:        fakePTYConfig{mode: ptyNotInteractive},
			wantReason: "no live terminal",
		},
		{
			name:       "the session cannot be spawned at all",
			cfg:        fakePTYConfig{mode: ptyDeliver, spawnErr: errFakeSpawn},
			wantReason: "spawn failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			subject := ptySubject(tc.cfg)
			subject.ProbeTimeout = 2 * time.Second
			if tc.timeout > 0 {
				subject.ProbeTimeout = tc.timeout
			}
			report := runSubject(t, subject)

			res := mustResult(t, report, IDNoticeLiveDelivery)
			if res.Status == StatusPass {
				t.Fatalf("notice/live-delivery passed\n%s", report.Text())
			}
			if !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if report.Earned(TierLiveNotice) {
				t.Errorf("live-notice earned\n%s", report.Text())
			}
		})
	}
}

// TestInjectResultSurvivesTheDrain covers the race the probe shipped with: the
// injecting goroutine's result was read with a non-blocking select the instant
// the drain returned, so an Inject that outlived the drain had its outcome
// discarded and the failure was reported as "the session never emitted an
// InitEvent" — a reason that is simply false.
func TestInjectResultSurvivesTheDrain(t *testing.T) {
	t.Parallel()
	subject := conformantSubject(fakeConfig{
		notice: agent.NoticeDeliveryRPCSteer, supportInject: true, inject: injectSlowUnsupported,
	})
	report := runSubject(t, subject)

	res := mustResult(t, report, IDNoticeLiveDelivery)
	if res.Status != StatusFail {
		t.Fatalf("notice/live-delivery = %q, want fail\n%s", res.Status, report.Text())
	}
	if strings.Contains(res.Reason, "never emitted an InitEvent") {
		t.Errorf("the probe blamed a missing InitEvent for an Inject that did return: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "Handle.Inject returned") {
		t.Errorf("reason = %q, want the injecting goroutine's own error", res.Reason)
	}
}

// TestInjectThatNeverReturnsIsReportedNotAssumed covers the same seam's other
// end: an Inject that never returns at all must be named, not silently read as
// "no injection happened".
func TestInjectThatNeverReturnsIsReportedNotAssumed(t *testing.T) {
	t.Parallel()
	subject := conformantSubject(fakeConfig{
		notice: agent.NoticeDeliveryRPCSteer, supportInject: true, inject: injectHang,
	})
	report := runSubject(t, subject)

	res := mustResult(t, report, IDNoticeLiveDelivery)
	if res.Status != StatusFail {
		t.Fatalf("notice/live-delivery = %q, want fail\n%s", res.Status, report.Text())
	}
	if !strings.Contains(res.Reason, "had not returned") {
		t.Errorf("reason = %q, want it to name the stalled Inject", res.Reason)
	}
}

// TestUnverifiedNamesTheChannelAttributionGap pins the blind spot at the
// centre of this tier. Report.Unverified's whole job is naming what was not
// checked; the thing it must name first is that a nonce coming back out of the
// event stream does not say which channel carried it.
func TestUnverifiedNamesTheChannelAttributionGap(t *testing.T) {
	t.Parallel()
	for _, manifest := range []agent.HarnessManifest{
		{},
		{Caps: agent.HarnessCaps{NoticeDelivery: agent.NoticeDeliveryHook, SupportsMessageInjection: true}},
		{Caps: agent.HarnessCaps{NoticeDelivery: agent.NoticeDeliveryNone}},
	} {
		var found *Claim
		claims := UnverifiedClaims(manifest)
		for i := range claims {
			if strings.Contains(strings.ToLower(claims[i].Claim), "declared notice channel") {
				found = &claims[i]
			}
		}
		if found == nil {
			t.Fatalf("no unverified claim names the channel-attribution gap for %q", manifest.Caps.NoticeDelivery)
		}
		for _, want := range []string{"event stream", "which channel"} {
			if !strings.Contains(found.Why, want) {
				t.Errorf("why = %q, want it to contain %q", found.Why, want)
			}
		}
	}
}
