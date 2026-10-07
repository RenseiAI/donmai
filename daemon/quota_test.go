package daemon

import (
	"reflect"
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

func TestQuotaState_ProbeDueFiveMinutes(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if !q.codexProbeDue(base) || !q.claudeProbeDue(base) {
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

func TestQuotaState_SnapshotOmitsUnavailable(t *testing.T) {
	t.Parallel()
	q := &quotaState{}
	if got := q.snapshot(); len(got) != 0 {
		t.Fatalf("empty snapshot = %+v, want none", got)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUsageLimits(agent.ISOTime(base), quotaTestWindows()), base)
	q.noteClaudeProbe("opaque-2", agent.MakeUnavailableUsageLimits(agent.ISOTime(base), agent.UsageUnavailableUnsupported, ""), base)
	got := (&Daemon{quota: q}).quotaSnapshot()
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
	if nilDaemon.quotaSnapshot() != nil {
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
	failed := func(at time.Time) agent.UsageLimits {
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
			for _, acct := range (&Daemon{quota: q}).quotaSnapshot() {
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
	q.noteCodexProbe("opaque-1", "promax", agent.MakeUnavailableUsageLimits(agent.ISOTime(later), agent.UsageUnavailableProbeFailed, ""), later)
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
