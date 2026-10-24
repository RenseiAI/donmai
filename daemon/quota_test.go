package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func quotaTestWindows() []agent.UsageWindow {
	d := 10080
	return []agent.UsageWindow{
		{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: 1, WindowDurationMins: &d},
	}
}

// quotaTestRefusedProbe is a failed read the provider answered (for
// example a JSON-RPC refusal): a real login verdict.
func quotaTestRefusedProbe(at time.Time) agent.UsageLimits {
	out := agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex could not read usage (JSON-RPC -32600).")
	out.Unavailable.Answered = true
	return out
}

func TestQuotaState_ProbeDueFiveMinutes(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if !q.codexProbeDue(base) || !q.claudeProbeDue(base) || !q.museProbeDue(base) {
		t.Fatal("first probe must be due")
	}
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	if q.codexProbeDue(base.Add(4 * time.Minute)) {
		t.Error("codex probe inside the interval must not be due")
	}
	if !q.codexProbeDue(base.Add(5 * time.Minute)) {
		t.Error("codex probe at the interval must be due")
	}
}

// A failed probe advances the probe clock like a successful one: the
// next read stays on the 5-minute cadence instead of firing again
// immediately, while the last good windows stay published.
func TestQuotaState_FailedProbeAdvancesProbeClock(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	attempt := base.Add(time.Minute)
	failed := agent.MakeUnavailableUsageLimits(agent.ISOTime(attempt), agent.UsageUnavailableProbeFailed, "unreachable")
	for name, note := range map[string]func(q *quotaState){
		"codex":  func(q *quotaState) { q.noteCodexProbe("opaque-1", "promax", failed, attempt) },
		"claude": func(q *quotaState) { q.noteClaudeProbe("opaque-2", failed, attempt) },
		"muse":   func(q *quotaState) { q.noteMuseProbe("muse-host-session", failed, attempt) },
	} {
		q := &quotaState{}
		note(q)
		due := q.codexProbeDue
		if name == "claude" {
			due = q.claudeProbeDue
		}
		if name == "muse" {
			due = q.museProbeDue
		}
		if due(attempt.Add(4 * time.Minute)) {
			t.Errorf("%s probe inside the interval after a failed attempt must not be due", name)
		}
		if !due(attempt.Add(5 * time.Minute)) {
			t.Errorf("%s probe at the interval after a failed attempt must be due", name)
		}
	}
}

func TestQuotaState_SnapshotOmitsUnavailable(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	if got := q.snapshot(); len(got) != 0 {
		t.Fatalf("empty snapshot = %+v, want none", got)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteClaudeProbe("opaque-2", agent.MakeUnavailableUsageLimits(agent.ISOTime(base), agent.UsageUnavailableUnsupported, ""), base)
	got := (&Daemon{quota: q}).QuotaSnapshot()
	if len(got) != 1 || got[0].Provider != "codex" || got[0].Plan != "promax" {
		t.Fatalf("snapshot = %+v, want the codex account only", got)
	}
	if got[0].ID != "opaque-1" || got[0].LimitID != "codex" {
		t.Errorf("account identity = %+v, want the opaque id and main bucket", got[0])
	}
}

func TestQuotaState_FailedProbeKeepsLastWindows(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUnavailableUsageLimits(agent.ISOTime(base.Add(time.Minute)), agent.UsageUnavailableProbeFailed, " unreachable"), base.Add(time.Minute))
	got := q.snapshot()
	if len(got) != 1 || len(got[0].Limits.Windows) != 1 || got[0].Limits.Windows[0].UsedPercent != 1 {
		t.Fatalf("snapshot = %+v, want the last good windows kept", got)
	}
}

func TestQuotaState_SparseUpdateMerges(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteClaudeProbe("opaque-2", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteClaudeUpdate(agent.UsageLimitsUpdate{
		CheckedAt: agent.ISOTime(base.Add(time.Minute)),
		Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: 9}},
	}, base.Add(time.Minute))
	if got := q.snapshot(); len(got) != 2 {
		t.Fatalf("snapshot = %+v, want both providers", got)
	}
	for _, acct := range q.snapshot() {
		if acct.Provider == "claude" && (len(acct.Limits.Windows) != 1 || acct.Limits.Windows[0].UsedPercent != 9) {
			t.Fatalf("claude snapshot = %+v, want the merged 9 percent", acct.Limits.Windows)
		}
	}
	q.noteCodexUpdate(agent.UsageLimitsUpdate{
		CheckedAt: agent.ISOTime(base.Add(time.Minute)),
		Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: 2}},
	}, base.Add(time.Minute))
	got := q.snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot = %+v, want both providers", got)
	}
	for _, acct := range got {
		if acct.Provider == "codex" {
			if len(acct.Limits.Windows) != 1 || acct.Limits.Windows[0].UsedPercent != 2 {
				t.Fatalf("codex snapshot = %+v, want the merged 2 percent", acct.Limits.Windows)
			}
			if acct.Limits.Windows[0].WindowDurationMins == nil {
				t.Error("sparse update dropped the probe's window duration")
			}
		}
	}
	var nilDaemon *Daemon
	if nilDaemon.QuotaSnapshot() != nil {
		t.Error("nil daemon must report no quota")
	}
}

// TestQuotaState_AuthCheck covers the login-check verdict each probe
// leaves on its account entry: ok after a read that produced windows,
// not ok after a failed read, stamped with the probe time, and omitted
// until a probe has run.
func TestQuotaState_AuthCheck(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	later := base.Add(10 * time.Minute)
	good := func(at time.Time) agent.UsageLimits {
		return agent.MakeUsageLimits(agent.ISOTime(at), quotaTestWindows())
	}
	failed := quotaTestRefusedProbe
	unreached := func(at time.Time) agent.UsageLimits {
		return agent.MakeUnavailableUsageLimits(agent.ISOTime(at), agent.UsageUnavailableProbeFailed, "Codex did not answer the usage request.")
	}
	update := func(at time.Time) agent.UsageLimitsUpdate {
		return agent.UsageLimitsUpdate{
			CheckedAt: agent.ISOTime(at),
			Windows:   []agent.UsageWindow{{ID: "primary", Kind: agent.UsageWindowWeekly, Label: "Weekly", UsedPercent: 9}},
		}
	}
	check := func(harness string, ok bool, at time.Time) *agent.UsageAuthCheck {
		return &agent.UsageAuthCheck{Harness: harness, OK: ok, CheckedAt: agent.ISOTime(at)}
	}

	tests := []struct {
		name string
		run  func(q *quotaState)
		// want maps provider to the expected authCheck. A present key
		// with a nil value is an entry that must carry no authCheck; an
		// absent key is an entry that must not be reported.
		want map[string]*agent.UsageAuthCheck
	}{
		{
			name: "successful codex probe is ok at the probe time",
			run:  func(q *quotaState) { q.noteCodexProbe("opaque-1", "promax", good(base), base) },
			want: map[string]*agent.UsageAuthCheck{"codex": check("codex", true, base)},
		},
		{
			name: "successful claude probe is ok at the probe time",
			run:  func(q *quotaState) { q.noteClaudeProbe("opaque-2", good(base), base) },
			want: map[string]*agent.UsageAuthCheck{"claude": check("claude", true, base)},
		},
		{
			name: "failed probe after a good one is not ok at the failure time",
			run: func(q *quotaState) {
				q.noteCodexProbe("opaque-1", "promax", good(base), base)
				q.noteCodexProbe("opaque-1", "promax", failed(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{"codex": check("codex", false, later)},
		},
		{
			name: "next good probe restores ok",
			run: func(q *quotaState) {
				q.noteClaudeProbe("opaque-2", good(base), base)
				q.noteClaudeProbe("opaque-2", failed(base.Add(5*time.Minute)), base.Add(5*time.Minute))
				q.noteClaudeProbe("opaque-2", good(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{"claude": check("claude", true, later)},
		},
		{
			name: "each harness carries its own verdict",
			run: func(q *quotaState) {
				q.noteCodexProbe("opaque-1", "promax", good(base), base)
				q.noteClaudeProbe("opaque-2", good(base), base)
				q.noteClaudeProbe("opaque-2", failed(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{
				"codex":  check("codex", true, base),
				"claude": check("claude", false, later),
			},
		},
		{
			name: "read that never reached the provider keeps the previous verdict",
			run: func(q *quotaState) {
				q.noteCodexProbe("opaque-1", "promax", good(base), base)
				q.noteCodexProbe("opaque-1", "promax", unreached(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{"codex": check("codex", true, base)},
		},
		{
			name: "read that never reached the provider records no first verdict",
			run: func(q *quotaState) {
				q.noteClaudeUpdate(update(base), base)
				q.noteClaudeProbe("opaque-2", unreached(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{"claude": nil},
		},
		{
			name: "turn update never refreshes the verdict",
			run: func(q *quotaState) {
				q.noteCodexProbe("opaque-1", "promax", good(base), base)
				q.noteCodexUpdate(update(later), later)
			},
			want: map[string]*agent.UsageAuthCheck{"codex": check("codex", true, base)},
		},
		{
			name: "windows from a turn update alone carry no verdict",
			run:  func(q *quotaState) { q.noteClaudeUpdate(update(base), base) },
			want: map[string]*agent.UsageAuthCheck{"claude": nil},
		},
		{
			name: "probe without a time records no verdict",
			run:  func(q *quotaState) { q.noteCodexProbe("opaque-1", "promax", good(base), time.Time{}) },
			want: map[string]*agent.UsageAuthCheck{"codex": nil},
		},
		{
			name: "account with no subscription windows is not reported",
			run: func(q *quotaState) {
				q.noteClaudeProbe("opaque-2", agent.MakeUnavailableUsageLimits(agent.ISOTime(base), agent.UsageUnavailableUnsupported, ""), base)
			},
			want: map[string]*agent.UsageAuthCheck{},
		},
		{
			name: "no probe reports nothing",
			run:  func(_ *quotaState) {},
			want: map[string]*agent.UsageAuthCheck{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := &quotaState{}
			tt.run(q)
			got := map[string]*agent.UsageAuthCheck{}
			for _, acct := range (&Daemon{quota: q}).QuotaSnapshot() {
				got[acct.Provider] = acct.AuthCheck
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("authCheck by provider = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestQuotaState_FailedProbeKeepsWindowsButReportsNotOK pins the two
// facts a consumer needs together after a lapsed login: the last good
// bars stay published, and the verdict beside them says the check failed.
func TestQuotaState_FailedProbeKeepsWindowsButReportsNotOK(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	later := base.Add(time.Minute)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteCodexProbe("opaque-1", "promax", quotaTestRefusedProbe(later), later)
	got := q.snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v, want one account", got)
	}
	if len(got[0].Limits.Windows) != 1 || got[0].Limits.CheckedAt != agent.ISOTime(base) {
		t.Errorf("limits = %+v, want the windows from the good probe", got[0].Limits)
	}
	if got[0].AuthCheck == nil || got[0].AuthCheck.OK || got[0].AuthCheck.CheckedAt != agent.ISOTime(later) {
		t.Errorf("authCheck = %+v, want not ok at the failure time", got[0].AuthCheck)
	}
}

// TestQuotaState_MuseProbeEmitsWindowlessEntry pins the muse
// heartbeat entry: the login check proves a supported CLI is
// installed, never usage windows, so the entry carries an empty
// window set beside its verdict, with the probe time on both.
func TestQuotaState_MuseProbeEmitsWindowlessEntry(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q := &quotaState{}
	if got := q.snapshot(); len(got) != 0 {
		t.Fatalf("empty snapshot = %+v, want none", got)
	}
	q.noteMuseProbe("muse-host-session", agent.MakeUsageLimits(agent.ISOTime(base), nil), base)
	got := (&Daemon{quota: q}).QuotaSnapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v, want the one muse entry", got)
	}
	acct := got[0]
	if acct.ID != "muse-host-session" || acct.Provider != agent.UsageHarnessMuse {
		t.Errorf("account = %+v, want the host-session muse entry", acct)
	}
	if acct.Plan != "" || acct.LimitID != "" {
		t.Errorf("account = %+v, want no plan or bucket on a windowless entry", acct)
	}
	if acct.Limits.CheckedAt != agent.ISOTime(base) || len(acct.Limits.Windows) != 0 {
		t.Errorf("limits = %+v, want an empty window set at the probe time", acct.Limits)
	}
	if acct.AuthCheck == nil || acct.AuthCheck.Harness != agent.UsageHarnessMuse || !acct.AuthCheck.OK || acct.AuthCheck.CheckedAt != agent.ISOTime(base) {
		t.Errorf("authCheck = %+v, want ok:true for muse at the probe time", acct.AuthCheck)
	}
	// A refused check keeps the verdict shape: ok:false at the new
	// probe time, still without windows.
	later := base.Add(5 * time.Minute)
	refused := agent.MakeUnavailableUsageLimits(agent.ISOTime(later), agent.UsageUnavailableProbeFailed, "Muse did not answer the usage request.")
	refused.Unavailable.Answered = true
	q.noteMuseProbe("muse-host-session", refused, later)
	got = (&Daemon{quota: q}).QuotaSnapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %+v, want the one muse entry", got)
	}
	if got[0].AuthCheck == nil || got[0].AuthCheck.OK || got[0].AuthCheck.CheckedAt != agent.ISOTime(later) {
		t.Errorf("authCheck = %+v, want not ok at the failure time", got[0].AuthCheck)
	}
	if len(got[0].Limits.Windows) != 0 {
		t.Errorf("limits = %+v, want no windows after a refused check", got[0].Limits)
	}
	// A read that never reached the CLI keeps the previous verdict.
	unreached := agent.MakeUnavailableUsageLimits(agent.ISOTime(later.Add(time.Minute)), agent.UsageUnavailableProbeFailed, "Muse did not answer the usage request.")
	q.noteMuseProbe("muse-host-session", unreached, later.Add(time.Minute))
	got = (&Daemon{quota: q}).QuotaSnapshot()
	if len(got) != 1 || got[0].AuthCheck == nil || got[0].AuthCheck.OK || got[0].AuthCheck.CheckedAt != agent.ISOTime(later) {
		t.Fatalf("authCheck = %+v, want the refused verdict untouched", got[0].AuthCheck)
	}
}

// TestQuotaState_SnapshotDoesNotAliasAuthCheck guards the published
// snapshot against mutating the cached verdict.
func TestQuotaState_SnapshotDoesNotAliasAuthCheck(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	first := q.snapshot()
	if len(first) != 1 || first[0].AuthCheck == nil {
		t.Fatalf("snapshot = %+v, want one account with an authCheck", first)
	}
	first[0].AuthCheck.OK = false
	first[0].AuthCheck.CheckedAt = "tampered"
	again := q.snapshot()
	if len(again) != 1 || again[0].AuthCheck == nil || !again[0].AuthCheck.OK || again[0].AuthCheck.CheckedAt != agent.ISOTime(base) {
		t.Fatalf("authCheck = %+v, want the cached ok verdict untouched", again[0].AuthCheck)
	}
}

// TestDaemon_QuotaSnapshot_NilSafeAndSeeded pins the exported snapshot
// contract: nil and empty daemons report nothing, and a daemon with all
// three probes seeded reports one account per harness, each with its
// login verdict.
func TestDaemon_QuotaSnapshot_NilSafeAndSeeded(t *testing.T) {
	t.Parallel()
	var nilDaemon *Daemon
	if got := nilDaemon.QuotaSnapshot(); got != nil {
		t.Fatalf("nil daemon snapshot = %+v, want nil", got)
	}
	if got := (&Daemon{}).QuotaSnapshot(); got != nil {
		t.Fatalf("empty daemon snapshot = %+v, want nil", got)
	}
	if got := New(Options{}).QuotaSnapshot(); got != nil {
		t.Fatalf("fresh daemon snapshot = %+v, want nil", got)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q := &quotaState{}
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteClaudeProbe("opaque-2", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteMuseProbe("muse-host-session", agent.MakeUsageLimits(agent.ISOTime(base), nil), base)
	got := (&Daemon{quota: q}).QuotaSnapshot()
	if len(got) != 3 {
		t.Fatalf("snapshot = %+v, want one account per harness", got)
	}
	byProvider := map[string]agent.UsageAccount{}
	for _, acct := range got {
		byProvider[acct.Provider] = acct
	}
	for _, harness := range []string{agent.UsageHarnessCodex, agent.UsageHarnessClaude, agent.UsageHarnessMuse} {
		acct, ok := byProvider[harness]
		if !ok {
			t.Fatalf("snapshot has no %s entry: %+v", harness, got)
		}
		if acct.AuthCheck == nil || acct.AuthCheck.Harness != harness {
			t.Errorf("%s authCheck = %+v, want a verdict for %s", harness, acct.AuthCheck, harness)
		}
	}
}

// TestDaemon_QuotaSnapshot_DeepCopy guards the published snapshot
// against aliasing the cache: mutating every mutable field of a
// returned account leaves the next call untouched.
func TestDaemon_QuotaSnapshot_DeepCopy(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	probed := agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows())
	probed.ResetCredits = &agent.ResetCredits{AvailableCount: 2, NextExpiresAt: agent.ISOTime(base)}
	q := &quotaState{}
	q.noteCodexProbe("opaque-1", "promax", probed, base)
	d := &Daemon{quota: q}
	first := d.QuotaSnapshot()
	if len(first) != 1 {
		t.Fatalf("snapshot = %+v, want the one codex account", first)
	}
	if first[0].AuthCheck == nil || first[0].Limits.ResetCredits == nil || len(first[0].Limits.Windows) != 1 || first[0].Limits.Windows[0].WindowDurationMins == nil {
		t.Fatalf("snapshot = %+v, want verdict, reset credits and a window duration to mutate", first)
	}
	want := d.QuotaSnapshot()
	mut := &first[0]
	mut.AuthCheck.OK = !mut.AuthCheck.OK
	mut.AuthCheck.CheckedAt = "tampered"
	mut.Limits.Windows[0].UsedPercent = 999
	*mut.Limits.Windows[0].WindowDurationMins = 9999
	mut.Limits.ResetCredits.AvailableCount = 999
	mut.Limits.Windows = append(mut.Limits.Windows, agent.UsageWindow{ID: "injected", Kind: agent.UsageWindowWeekly, Label: "Injected", UsedPercent: 50})
	got := d.QuotaSnapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second snapshot = %+v, want the pristine %+v", got, want)
	}
	if got[0].Limits.Windows[0].UsedPercent == 999 || *got[0].Limits.Windows[0].WindowDurationMins == 9999 {
		t.Errorf("windows = %+v, want the probe's values untouched", got[0].Limits.Windows)
	}
	if got[0].Limits.ResetCredits.AvailableCount == 999 {
		t.Errorf("resetCredits = %+v, want the probe's count untouched", got[0].Limits.ResetCredits)
	}
	if len(got[0].Limits.Windows) != 1 {
		t.Errorf("windows = %+v, want the appended row kept out of the cache", got[0].Limits.Windows)
	}
}

// TestDaemon_QuotaSnapshot_MuseWindowsEmptyArray pins the windowless
// entry's wire shape: an empty non-nil window set serializes as [],
// never null, both on first read and on a copy.
func TestDaemon_QuotaSnapshot_MuseWindowsEmptyArray(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q := &quotaState{}
	q.noteMuseProbe("muse-host-session", agent.MakeUsageLimits(agent.ISOTime(base), nil), base)
	d := &Daemon{quota: q}
	for _, snap := range []struct {
		name string
		acct []agent.UsageAccount
	}{
		{"first read", d.QuotaSnapshot()},
		{"copy", d.QuotaSnapshot()},
	} {
		if len(snap.acct) != 1 {
			t.Fatalf("%s: snapshot = %+v, want the one muse entry", snap.name, snap.acct)
		}
		raw, err := json.Marshal(snap.acct[0].Limits)
		if err != nil {
			t.Fatalf("%s: marshal limits: %v", snap.name, err)
		}
		if !strings.Contains(string(raw), `"windows":[]`) {
			t.Errorf("%s: limits = %s, want an empty windows array", snap.name, raw)
		}
	}
}
