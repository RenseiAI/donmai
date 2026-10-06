package daemon

import (
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
