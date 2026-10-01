package hostwatch

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// newTestModel builds a plain-mode model over a fake daemon + on-disk
// worktree fixtures, sized for rendering.
func newTestModel(t *testing.T, fd *fakeDaemon, scope string) *Model {
	t.Helper()
	src := NewSource(fd, state.NewStore(), scope)
	m := New(Options{
		Source:        src,
		Plain:         true,
		IndexInterval: time.Millisecond,
		TailInterval:  time.Millisecond,
		Now:           func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return mm.(*Model)
}

func TestModel_FullFlow_PlainRender(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-1")
	writeState(t, wt, state.State{
		IssueIdentifier: "ENG-1284",
		ProviderName:    "claude",
		WorkType:        "development",
		StartedAt:       time.Date(2026, 6, 13, 13, 58, 0, 0, time.UTC).UnixMilli(),
	})
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID:    "sess-1",
			State:        "running",
			Repository:   "o/acme",
			ProjectName:  "acme",
			WorktreePath: wt,
		}},
		status: &afclient.DaemonStatusResponse{ActiveSessions: 1, MaxSessions: 8, UptimeSeconds: 60, Version: "0.39.0"},
		stats:  &afclient.DaemonStatsResponse{QueueDepth: 0},
	}

	m := newTestModel(t, fd, "o/acme")

	// 1. Drive an index poll → cards + tailer registration.
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 1 {
		t.Fatalf("want 1 card after snapshot, got %d", len(m.cards))
	}
	if _, ok := m.tailers["sess-1"]; !ok {
		t.Fatal("tailer should be registered for sess-1 (worktree path known)")
	}

	// 2. Append events to the session's events.jsonl, then drive a tail tick.
	writeEvents(t, evPath,
		agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "pnpm test"}},
		agent.ToolResultEvent{ToolName: "Bash", Content: "ok"},
	)
	drainTails(t, m)

	writeEvents(t, evPath, agent.AssistantTextEvent{Text: "running the suite"})
	drainTails(t, m)

	out := m.render()
	for _, want := range []string{"ENG-1284", "session stream", "development", "running the suite"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestModel_TerminalAggregateReplacesWithoutEndingLiveTail(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-x")
	writeState(t, wt, state.State{IssueIdentifier: "ENG-9", StartedAt: 1})
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "sess-x", State: "running", Repository: "o/a", WorktreePath: wt,
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	drainTails(t, m)

	cost := 2.0
	turns := 3
	writeEvents(t, evPath, agent.ResultEvent{Success: true, Cost: &agent.CostData{TotalCostUsd: cost, NumTurns: turns}, ObservedCostUsd: &cost, ObservedTurns: &turns})
	drainTails(t, m)

	if _, ok := m.tailers["sess-x"]; !ok {
		t.Error("live tailer must remain after a provider turn result")
	}
	if c := findCard(m, "sess-x"); c == nil || c.CostUsd != 2.0 || c.NumTurns != 3 || !c.CostReported || !c.TurnsReported {
		t.Errorf("terminal cost/turns not folded onto card: %#v", c)
	}
}

func TestModel_DaemonUnreachableShowsWarning(t *testing.T) {
	fd := &fakeDaemon{sessErr: errFake("connection refused")}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	out := m.render()
	if !strings.Contains(out, "daemon unreachable") {
		t.Errorf("want unreachable warning, got:\n%s", out)
	}
}

func TestModel_PreEnrichmentNoWorktreeNoTailer(t *testing.T) {
	// A pre-enrichment daemon returns no WorktreePath; the model still shows
	// a card but cannot tail it.
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID: "old", State: "running", Repository: "o/a", // no WorktreePath
		}},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(m.cards))
	}
	if len(m.tailers) != 0 {
		t.Errorf("want 0 tailers (no worktree path), got %d", len(m.tailers))
	}
}

func TestModel_HeldSessionHasNoTailerActivityOrRunningClaim(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "held-123", State: "unknown", Repository: "o/a", ProjectName: "a"},
			{SessionID: "live-123", State: "running", Repository: "o/a"},
		},
		status: &afclient.DaemonStatusResponse{ActiveSessions: 1, MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	// Plain cards stack; reserve enough rows for both complete cards.
	resized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 80})
	m = resized.(*Model)
	// Model an earlier projection of the same identity as live: when the new
	// held snapshot arrives, any stale event tailer must be dropped.
	m.tailers["held-123"] = NewTailer("held-123", "/fixture/old/events.jsonl", true, time.Now)
	m.applySnapshot(m.src.Snapshot())
	if len(m.cards) != 2 {
		t.Fatalf("want both visible sessions, got %d", len(m.cards))
	}
	if _, ok := m.tailers["held-123"]; ok {
		t.Error("held session must not retain or start an event tailer")
	}

	m.applyTailBatch([]TailEvent{{
		SessionID: "held-123", At: time.Now(), Event: agent.AssistantTextEvent{Text: "fabricated live activity"},
	}})
	out := m.render()
	for _, want := range []string{"2 sessions", "held-123", "live-123", "held", "project a", "repo o/a"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if strings.Contains(header, "running") || !strings.Contains(header, "2 sessions") {
		t.Errorf("header should count visible sessions without claiming all are running: %q", header)
	}
	for _, absent := range []string{"fabricated live activity"} {
		if strings.Contains(out, absent) {
			t.Errorf("render should not claim/show %q:\n%s", absent, out)
		}
	}
	if c := findCard(m, "held-123"); c == nil || c.LastActivity != "" || c.ToolCalls != 0 {
		t.Errorf("held card acquired live metrics: %#v", c)
	}
}

func TestHeldSnapshotDropsPreviousLiveObservations(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{
		SessionID: "held", DaemonState: "running", Observed: true, MetricsReported: true,
		ActualModel: "prior-response", ActualModelProvider: "prior-vendor", ActualModelVersion: "prior-version",
		ToolCalls: 3, CostUsd: 1, LastActivity: "prior activity", LastWorkAt: time.Now(), LastOutputAt: time.Now(),
	}}
	m.applySnapshot(Snapshot{Cards: []SessionCard{{SessionID: "held", DaemonState: "unknown", AgentCardID: "card-id", AgentCardName: "Review"}}})
	c := m.cards[0]
	if c.Observed || c.MetricsReported || c.ActualModel != "" || c.ActualModelProvider != "" || c.ActualModelVersion != "" ||
		c.ToolCalls != 0 || c.CostUsd != 0 || c.LastActivity != "" || !c.LastWorkAt.IsZero() || !c.LastOutputAt.IsZero() {
		t.Fatalf("held projection inherited previous live data: %+v", c)
	}
	if c.AgentCardID != "card-id" || c.AgentCardName != "Review" {
		t.Fatalf("held projection lost explicit card annotations: %+v", c)
	}
}

func TestModel_KeyNavAndFollow(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "a", State: "running", Repository: "o/a"},
			{SessionID: "b", State: "running", Repository: "o/a"},
		},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newTestModel(t, fd, "")
	m.applySnapshot(m.src.Snapshot())
	if m.cursor != 0 {
		t.Fatalf("initial cursor: want 0, got %d", m.cursor)
	}
	m.handleKey(keyMsg("down"))
	if m.cursor != 1 {
		t.Errorf("after down: want cursor 1, got %d", m.cursor)
	}
	m.handleKey(keyMsg("down")) // clamp at len-1
	if m.cursor != 1 {
		t.Errorf("cursor should clamp at 1, got %d", m.cursor)
	}
	m.handleKey(keyMsg("up"))
	if m.cursor != 0 {
		t.Errorf("after up: want 0, got %d", m.cursor)
	}
	wasFollowing := m.stream.Following()
	m.handleKey(keyMsg("f"))
	if m.stream.Following() == wasFollowing {
		t.Error("f should toggle follow")
	}
}

func TestModel_QuitKey(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{status: &afclient.DaemonStatusResponse{}}, "")
	_, cmd := m.handleKey(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q should return a quit command")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// drainTails runs one synchronous tail poll through the model's update path.
func drainTails(t *testing.T, m *Model) {
	t.Helper()
	cmd := m.pollTails()
	if cmd == nil {
		return
	}
	msg := cmd()
	batch, ok := msg.(tailBatchMsg)
	if !ok {
		t.Fatalf("pollTails: want tailBatchMsg, got %T", msg)
	}
	m.applyTailBatch(batch.events)
}

func findCard(m *Model, id string) *SessionCard {
	for i := range m.cards {
		if m.cards[i].SessionID == id {
			return &m.cards[i]
		}
	}
	return nil
}

func keyMsg(s string) tea.KeyPressMsg {
	switch s {
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	default:
		return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
}
