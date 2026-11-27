package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	snapshot := (&Daemon{quota: q}).QuotaSnapshot()
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
	got := (&Daemon{quota: q}).QuotaSnapshot()
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
			got := d.QuotaSnapshot()
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
	if got := d.QuotaSnapshot(); len(got) != 0 {
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
	got := dGone.QuotaSnapshot()
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
		for _, acct := range d.QuotaSnapshot() {
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
			t.Fatalf("poller loop never published the muse entry: %+v", d.QuotaSnapshot())
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

func writeClaudeSlotCLI(t *testing.T, usageLog string) string {
	t.Helper()
	t.Setenv("USER", "probe-operator")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" auth status --json \"*)\n" +
		"case \"$CLAUDE_CONFIG_DIR\" in *hang*) (sleep 1.5; touch \"" + usageLog + ".survived\") & sleep 30; exit 0;; *home-a*) printf '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"m\":\"a\"}\\n'; exit 0;; *home-b*|*exhausted*) printf '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"m\":\"b\"}\\n'; exit 0;; *) printf '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"m\":\"d\"}\\n'; exit 0;; esac;;\n" +
		"esac\necho \"usage $CLAUDE_CONFIG_DIR\" >> \"" + usageLog + "\"\n" +
		"case \"$CLAUDE_CONFIG_DIR\" in *hang*) (sleep 1.5; touch \"" + usageLog + ".survived\") & sleep 30; exit 0;; *home-a*) u='{\"five_hour\":{\"utilization\":11,\"resets_at\":\"2030-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":12,\"resets_at\":\"2030-01-01T00:00:00Z\"}}';; *exhausted*) u='{\"five_hour\":{\"utilization\":100,\"resets_at\":\"2030-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":100,\"resets_at\":\"2030-01-01T00:00:00Z\"}}';; *home-b*) u='{\"five_hour\":{\"utilization\":33,\"resets_at\":\"2030-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":34,\"resets_at\":\"2030-01-01T00:00:00Z\"}}';; *) u='{\"five_hour\":{\"utilization\":10,\"resets_at\":\"2030-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":20,\"resets_at\":\"2030-01-01T00:00:00Z\"}}';; esac\n" +
		"printf '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"response\":{\"rate_limits_available\":true,\"rate_limits\":%s}}}\\n' \"$u\"\n"
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := testexec.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write slot CLI: %v", err)
	}
	return path
}

func slotUsageLines(t *testing.T, log string) []string {
	t.Helper()
	raw, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func driveClaudeScan(t *testing.T, bin, def string, extras []string) *Daemon {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	d := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessClaude: bin}})
	d.config = &Config{QuotaProbeHomes: map[string][]string{agent.UsageHarnessClaude: extras}}
	newQuotaPoller(d.quota, d.quotaProbes()).probeDue(context.Background())
	return d
}

func TestQuotaPoller_ClaudeSlotsPairAccountsAndPreserveHeartbeat(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "def")
	a := filepath.Join(root, "home-a")
	b := filepath.Join(root, "home-b")
	exh := filepath.Join(root, "exhausted")
	for _, d := range []string{def, a, b, exh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	usageLog := filepath.Join(root, "u.log")
	bin := writeClaudeSlotCLI(t, usageLog)
	cfgPath := filepath.Join(root, "daemon.yaml")
	if err := WriteConfig(cfgPath, &Config{Machine: MachineConfig{ID: "m"}, Orchestrator: OrchestratorConfig{URL: "https://example.test"}, QuotaProbeHomes: map[string][]string{"claude": {a, b}}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil || len(loaded.QuotaProbeHomes["claude"]) != 2 {
		t.Fatalf("load homes = %v, %v", loaded, err)
	}
	d1 := driveClaudeScan(t, bin, def, []string{a, b})
	d2 := driveClaudeScan(t, bin, def, []string{b, a})
	for _, key := range []string{normalizeClaudeProbeHome(a), normalizeClaudeProbeHome(b)} {
		s1, ok1 := d1.quota.claudeSlotAttempt(key)
		s2, ok2 := d2.quota.claudeSlotAttempt(key)
		if !ok1 || !ok2 || s1.accountID == "" || s1.accountID != s2.accountID || len(s1.limits.Windows) != 2 || s1.limits.Windows[0].UsedPercent != s2.limits.Windows[0].UsedPercent {
			t.Fatalf("slot %s pairing failed: %+v %+v", key, s1, s2)
		}
	}
	if len(d1.QuotaSnapshot()) != 1 || len(d2.QuotaSnapshot()) != 1 {
		t.Fatalf("snapshots = %v %v, want one default entry", d1.QuotaSnapshot(), d2.QuotaSnapshot())
	}
	// Each home must answer with its own identity: a status read routed to
	// the default would reuse one account id across every slot.
	sa, _ := d1.quota.claudeSlotAttempt(normalizeClaudeProbeHome(a))
	sb, _ := d1.quota.claudeSlotAttempt(normalizeClaudeProbeHome(b))
	if sa.accountID == sb.accountID || sa.accountID == d1.QuotaSnapshot()[0].ID || sb.accountID == d1.QuotaSnapshot()[0].ID {
		t.Fatalf("claude slot identities not distinct: a=%q b=%q default=%q", sa.accountID, sb.accountID, d1.QuotaSnapshot()[0].ID)
	}
	dex := driveClaudeScan(t, bin, def, []string{exh})
	snapEx := dex.QuotaSnapshot()
	dDef := driveClaudeScan(t, bin, def, nil)
	snapDef := dDef.QuotaSnapshot()
	if len(snapEx) != 1 || len(snapDef) != 1 || snapEx[0].Provider != "claude" {
		t.Fatalf("heartbeat snapshots = %v %v", snapEx, snapDef)
	}
	if snapEx[0].ID != snapDef[0].ID || len(snapEx[0].Limits.Windows) != 2 || snapEx[0].Limits.Windows[0].UsedPercent != 10 || snapDef[0].Limits.Windows[0].UsedPercent != 10 {
		t.Fatalf("exhausted extra changed heartbeat: %+v vs %+v", snapEx, snapDef)
	}
	now := time.Now()
	if err := decideQuotaClaimAdmission(snapEx, string(agent.AuthHostSession), "claude", now); err != nil {
		t.Fatalf("claim admission refused on available default: %v", err)
	}
	if suspended, _ := quotaExhaustionSuspendsClaiming(snapEx, quotaServedClaimLanesForSnapshot(snapEx, map[string]string{"claude": bin}, nil), now); suspended {
		t.Fatal("claiming suspended by exhausted extra")
	}
}

func TestQuotaPoller_ClaudeDedupCountsUsageOnly(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "def")
	a := filepath.Join(root, "home-a")
	eb := filepath.Join(root, "home-b")
	neighbor := filepath.Join(root, "neighbor")
	for _, d := range []string{def, a, eb, neighbor} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	linkA := filepath.Join(root, "link-a")
	linkDef := filepath.Join(root, "link-def")
	if err := os.Symlink(a, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(def, linkDef); err != nil {
		t.Fatal(err)
	}
	usageLog := filepath.Join(root, "u.log")
	bin := writeClaudeSlotCLI(t, usageLog)
	driveClaudeScan(t, bin, def, nil)
	if got := len(slotUsageLines(t, usageLog)); got != 1 {
		t.Fatalf("default-only usage lines = %d, want 1", got)
	}
	if err := os.Remove(usageLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	d2 := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessClaude: bin}})
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	d2.config = &Config{QuotaProbeHomes: map[string][]string{"claude": {a, a, linkA, eb, linkDef}}}
	newQuotaPoller(d2.quota, d2.quotaProbes()).probeDue(context.Background())
	lines := slotUsageLines(t, usageLog)
	if len(lines) != 3 {
		t.Fatalf("usage lines = %v, want default plus two distinct extras", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, neighbor) {
			t.Fatalf("neighbor probed: %v", lines)
		}
	}
	n := len(lines)
	newQuotaPoller(d2.quota, d2.quotaProbes()).probeDue(context.Background())
	if got := len(slotUsageLines(t, usageLog)); got != n {
		t.Fatalf("second scan usage = %d, want %d (no repeat inside interval)", got, n)
	}
}

func TestQuotaPoller_ClaudeUnknownHangAndMuseCancel(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "def")
	good := filepath.Join(root, "home-a")
	hang := filepath.Join(root, "hang")
	later := filepath.Join(root, "home-b")
	for _, d := range []string{def, good, hang, later} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	usageLog := filepath.Join(root, "u.log")
	bin := writeClaudeSlotCLI(t, usageLog)
	d := driveClaudeScan(t, bin, def, []string{good, later})
	goodKey := normalizeClaudeProbeHome(good)
	first, ok := d.quota.claudeSlotAttempt(goodKey)
	if !ok || first.accountID == "" {
		t.Fatalf("first scan missing good slot: %+v", first)
	}
	if err := os.RemoveAll(good); err != nil {
		t.Fatal(err)
	}
	d.quota.mu.Lock()
	d.quota.claudeAt = time.Now().Add(-providercodex.RateLimitsProbeInterval - time.Second)
	d.quota.mu.Unlock()
	newQuotaPoller(d.quota, d.quotaProbes()).probeDue(context.Background())
	after, ok := d.quota.claudeSlotAttempt(goodKey)
	if !ok {
		t.Fatal("good slot missing after rescan")
	}
	if after.limits.Unavailable == nil || after.limits.Unavailable.Reason != agent.UsageUnavailableProbeFailed || after.limits.Unavailable.Answered {
		t.Fatalf("unreadable slot not unknown: %+v", after.limits.Unavailable)
	}
	if _, ok := d.quota.claudeSlotAttempt(normalizeClaudeProbeHome(later)); !ok {
		t.Fatal("sibling slot lost after failure")
	}
	if err := os.Remove(usageLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(usageLog + ".survived"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	dHang := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessClaude: bin}})
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	dHang.config = &Config{QuotaProbeHomes: map[string][]string{"claude": {hang, later}}}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	start := time.Now()
	newQuotaPoller(dHang.quota, dHang.quotaProbes()).probeDue(ctx)
	if time.Since(start) > 10*time.Second {
		t.Fatal("hung scan held caller past deadline")
	}
	for _, l := range slotUsageLines(t, usageLog) {
		if strings.Contains(l, later) {
			t.Fatalf("later slot started after cancellation: %v", l)
		}
	}
	hangAttempt, ok := dHang.quota.claudeSlotAttempt(normalizeClaudeProbeHome(hang))
	if !ok {
		t.Fatal("hang slot missing after cancelled scan")
	}
	if hangAttempt.limits.Unavailable == nil || hangAttempt.limits.Unavailable.Reason != agent.UsageUnavailableProbeFailed || hangAttempt.limits.Unavailable.Answered {
		t.Fatalf("hang slot not unknown-unanswered: %+v", hangAttempt.limits.Unavailable)
	}
	if _, ok := dHang.quota.claudeSlotAttempt(normalizeClaudeProbeHome(later)); ok {
		t.Fatal("later slot recorded after cancellation; no launch means no attempt")
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(usageLog + ".survived"); !os.IsNotExist(err) {
		t.Fatalf("hung child survived: %v", err)
	}
	if _, ok := dHang.quota.claudeSlotAttempt(normalizeClaudeProbeHome(def)); !ok {
		t.Fatal("earlier slot lost after hang")
	}
	goodMuse := writeFakeMuseProbeCLI(t, "Muse Code 1.4.9 (1.4.9-R6297.1)", false)
	dMuse := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessMuse: goodMuse}})
	newQuotaPoller(dMuse.quota, dMuse.quotaProbes()).probeDue(context.Background())
	if got := dMuse.QuotaSnapshot(); len(got) != 1 || got[0].AuthCheck == nil || !got[0].AuthCheck.OK {
		t.Fatalf("muse initial verdict = %+v", got)
	}
	busy := filepath.Join(root, "busy-muse")
	if err := testexec.WriteFile(busy, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatalf("write busy muse: %v", err)
	}
	dMuse2 := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessMuse: busy}})
	dMuse2.quota = dMuse.quota
	dMuse2.quota.mu.Lock()
	dMuse2.quota.museAt = time.Now().Add(-providercodex.RateLimitsProbeInterval - time.Second)
	dMuse2.quota.mu.Unlock()
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	newQuotaPoller(dMuse2.quota, dMuse2.quotaProbes()).probeDue(short)
	if got := dMuse.quota.snapshot(); len(got) != 1 || got[0].AuthCheck == nil || !got[0].AuthCheck.OK {
		t.Fatalf("cancelled muse overwrote verdict: %+v", got)
	}
}

func writeCodexSlotCLI(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *rateLimits/read*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
      [ -n "$id" ] || continue
      marker=$(cat "$CODEX_HOME/auth.json" 2>/dev/null)
      case "$marker" in
        *home-a*) acct="fixture-codex-a"; plan="team"; p1=11; p2=12;;
        *home-b*) acct="fixture-codex-b"; plan="plus"; p1=33; p2=34;;
        *exhausted*) acct="fixture-codex-exhausted"; plan="team"; p1=100; p2=100;;
        *) acct="fixture-codex-default"; plan="team"; p1=10; p2=20;;
      esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"accountId":"%s","rateLimits":{"limitId":"codex","planType":"%s","primary":{"usedPercent":%s,"resetsAt":1893456000,"windowDurationMins":300},"secondary":{"usedPercent":%s,"resetsAt":1893456000,"windowDurationMins":10080}}}}\n' "$id" "$acct" "$plan" "$p1" "$p2"
      ;;
    *initialized*)
      ;;
    *initialize*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
      [ -n "$id" ] || continue
      printf '{"jsonrpc":"2.0","id":%s,"result":{"codexHome":"%s"}}\n' "$id" "$CODEX_HOME"
      ;;
  esac
done
`
	path := filepath.Join(t.TempDir(), "fake-codex")
	if err := testexec.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write slot CLI: %v", err)
	}
	return path
}

func writeCodexHome(t *testing.T, dir, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"marker":"`+marker+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCodexCountingCLI writes a stand-in codex binary that answers the
// production app-server handshake and logs one line per rateLimits/read
// carrying the isolated home's auth marker, so a poller test can count
// how many distinct homes were probed without reading the log transport.
func writeCodexCountingCLI(t *testing.T, probeLog string) string {
	t.Helper()
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *rateLimits/read*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
      [ -n "$id" ] || continue
      marker=$(cat "$CODEX_HOME/auth.json" 2>/dev/null)
      printf '%s\n' "$marker" >> "` + probeLog + `"
      case "$marker" in
        *home-a*) acct="fixture-codex-a"; plan="team"; p1=11; p2=12;;
        *home-b*) acct="fixture-codex-b"; plan="plus"; p1=33; p2=34;;
        *exhausted*) acct="fixture-codex-exhausted"; plan="team"; p1=100; p2=100;;
        *) acct="fixture-codex-default"; plan="team"; p1=10; p2=20;;
      esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"accountId":"%s","rateLimits":{"limitId":"codex","planType":"%s","primary":{"usedPercent":%s,"resetsAt":1893456000,"windowDurationMins":300},"secondary":{"usedPercent":%s,"resetsAt":1893456000,"windowDurationMins":10080}}}}\n' "$id" "$acct" "$plan" "$p1" "$p2"
      ;;
    *initialized*)
      ;;
    *initialize*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
      [ -n "$id" ] || continue
      printf '{"jsonrpc":"2.0","id":%s,"result":{"codexHome":"%s"}}\n' "$id" "$CODEX_HOME"
      ;;
  esac
done
`
	path := filepath.Join(t.TempDir(), "fake-codex-counting")
	if err := testexec.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write counting slot CLI: %v", err)
	}
	return path
}

func codexProbeLines(t *testing.T, log string) []string {
	t.Helper()
	raw, _ := os.ReadFile(log)
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func driveCodexScan(t *testing.T, bin, def string, extras []string) *Daemon {
	t.Helper()
	t.Setenv("CODEX_HOME", def)
	d := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessCodex: bin}})
	d.config = &Config{QuotaProbeHomes: map[string][]string{agent.UsageHarnessCodex: extras}}
	newQuotaPoller(d.quota, d.quotaProbes()).probeDue(context.Background())
	return d
}

func TestQuotaPoller_CodexSlotsPairAccountsAndPreserveHeartbeat(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "def")
	a := filepath.Join(root, "home-a")
	b := filepath.Join(root, "home-b")
	exh := filepath.Join(root, "exhausted")
	writeCodexHome(t, def, "default")
	writeCodexHome(t, a, "home-a")
	writeCodexHome(t, b, "home-b")
	writeCodexHome(t, exh, "exhausted")
	bin := writeCodexSlotCLI(t)
	cfgPath := filepath.Join(root, "daemon.yaml")
	if err := WriteConfig(cfgPath, &Config{Machine: MachineConfig{ID: "m"}, Orchestrator: OrchestratorConfig{URL: "https://example.test"}, QuotaProbeHomes: map[string][]string{"codex": {a, b}}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil || len(loaded.QuotaProbeHomes["codex"]) != 2 {
		t.Fatalf("load homes = %v, %v", loaded, err)
	}
	d1 := driveCodexScan(t, bin, def, []string{a, b})
	d2 := driveCodexScan(t, bin, def, []string{b, a})
	for _, tc := range []struct {
		dir         string
		wantAccount string
		wantPlan    string
		wantPrimary float64
	}{
		{a, "fixture-codex-a", "team", 11},
		{b, "fixture-codex-b", "plus", 33},
	} {
		key := normalizeCodexProbeHome(tc.dir)
		s1, ok1 := d1.quota.codexSlotAttempt(key)
		s2, ok2 := d2.quota.codexSlotAttempt(key)
		if !ok1 || !ok2 || s1.accountID == "" || s1.accountID != s2.accountID || s1.accountID != tc.wantAccount || s1.plan != tc.wantPlan || s2.plan != tc.wantPlan {
			t.Fatalf("slot %s pairing failed: %+v %+v", tc.dir, s1, s2)
		}
		if len(s1.limits.Windows) != 2 || len(s2.limits.Windows) != 2 || s1.limits.Windows[0].UsedPercent != tc.wantPrimary || s2.limits.Windows[0].UsedPercent != tc.wantPrimary {
			t.Fatalf("slot %s windows failed: %+v %+v", tc.dir, s1.limits.Windows, s2.limits.Windows)
		}
	}
	if len(d1.QuotaSnapshot()) != 1 || len(d2.QuotaSnapshot()) != 1 {
		t.Fatalf("snapshots = %v %v, want one default entry", d1.QuotaSnapshot(), d2.QuotaSnapshot())
	}
	dex := driveCodexScan(t, bin, def, []string{exh})
	snapEx := dex.QuotaSnapshot()
	dDef := driveCodexScan(t, bin, def, nil)
	snapDef := dDef.QuotaSnapshot()
	if len(snapEx) != 1 || len(snapDef) != 1 || snapEx[0].Provider != "codex" {
		t.Fatalf("heartbeat snapshots = %v %v", snapEx, snapDef)
	}
	if snapEx[0].ID != snapDef[0].ID || snapEx[0].ID != "fixture-codex-default" || snapEx[0].Plan != "team" || len(snapEx[0].Limits.Windows) != 2 || snapEx[0].Limits.Windows[0].UsedPercent != 10 || snapDef[0].Limits.Windows[0].UsedPercent != 10 {
		t.Fatalf("exhausted extra changed heartbeat: %+v vs %+v", snapEx, snapDef)
	}
	now := time.Now()
	if err := decideQuotaClaimAdmission(snapEx, string(agent.AuthHostSession), "codex", now); err != nil {
		t.Fatalf("claim admission refused on available default: %v", err)
	}
	if suspended, _ := quotaExhaustionSuspendsClaiming(snapEx, quotaServedClaimLanesForSnapshot(snapEx, map[string]string{"codex": bin}, nil), now); suspended {
		t.Fatal("claiming suspended by exhausted extra")
	}
}

// TestQuotaPoller_CodexDedupCountsProbesOnly is the codex twin of the
// claude dedup proof: it drives the production poller with a counting
// fixture CLI, so duplicate and symlink aliases of the default and of one
// extra collapse to one probe each, an undeclared neighbor stays
// unprobed, and an immediate second scan starts no new probe.
func TestQuotaPoller_CodexDedupCountsProbesOnly(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "def")
	a := filepath.Join(root, "home-a")
	b := filepath.Join(root, "home-b")
	neighbor := filepath.Join(root, "neighbor")
	for _, d := range []string{def, a, b, neighbor} {
		writeCodexHome(t, d, filepath.Base(d))
	}
	linkA := filepath.Join(root, "link-a")
	linkDef := filepath.Join(root, "link-def")
	if err := os.Symlink(a, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(def, linkDef); err != nil {
		t.Fatal(err)
	}
	probeLog := filepath.Join(root, "probes.log")
	bin := writeCodexCountingCLI(t, probeLog)
	driveCodexScan(t, bin, def, nil)
	if got := len(codexProbeLines(t, probeLog)); got != 1 {
		t.Fatalf("default-only codex probes = %d, want 1", got)
	}
	if err := os.Remove(probeLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	d2 := New(Options{QuotaProbeBinaries: map[string]string{agent.UsageHarnessCodex: bin}})
	t.Setenv("CODEX_HOME", def)
	d2.config = &Config{QuotaProbeHomes: map[string][]string{agent.UsageHarnessCodex: {a, a, linkA, b, linkDef}}}
	newQuotaPoller(d2.quota, d2.quotaProbes()).probeDue(context.Background())
	lines := codexProbeLines(t, probeLog)
	if len(lines) != 3 {
		t.Fatalf("codex probe lines = %v, want default plus two distinct extras", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "neighbor") {
			t.Fatalf("neighbor probed: %v", lines)
		}
	}
	n := len(lines)
	newQuotaPoller(d2.quota, d2.quotaProbes()).probeDue(context.Background())
	if got := len(codexProbeLines(t, probeLog)); got != n {
		t.Fatalf("second scan codex probes = %d, want %d (no repeat inside interval)", got, n)
	}
}
