package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
)

// quotaPollerFixtureCodex feeds the poller from the recorded codex
// promax read under provider testdata. Values are synthetic; the
// mapping rules they pin come from the MIT-licensed t3code provider
// usage-limits modules. The fixture maps through the production codex
// mapper so the rows carry the probe-stable window ids the stream
// updates merge onto.
func quotaPollerFixtureCodex(t *testing.T, at time.Time) (plan string, probed agent.UsageLimits) {
	t.Helper()
	raw, err := os.ReadFile("../provider/harness/codex/testdata/t3code/promax_rate_limits_read.json")
	if err != nil {
		t.Fatalf("read codex fixture: %v", err)
	}
	var decoded struct {
		RateLimits          *providercodex.RateLimitSnapshot            `json:"rateLimits"`
		RateLimitsByLimitID map[string]*providercodex.RateLimitSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode codex fixture: %v", err)
	}
	limits := providercodex.RateLimitsToLimits(decoded.RateLimits, decoded.RateLimitsByLimitID, nil, agent.ISOTime(at))
	if decoded.RateLimits != nil && decoded.RateLimits.PlanType != nil {
		plan = *decoded.RateLimits.PlanType
	}
	return plan, limits
}

// quotaPollerFixtureClaude feeds the poller from the recorded claude
// usage read under provider testdata, through the production claude
// mapper.
func quotaPollerFixtureClaude(t *testing.T, at time.Time) agent.UsageLimits {
	t.Helper()
	raw, err := os.ReadFile("../provider/harness/claude/testdata/t3code/usage_read.json")
	if err != nil {
		t.Fatalf("read claude fixture: %v", err)
	}
	limits, _ := providerclaude.UsageResponseToLimits(raw, agent.ISOTime(at))
	return limits
}

// TestQuotaPoller_ProbesDueImmediatelyOnStart drives the production
// poller entry point (Start): with both harnesses due, the first
// attempts fire within about a second of Start (the loop floors its
// first wait), and the heartbeat snapshot carries quota
// windows with an ok:true login check for each — the recorded-fixture
// proof behind the "within one probe interval" acceptance.
func TestQuotaPoller_ProbesDueImmediatelyOnStart(t *testing.T) {
	t.Parallel()

	q := &quotaState{}
	var codexCalls, claudeCalls atomic.Int32
	probed := make(chan struct{}, 4)
	poller := newQuotaPoller(q, map[string]quotaProbeFunc{
		agent.UsageHarnessCodex: func(_ context.Context, q *quotaState) {
			codexCalls.Add(1)
			now := time.Now()
			plan, limits := quotaPollerFixtureCodex(t, now)
			q.noteCodexProbe("fixture-codex-account", plan, limits, now)
			probed <- struct{}{}
		},
		agent.UsageHarnessClaude: func(_ context.Context, q *quotaState) {
			claudeCalls.Add(1)
			now := time.Now()
			q.noteClaudeProbe("fixture-claude-account", quotaPollerFixtureClaude(t, now), now)
			probed <- struct{}{}
		},
	})
	poller.Start()
	t.Cleanup(poller.Stop)

	for i := 0; i < 2; i++ {
		select {
		case <-probed:
		case <-time.After(5 * time.Second):
			t.Fatalf("probe %d never fired on Start (both harnesses are due on a fresh cache)", i+1)
		}
	}
	if got := codexCalls.Load(); got != 1 {
		t.Errorf("codex probe calls = %d, want exactly 1 (one attempt per interval)", got)
	}
	if got := claudeCalls.Load(); got != 1 {
		t.Errorf("claude probe calls = %d, want exactly 1 (one attempt per interval)", got)
	}

	snapshot := (&Daemon{quota: q}).quotaSnapshot()
	byProvider := map[string]agent.UsageAccount{}
	for _, acct := range snapshot {
		byProvider[acct.Provider] = acct
	}
	for _, harness := range []string{agent.UsageHarnessCodex, agent.UsageHarnessClaude} {
		acct, ok := byProvider[harness]
		if !ok {
			t.Fatalf("snapshot has no %s entry: %+v", harness, snapshot)
		}
		if len(acct.Limits.Windows) == 0 {
			t.Errorf("%s entry carries no windows", harness)
		}
		if acct.AuthCheck == nil || !acct.AuthCheck.OK || acct.AuthCheck.Harness != harness {
			t.Errorf("%s authCheck = %+v, want ok:true for %s", harness, acct.AuthCheck, harness)
		}
	}
	if byProvider[agent.UsageHarnessCodex].Plan != "promax" {
		t.Errorf("codex plan = %q, want promax from the fixture", byProvider[agent.UsageHarnessCodex].Plan)
	}
}

// TestQuotaPoller_SkipsHarnessInsideInterval pins the 5-minute
// cadence through the production loop: after a fresh attempt, the
// poller fires no second attempt for that harness inside the
// interval.
func TestQuotaPoller_SkipsHarnessInsideInterval(t *testing.T) {
	t.Parallel()

	q := &quotaState{}
	now := time.Now()
	plan, limits := quotaPollerFixtureCodex(t, now)
	q.noteCodexProbe("fixture-codex-account", plan, limits, now)

	var calls atomic.Int32
	poller := newQuotaPoller(q, map[string]quotaProbeFunc{
		agent.UsageHarnessCodex: func(_ context.Context, _ *quotaState) {
			calls.Add(1)
		},
	})
	poller.Start()
	t.Cleanup(poller.Stop)

	time.Sleep(300 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("codex probe calls = %d, want 0 inside the interval after a fresh attempt", got)
	}
}

// TestQuotaPoller_FailedAttemptStillAdvancesClock feeds a failed
// attempt through the production loop and pins the retry gate: the
// next attempt for that harness stays a full interval out, while the
// last good windows stay published with no new verdict.
func TestQuotaPoller_FailedAttemptStillAdvancesClock(t *testing.T) {
	t.Parallel()

	q := &quotaState{}
	base := time.Now()
	plan, limits := quotaPollerFixtureCodex(t, base)
	q.noteCodexProbe("fixture-codex-account", plan, limits, base)

	fired := make(chan struct{}, 1)
	poller := newQuotaPoller(q, map[string]quotaProbeFunc{
		agent.UsageHarnessCodex: func(_ context.Context, q *quotaState) {
			attempt := time.Now()
			q.noteCodexProbe("fixture-codex-account", plan,
				agent.MakeUnavailableUsageLimits(agent.ISOTime(attempt), agent.UsageUnavailableProbeFailed, "unreachable"),
				attempt)
			fired <- struct{}{}
		},
	})
	// Force the attempt due by rewinding the test clock past the
	// interval: the poller must fire once, then hold.
	q.mu.Lock()
	q.codexAt = base.Add(-providercodex.RateLimitsProbeInterval - time.Second)
	q.mu.Unlock()
	poller.Start()
	t.Cleanup(poller.Stop)

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("failed probe attempt never fired")
	}
	// One attempt only: the failed attempt advanced the clock, so no
	// immediate retry follows.
	select {
	case <-fired:
		t.Fatal("failed attempt retried immediately; retries must stay on the interval cadence")
	case <-time.After(300 * time.Millisecond):
	}
	got := (&Daemon{quota: q}).quotaSnapshot()
	if len(got) != 1 || len(got[0].Limits.Windows) == 0 {
		t.Fatalf("snapshot = %+v, want the last good windows kept after a failed attempt", got)
	}
	if got[0].AuthCheck == nil || !got[0].AuthCheck.OK {
		t.Errorf("authCheck = %+v, want the previous ok:true (an unreached read records no verdict)", got[0].AuthCheck)
	}
}

// TestQuotaProbeBounds pins the timing contract the consumer relies
// on: one probe attempt never runs longer than the per-attempt
// ceiling, the probe interval stays below that ceiling's reach, and
// the interval stays below the consumer freshness TTL, so a fresh
// ok:true always lands before the previous verdict ages out.
func TestQuotaProbeBounds(t *testing.T) {
	t.Parallel()

	if quotaProbeTimeout >= providercodex.RateLimitsProbeInterval {
		t.Errorf("probe timeout %v must stay below the probe interval %v", quotaProbeTimeout, providercodex.RateLimitsProbeInterval)
	}
	if providercodex.RateLimitsProbeInterval >= quotaConsumerFreshnessTTL {
		t.Errorf("probe interval %v must stay below the consumer freshness TTL %v", providercodex.RateLimitsProbeInterval, quotaConsumerFreshnessTTL)
	}
	if providerclaude.UsageProbeTimeout > quotaProbeTimeout {
		t.Errorf("claude usage read ceiling %v must stay within the probe attempt ceiling %v", providerclaude.UsageProbeTimeout, quotaProbeTimeout)
	}
}
