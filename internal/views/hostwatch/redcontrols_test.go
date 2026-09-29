package hostwatch

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/tui-components/theme"
)

// TestModel_SnapshotRetainsFoldedMetrics is the red control for the index
// poll overwriting live tail metrics: one tail tick folds tool counts and
// activity onto a card, then two index refreshes must keep them (the index
// carries no metrics, so a naive replace would zero them every tick).
func TestModel_SnapshotRetainsFoldedMetrics(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-keep", State: "running", Repository: "o/a",
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())

	now := m.now()
	m.foldMetrics(TailEvent{
		SessionID: "sess-keep",
		At:        now,
		Event:     agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test"}},
	})
	if got := findCard(m, "sess-keep").ToolCalls; got != 1 {
		t.Fatalf("precondition: ToolCalls = %d, want 1", got)
	}

	m.applySnapshot(m.src.Snapshot())
	m.applySnapshot(m.src.Snapshot())

	c := findCard(m, "sess-keep")
	if c == nil {
		t.Fatal("card vanished across refreshes")
	}
	if c.ToolCalls != 1 {
		t.Errorf("ToolCalls after two refreshes = %d, want 1", c.ToolCalls)
	}
	if c.LastActivity == "" {
		t.Error("LastActivity was dropped by the refresh")
	}
	if !c.Observed {
		t.Error("Observed was dropped by the refresh")
	}
}

// TestModel_SnapshotReorderAndRemove keeps per-session metrics with the
// right id when the index reorders, and drops metrics for removed ids:
// reorder must not shuffle metrics between sessions, and a removed session
// must not leak its metrics onto a later card.
func TestModel_SnapshotReorderAndRemove(t *testing.T) {
	mk := func(ids ...string) *fakeDaemon {
		var hs []afclient.DaemonSessionHandle
		for _, id := range ids {
			hs = append(hs, afclient.DaemonSessionHandle{
				SessionID: id, State: "running", Repository: "o/a",
			})
		}
		return &fakeDaemon{sessions: hs, status: &afclient.DaemonStatusResponse{MaxSessions: 8}}
	}
	fd := mk("a", "b")
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())

	now := m.now()
	m.foldMetrics(TailEvent{SessionID: "a", At: now, Event: agent.ToolUseEvent{ToolName: "Bash"}})
	m.foldMetrics(TailEvent{SessionID: "a", At: now, Event: agent.ToolUseEvent{ToolName: "Bash"}})
	m.foldMetrics(TailEvent{SessionID: "b", At: now, Event: agent.ToolUseEvent{ToolName: "Read"}})

	// Reorder: metrics must follow their session id.
	m.src = NewSource(mk("b", "a"), state.NewStore(), "")
	m.applySnapshot(m.src.Snapshot())
	if got := findCard(m, "a").ToolCalls; got != 2 {
		t.Errorf("after reorder: a.ToolCalls = %d, want 2", got)
	}
	if got := findCard(m, "b").ToolCalls; got != 1 {
		t.Errorf("after reorder: b.ToolCalls = %d, want 1", got)
	}

	// Remove b: its metrics must not leak onto a later card reusing the
	// slot, and a must keep its own.
	m.src = NewSource(mk("a"), state.NewStore(), "")
	m.applySnapshot(m.src.Snapshot())
	if findCard(m, "b") != nil {
		t.Fatal("removed session b still has a card")
	}
	if got := findCard(m, "a").ToolCalls; got != 2 {
		t.Errorf("after remove: a.ToolCalls = %d, want 2", got)
	}
}

// TestModel_ReplayNeverLooksActive is the red control for historical replay
// driving liveness: replayed history (scroll-back tailer) still folds
// cumulative metrics but must never advance freshness timestamps —
// replaying an old tool call must not make an idle session look active now.
func TestModel_ReplayNeverLooksActive(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-replay", State: "running", Repository: "o/a",
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())

	now := m.now()
	m.foldMetrics(TailEvent{
		SessionID: "sess-replay",
		At:        now,
		Replay:    true,
		Event:     agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "x"}},
	})
	c := findCard(m, "sess-replay")
	if c.ToolCalls != 1 {
		t.Errorf("replay should fold cumulative counts: got %d, want 1", c.ToolCalls)
	}
	if !c.LastWorkAt.IsZero() {
		t.Errorf("replay advanced work freshness to %v; must stay zero", c.LastWorkAt)
	}

	// The same event observed live advances freshness.
	m.foldMetrics(TailEvent{
		SessionID: "sess-replay",
		At:        now,
		Replay:    false,
		Event:     agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "x"}},
	})
	c = findCard(m, "sess-replay")
	if c.LastWorkAt.IsZero() {
		t.Error("live event did not advance work freshness")
	}
}

// TestModel_TerminalCostArrivesOnlyOnResult pins the cost/turns contract:
// tool output never reports cost, and only the terminal result event flips
// MetricsReported (which is what the card's "not reported" rendering keys
// on).
func TestModel_TerminalCostArrivesOnlyOnResult(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-cost", State: "running", Repository: "o/a",
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())

	now := m.now()
	m.foldMetrics(TailEvent{
		SessionID: "sess-cost", At: now,
		Event: agent.ToolResultEvent{ToolName: "Bash", Content: "ok"},
	})
	if c := findCard(m, "sess-cost"); c.MetricsReported {
		t.Error("non-terminal output must not mark metrics reported")
	}

	m.foldMetrics(TailEvent{
		SessionID: "sess-cost", At: now,
		Event: agent.ResultEvent{Success: true, Cost: &agent.CostData{TotalCostUsd: 1.25, NumTurns: 4}},
	})
	c := findCard(m, "sess-cost")
	if !c.MetricsReported || c.CostUsd != 1.25 || c.NumTurns != 4 {
		t.Errorf("terminal cost not folded: %+v", c)
	}
}

// TestSource_HandleWinsOverState pins the owning-seam priority: the daemon
// index handle carries the admitted display identity, so it wins over a
// stale state.json; state.json only backfills what the handle omits (the
// old-daemon path).
func TestSource_HandleWinsOverState(t *testing.T) {
	dir := t.TempDir()
	wt := dir + "/sess-prio"
	writeState(t, wt, state.State{
		Harness: "stale-harness", Model: "stale-model",
		ProviderName: "stale-provider", WorkType: "stale-work",
	})
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-prio", State: "running", Repository: "o/a",
			WorktreePath: wt,
			Harness:      "fresh-harness", Model: "fresh-model",
			ModelProvider: "fresh-vendor", WorkType: "fresh-work",
		}},
	}
	src := NewSource(fd, state.NewStore(), "")
	card := src.Snapshot().Cards[0]
	if card.Harness != "fresh-harness" || card.Model != "fresh-model" ||
		card.ModelProvider != "fresh-vendor" || card.WorkType != "fresh-work" {
		t.Errorf("handle must win over state.json: %+v", card)
	}

	// Old daemon: handle omits the new fields, state.json backfills.
	fdOld := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-old", State: "running", Repository: "o/a",
			WorktreePath: wt,
		}},
	}
	old := NewSource(fdOld, state.NewStore(), "").Snapshot().Cards[0]
	if old.Harness != "stale-harness" || old.Model != "stale-model" ||
		old.WorkType != "stale-work" {
		t.Errorf("state.json must backfill old-daemon handles: %+v", old)
	}
	// Old daemon with no state.json either renders as absent (the card
	// layer labels these unknown) — not as invented values.
	empty := NewSource(&fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-bare", State: "running", Repository: "o/a",
		}},
	}, &noopState{}, "").Snapshot().Cards[0]
	if empty.Harness != "" || empty.Model != "" || empty.ModelProvider != "" || empty.WorkType != "" {
		t.Errorf("absent fields must stay absent: %+v", empty)
	}
}

// TestModelProvider_LegacyFallback pins the vendor axis split: an explicit
// ModelProvider always wins, and the legacy conflated Provider only fills
// the gap for state written before the split.
func TestModelProvider_LegacyFallback(t *testing.T) {
	explicit := SessionCard{Provider: "legacy", ModelProvider: "anthropic"}
	if got := explicit.modelProvider(); got != "anthropic" {
		t.Errorf("explicit ModelProvider must win: got %q", got)
	}
	legacy := SessionCard{Provider: "legacy"}
	if got := legacy.modelProvider(); got != "legacy" {
		t.Errorf("legacy Provider must backfill: got %q", got)
	}
	if got := (SessionCard{}).modelProvider(); got != "" {
		t.Errorf("absent vendor must stay absent: got %q", got)
	}
}

// TestRenderCard_Widths checks truthful labels at card widths and the
// deliberate compact fallback below the minimum card width.
func TestRenderCard_Widths(t *testing.T) {
	now := time.Date(2026, 6, 13, 14, 5, 0, 0, time.UTC)
	cards := []SessionCard{
		{
			SessionID: "sess-1", DaemonState: "running", IssueIdentifier: "ENG-1284",
			Harness: "loop-driver", Model: "model-id", ModelProvider: "vendor",
			Provider: "legacy", WorkType: "development",
			StartedAtUnixMs: now.Add(-4 * time.Minute).UnixMilli(),
			ToolCalls:       37, Observed: true, CostUsd: 0.84, NumTurns: 5,
			MetricsReported: true, LastActivity: "Bash: pnpm test",
			LastWorkAt: now.Add(-30 * time.Second), LastOutputAt: now.Add(-10 * time.Second),
		},
		// An old-daemon card with absent fields overflows gracefully.
		{SessionID: "sess-2"},
	}
	tm := theme.DefaultTheme()
	for _, width := range []int{40, 80, 120, 200} {
		out := renderGrid(tm, cards, 0, 0, width, 0, true, now)
		t.Logf("--- width %d ---\n%s", width, out)
		if width < minCardWidth {
			if !strings.Contains(out, "ENG-1284") || strings.Contains(out, "harness loop-driver") {
				t.Errorf("width %d: compact fallback must show identity without full fields:\n%s", width, out)
			}
			continue
		}
		for _, want := range []string{
			"harness loop-driver", "provider vendor", "tools 37",
			"harness unknown", "tools not reported",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: render missing %q", width, want)
			}
		}
	}
}
