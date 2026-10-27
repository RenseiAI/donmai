package hostwatch

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	for _, want := range []string{"2 sessions", "held-123", "live-123", "held", "awaiting local execution"} {
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

// ── host-watch card filter ───────────────────────────────────────────────────

// filterTestCards returns four cards with disjoint sanitized surfaces so
// each of the four allowed match fields can be pinned independently.
func filterTestCards() []SessionCard {
	return []SessionCard{
		{SessionID: "sess-alpha-0001", IssueIdentifier: "ENG-1001", IssueTitle: "Fix flaky upload retry", Repository: "https://github.com/acme/web.git", WorkType: "development", DaemonState: "running"},
		{SessionID: "sess-beta-0002", IssueIdentifier: "ENG-2002", IssueTitle: "Billing ledger migration", Repository: "https://github.com/acme/billing.git", WorkType: "qa", DaemonState: "running"},
		{SessionID: "sess-gamma-0003", IssueIdentifier: "ENG-3003", IssueTitle: "日本語の更新", Repository: "https://github.com/acme/docs.git", WorkType: "research", DaemonState: "running"},
		{SessionID: "sess-delta-0004", IssueIdentifier: "ENG-4004", IssueTitle: "Tighten session cache", Repository: "https://github.com/acme/api.git", WorkType: "development", DaemonState: "running", Handle: "ops-lead"},
	}
}

// newFilterModel builds a sized plain model over filterTestCards without a
// daemon, so the tests drive the production Update + render path only.
func newFilterModel(t *testing.T, width, height int) *Model {
	t.Helper()
	m := New(Options{Plain: true, Now: func() time.Time { return fixtureNow }, Handles: mapHandleEnricher{"sess-delta-0004": "ops-lead"}})
	_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	return m
}

// feedQuery feeds one production Update per rune.
func feedQuery(m *Model, query string) {
	for _, r := range query {
		_, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func feedKey(m *Model, msg tea.KeyPressMsg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

func visibleIDs(m *Model) []string {
	vis := m.visibleCards()
	out := make([]string, len(vis))
	for i, c := range vis {
		out[i] = c.SessionID
	}
	return out
}

func equalIDs(a, b []string) bool {
	return slices.Equal(a, b)
}

// TestModel_FilterNarrowsAndRestores proves AC-1 through production Update:
// "/" enters filtering, typed runes live-narrow across display identity,
// title, repository, and work type, backspace deletes one rune, and escape
// restores every card in snapshot order. Bypassing the match predicate
// (always true) keeps every non-match visible and fails this test.
func TestModel_FilterNarrowsAndRestores(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"display identity issue id", "ENG-2002", []string{"sess-beta-0002"}},
		{"display identity handle", "ops-lead", []string{"sess-delta-0004"}},
		{"title match", "ledger", []string{"sess-beta-0002"}},
		{"repository match", "billing", []string{"sess-beta-0002"}},
		{"work type match", "research", []string{"sess-gamma-0003"}},
		{"case insensitive", "LEDGER", []string{"sess-beta-0002"}},
		{"unicode title", "日本語", []string{"sess-gamma-0003"}},
		{"snapshot order preserved", "ENG-", []string{"sess-alpha-0001", "sess-beta-0002", "sess-gamma-0003"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newFilterModel(t, 120, 40)
			if got := visibleIDs(m); len(got) != 4 {
				t.Fatalf("unfiltered visible = %v, want 4 cards", got)
			}
			feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
			if !m.filterActive {
				t.Fatal("\"/\" did not enter filter mode")
			}
			feedQuery(m, tc.query)
			if got := visibleIDs(m); !equalIDs(got, tc.want) {
				t.Fatalf("query %q visible = %v, want %v", tc.query, got, tc.want)
			}
			view := m.render()
			if len(tc.want) == 1 {
				for _, id := range tc.want {
					c := findCard(m, id)
					if c == nil {
						t.Fatalf("source card %s missing", id)
					}
					if !strings.Contains(view, c.displayID()) {
						t.Errorf("render missing visible %q:\n%s", c.displayID(), view)
					}
				}
			}
			for _, c := range m.cards {
				isWant := false
				for _, w := range tc.want {
					if c.SessionID == w {
						isWant = true
					}
				}
				if !isWant {
					for _, v := range m.visibleCards() {
						if v.SessionID == c.SessionID {
							t.Errorf("non-match %s remains visible for query %q", c.SessionID, tc.query)
						}
					}
				}
			}
			feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.filterActive || m.filterQuery != "" {
				t.Fatalf("escape left filter active=%v query=%q", m.filterActive, m.filterQuery)
			}
			if got := visibleIDs(m); !equalIDs(got, []string{"sess-alpha-0001", "sess-beta-0002", "sess-gamma-0003", "sess-delta-0004"}) {
				t.Fatalf("after escape visible = %v, want snapshot order", got)
			}
			if len(m.cards) != 4 {
				t.Fatalf("source cards mutated by filter: %d", len(m.cards))
			}
		})
	}
}

// TestModel_FilterBackspaceDeletesOneRune proves rune-safe backspace
// through production Update.
func TestModel_FilterBackspaceDeletesOneRune(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "日本語")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("cjk query visible = %v", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.filterQuery != "日本" {
		t.Fatalf("backspace query = %q, want one rune deleted", m.filterQuery)
	}
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("after one backspace visible = %v, want gamma only", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.filterQuery != "" {
		t.Fatalf("query = %q, want empty", m.filterQuery)
	}
	if got := visibleIDs(m); len(got) != 4 {
		t.Fatalf("empty query visible = %v, want all 4", got)
	}
}

// TestModel_FilterConsumesPrintableShortcuts proves filter mode consumes
// printable keys (q, f, navigation letters) as query text instead of
// running their global shortcuts.
func TestModel_FilterConsumesPrintableShortcuts(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	following := m.stream.Following()
	cmd := feedKey(m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd != nil {
		t.Error("\"q\" in filter mode returned a command (must not quit)")
	}
	if m.filterQuery != "q" {
		t.Fatalf("filter query = %q, want q", m.filterQuery)
	}
	if m.stream.Following() != following {
		t.Error("follow toggled while typing q in filter mode")
	}
	feedKey(m, tea.KeyPressMsg{Code: 'f', Text: "f"})
	if m.stream.Following() != following {
		t.Error("\"f\" toggled follow while filtering; it must be filter text")
	}
	if m.filterQuery != "qf" {
		t.Fatalf("filter query = %q, want qf", m.filterQuery)
	}
	feedKey(m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.filterQuery != "qfj" {
		t.Fatalf("filter query = %q, want qfj", m.filterQuery)
	}
	if len(m.visibleCards()) != 0 {
		t.Fatalf("qfj should match nothing, visible = %v", visibleIDs(m))
	}
}

// TestModel_FilterSelectionPreservedAcrossEditsAndSnapshots proves AC-2
// through production Update: the same SessionID stays selected while
// visible across query edits and a reordered snapshot; exclusion selects
// the first match; no match selects none; enter/detail never indexes the
// unfiltered slice. Retaining the old numeric cursor selects the wrong
// session and fails this test.
func TestModel_FilterSelectionPreservedAcrossEditsAndSnapshots(t *testing.T) {
	m := newFilterModel(t, 120, 40)
	feedKey(m, keyMsg("down"))
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("selected = %s, want sess-beta-0002", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after / selected = %s, want beta", got)
	}
	feedQuery(m, "ENG-")
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("query ENG- selected = %s, want beta by id", got)
	}
	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (beta index in full list)", m.cursor)
	}
	reordered := []SessionCard{filterTestCards()[3], filterTestCards()[2], filterTestCards()[1], filterTestCards()[0]}
	m.applySnapshot(Snapshot{Cards: reordered, Counters: Counters{Sessions: 4, Running: 4}})
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after reorder selected = %s, want beta by id", got)
	}
	if m.cursor != 1 {
		t.Fatalf("after reorder cursor = %d, want 1 (beta by id in ENG- visible)", m.cursor)
	}
	m.applySnapshot(Snapshot{Cards: filterTestCards(), Counters: Counters{Sessions: 4, Running: 4}})
	for m.filterQuery != "" {
		feedKey(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	if got := m.selectedVisibleID(); got != "sess-beta-0002" {
		t.Fatalf("after clear selected = %s, want beta", got)
	}
	feedQuery(m, "research")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-gamma-0003"}) {
		t.Fatalf("research visible = %v", got)
	}
	if got := m.selectedVisibleID(); got != "sess-gamma-0003" {
		t.Fatalf("excluded beta must fall back to first match gamma, got %s", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.detail {
		t.Fatal("enter did not open detail for the visible match")
	}
	view := m.render()
	if !strings.Contains(view, "sess-gamma-0003") && !strings.Contains(view, "日本") {
		t.Fatalf("detail shows wrong session (must be gamma):\n%s", view)
	}
	if strings.Contains(view, "sess-alpha-0001") {
		t.Fatalf("detail indexed the unfiltered slice (alpha leaks):\n%s", view)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "development")
	if got := visibleIDs(m); !equalIDs(got, []string{"sess-alpha-0001", "sess-delta-0004"}) {
		t.Fatalf("development visible = %v", got)
	}
	if got := m.selectedVisibleID(); got != "sess-alpha-0001" {
		t.Fatalf("development first match = %s, want alpha", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.selectedVisibleID(); got != "sess-delta-0004" {
		t.Fatalf("after down selected = %s, want delta", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.selectedVisibleID(); got != "sess-alpha-0001" {
		t.Fatalf("after up selected = %s, want alpha", got)
	}
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	feedQuery(m, "zzz-no-such-session")
	if len(m.visibleCards()) != 0 {
		t.Fatalf("no-match visible = %v, want empty", visibleIDs(m))
	}
	m.detail = false
	feedKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.detail {
		t.Error("enter with no match must not open detail")
	}
	_ = m.render()
}

// TestModel_FilterViewWidthBoundedAndSourceUnchanged proves AC-3 through
// production View: empty and no-match filters stay width-bounded at 40,
// 80, 120, and 200 columns in plain and styled modes while m.cards,
// counters, tailers, and stream content stay unchanged; clearing restores
// all cards. Filtering m.cards in place breaks the restore and fails.
func TestModel_FilterViewWidthBoundedAndSourceUnchanged(t *testing.T) {
	for _, width := range []int{40, 80, 120, 200} {
		for _, plain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx-plain=%v", width, plain), func(t *testing.T) {
				m := newFixtureModel(t, width, 40, plain, 0)
				wantCards := len(m.cards)
				wantCounters := m.counters
				wantTailers := len(m.tailers)
				wantStream := strings.Join(m.stream.View(width, 10), "\n")
				wantIDs := make([]string, len(m.cards))
				for i, c := range m.cards {
					wantIDs[i] = c.SessionID
				}
				checkBounded := func(when string) {
					t.Helper()
					view := m.render()
					for i, line := range strings.Split(view, "\n") {
						if w := lipgloss.Width(line); w > width {
							t.Fatalf("%s width %d row %d is %d cells: %q", when, width, i, w, line)
						}
					}
					if plain && strings.Contains(view, "\x1b") {
						t.Fatalf("%s plain frame carries escapes", when)
					}
				}
				feedKey(m, tea.KeyPressMsg{Code: '/', Text: "/"})
				if len(m.visibleCards()) != wantCards {
					t.Fatalf("empty filter visible = %d, want %d", len(m.visibleCards()), wantCards)
				}
				checkBounded("empty filter")
				for _, r := range "zzz-no-such-xyz" {
					feedKey(m, tea.KeyPressMsg{Code: r, Text: string(r)})
				}
				if len(m.visibleCards()) != 0 {
					t.Fatalf("no-match visible = %d, want 0", len(m.visibleCards()))
				}
				checkBounded("no-match filter")
				if len(m.cards) != wantCards {
					t.Fatalf("m.cards mutated: %d, want %d", len(m.cards), wantCards)
				}
				for i, c := range m.cards {
					if c.SessionID != wantIDs[i] {
						t.Fatalf("m.cards order mutated at %d: %s, want %s", i, c.SessionID, wantIDs[i])
					}
				}
				if m.counters != wantCounters {
					t.Fatalf("counters mutated: %+v, want %+v", m.counters, wantCounters)
				}
				if len(m.tailers) != wantTailers {
					t.Fatalf("tailers mutated: %d, want %d", len(m.tailers), wantTailers)
				}
				if got := strings.Join(m.stream.View(width, 10), "\n"); got != wantStream {
					t.Fatalf("stream mutated:\n%s\nwant:\n%s", got, wantStream)
				}
				feedKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
				if len(m.visibleCards()) != wantCards {
					t.Fatalf("after escape visible = %d, want %d", len(m.visibleCards()), wantCards)
				}
				for i, c := range m.visibleCards() {
					if c.SessionID != wantIDs[i] {
						t.Fatalf("restore order mutated at %d: %s", i, c.SessionID)
					}
				}
				checkBounded("restored")
			})
		}
	}
}
