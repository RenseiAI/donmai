package daemon

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
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
	// retry follows. The window runs past the loop's 1s wait floor:
	// anything shorter cannot tell "held" from "still waiting".
	select {
	case <-fired:
		t.Fatal("failed attempt retried immediately; retries must stay on the interval cadence")
	case <-time.After(1500 * time.Millisecond):
	}
	got := (&Daemon{quota: q}).quotaSnapshot()
	if len(got) != 1 || len(got[0].Limits.Windows) == 0 {
		t.Fatalf("snapshot = %+v, want the last good windows kept after a failed attempt", got)
	}
	if got[0].AuthCheck == nil || !got[0].AuthCheck.OK {
		t.Errorf("authCheck = %+v, want the previous ok:true (an unreached read records no verdict)", got[0].AuthCheck)
	}
}

// writeFakeMuseProbeCLI writes a stand-in muse binary for poller
// tests: it answers `--version` with versionLine, or exits non-zero
// when fail is set.
func writeFakeMuseProbeCLI(t *testing.T, versionLine string, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/muse"
	var script string
	if fail {
		script = "#!/bin/sh\nexit 3\n"
	} else {
		script = "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + versionLine + "'; exit 0; fi\nexit 3\n"
	}
	if err := testexec.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write fake muse probe CLI: %v", err)
	}
	return path
}

// TestQuotaPoller_MuseProbeToHeartbeatSnapshot is the production-path
// proof for the muse half of the wiring: the poller's muse probe runs
// a fake binary into the quota cache, and the heartbeat quota field
// the daemon sends carries the exact muse entry.
//
// It calls the probe func directly, so it cannot see the poller loop
// wiring (probeDue's muse case): TestQuotaPoller_MuseLoopPublishesEntry
// below owns that half, through the real Start/loop path.
func TestQuotaPoller_MuseProbeToHeartbeatSnapshot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		version string
		fail    bool
		wantOK  bool
	}{
		{name: "supported version is ok", version: "Muse Code 1.4.9 (1.4.9-R6297.1)", wantOK: true},
		{name: "too-old version is not ok", version: "Muse Code 1.4.2 (1.4.2-R5000.1)", wantOK: false},
		{name: "non-zero exit is not ok", fail: true, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessMuse: writeFakeMuseProbeCLI(t, tc.version, tc.fail)}})
			probes := d.quotaProbes()
			if probes[agent.UsageHarnessMuse] == nil {
				t.Fatal("no muse probe registered for a configured binary")
			}
			// Drive the production entry point (the poller probe
			// func), not the cache note, then read the heartbeat
			// field shape the daemon sends.
			probes[agent.UsageHarnessMuse](context.Background(), d.quota)
			got := d.quotaSnapshot()
			if len(got) != 1 {
				t.Fatalf("snapshot = %+v, want the one muse entry", got)
			}
			acct := got[0]
			if acct.ID != "muse-host-session" || acct.Provider != agent.UsageHarnessMuse {
				t.Errorf("account = %+v, want the host-session muse entry", acct)
			}
			if len(acct.Limits.Windows) != 0 {
				t.Errorf("limits = %+v, want no windows on a muse entry", acct.Limits)
			}
			if acct.AuthCheck == nil || acct.AuthCheck.Harness != agent.UsageHarnessMuse || acct.AuthCheck.OK != tc.wantOK {
				t.Errorf("authCheck = %+v, want ok:%v for muse", acct.AuthCheck, tc.wantOK)
			}
			if acct.AuthCheck != nil && (acct.AuthCheck.CheckedAt == "" || acct.AuthCheck.CheckedAt != acct.Limits.CheckedAt) {
				t.Errorf("authCheck = %+v, want the probe time on both", acct.AuthCheck)
			}
		})
	}
}

// TestQuotaPoller_MuseProbeMissingBinaryOmitsEntry pins the absent
// half: no configured binary means no probe and no muse entry at
// all, never a false verdict.
func TestQuotaPoller_MuseProbeMissingBinaryOmitsEntry(t *testing.T) {
	t.Parallel()

	d := New(Options{})
	if probes := d.quotaProbes(); probes[agent.UsageHarnessMuse] != nil {
		t.Fatal("muse probe registered without a configured binary")
	}
	if got := d.quotaSnapshot(); len(got) != 0 {
		t.Fatalf("snapshot = %+v, want no muse entry", got)
	}
	// A configured-but-gone binary records a refusal, not an entry
	// without a verdict: the probe ran and the login is not proven.
	dGone := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessMuse: "/nonexistent-muse-binary"}})
	probes := dGone.quotaProbes()
	if probes[agent.UsageHarnessMuse] == nil {
		t.Fatal("no muse probe registered for a configured binary")
	}
	probes[agent.UsageHarnessMuse](context.Background(), dGone.quota)
	got := dGone.quotaSnapshot()
	if len(got) != 1 || got[0].AuthCheck == nil || got[0].AuthCheck.OK {
		t.Fatalf("snapshot = %+v, want one muse entry with ok:false", got)
	}
}

// TestQuotaPoller_MuseLoopPublishesEntry pins the production poller
// loop wiring for muse: it starts the real quota poller (Start/loop,
// not the probe func directly) with a fake muse binary and waits for
// the muse entry to land on the heartbeat snapshot. Deleting the
// probeDue muse case (daemon/quota_poller.go) leaves every other
// muse test green while the entry never appears, so this test owns
// that case.
func TestQuotaPoller_MuseLoopPublishesEntry(t *testing.T) {
	t.Parallel()

	bin := writeFakeMuseProbeCLI(t, "Muse Code 1.4.9 (1.4.9-R6297.1)", false)
	d := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessMuse: bin}})
	poller := newQuotaPoller(d.quota, d.quotaProbes())
	poller.Start()
	t.Cleanup(poller.Stop)

	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, acct := range d.quotaSnapshot() {
			if acct.Provider != agent.UsageHarnessMuse {
				continue
			}
			if acct.ID != "muse-host-session" {
				t.Fatalf("muse account = %+v, want the host-session entry", acct)
			}
			if acct.AuthCheck == nil || !acct.AuthCheck.OK || acct.AuthCheck.Harness != agent.UsageHarnessMuse {
				t.Fatalf("muse authCheck = %+v, want ok:true for muse", acct.AuthCheck)
			}
			if len(acct.Limits.Windows) != 0 {
				t.Fatalf("muse limits = %+v, want no windows on a muse entry", acct.Limits)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("poller loop never published the muse entry: %+v", d.quotaSnapshot())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestQuotaProbeDueAt_MuseReadsTheMuseClock pins the explicit muse
// case: the muse next-due time follows the muse probe clock, never
// the claude one.
func TestQuotaProbeDueAt_MuseReadsTheMuseClock(t *testing.T) {
	t.Parallel()

	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteClaudeProbe("opaque-2", agent.MakeUsageLimits(agent.ISOTime(base), nil), base)
	if due := q.quotaProbeDueAt(agent.UsageHarnessMuse, base); !due.IsZero() {
		t.Fatalf("muse due = %v, want due now with no muse probe yet", due)
	}
	q.noteMuseProbe("muse-host-session", agent.MakeUsageLimits(agent.ISOTime(base), nil), base)
	if due := q.quotaProbeDueAt(agent.UsageHarnessMuse, base); !due.Equal(base.Add(providercodex.RateLimitsProbeInterval)) {
		t.Errorf("muse due = %v, want one interval past the muse probe", due)
	}
	if due := q.quotaProbeDueAt(agent.UsageHarnessMuse, base.Add(providercodex.RateLimitsProbeInterval)); !due.IsZero() {
		t.Errorf("muse due = %v, want due at the interval", due)
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
