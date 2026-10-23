package hostwatch

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/tui-components/theme"
)

// mapHandleEnricher is a test HandleEnricher backed by a map. Missing ids
// report no handle.
type mapHandleEnricher map[string]string

func (m mapHandleEnricher) Handle(sessionID string) string { return m[sessionID] }

// countingHandleEnricher wraps a map and counts Handle calls.
type countingHandleEnricher struct {
	mu    sync.Mutex
	calls int
	m     map[string]string
}

func (c *countingHandleEnricher) Handle(sessionID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.m[sessionID]
}

func (c *countingHandleEnricher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newHandleTestModel(t *testing.T, fd *fakeDaemon, h HandleEnricher) *Model {
	t.Helper()
	src := NewSource(fd, state.NewStore(), "")
	m := New(Options{
		Source:        src,
		Plain:         true,
		IndexInterval: time.Millisecond,
		TailInterval:  time.Millisecond,
		Now:           func() time.Time { return time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) },
		Handles:       h,
	})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return mm.(*Model)
}

// TestModel_HandleEnricherPrefersHandle drives the real snapshot + tail
// path: the handled cards render their handles as their identity and its stream
// lines carry the handle prefix, while the other cards fall back. Held rows
// resolve handles through the same snapshot path.
func TestModel_HandleEnricherPrefersHandle(t *testing.T) {
	root := t.TempDir()
	wtA := filepath.Join(root, "a")
	wtB := filepath.Join(root, "b")
	wtC := filepath.Join(root, "c")
	writeState(t, wtA, state.State{SessionID: "sess-handled-long-id-aaaa", StartedAt: 1})
	writeState(t, wtB, state.State{SessionID: "sess-plain-long-id-bbbb", StartedAt: 1})
	writeState(t, wtC, state.State{SessionID: "abcdef0123456789-long", StartedAt: 1})
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "sess-handled-long-id-aaaa", State: "running", Repository: "o/a", WorktreePath: wtA, IssueIdentifier: "ENG-100"},
			{SessionID: "sess-plain-long-id-bbbb", State: "running", Repository: "o/a", WorktreePath: wtB, IssueIdentifier: "ENG-200"},
			{SessionID: "abcdef0123456789-long", State: "running", Repository: "o/a", WorktreePath: wtC},
			{SessionID: "held-handled-long-id-dddd", State: "unknown", Repository: "o/a", IssueIdentifier: "ENG-300"},
		},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	m := newHandleTestModel(t, fd, mapHandleEnricher{"sess-handled-long-id-aaaa": "ops-lead", "held-handled-long-id-dddd": "held-lead"})

	m.applySnapshot(m.src.Snapshot())

	if got := findCard(m, "sess-handled-long-id-aaaa"); got == nil || got.displayID() != "ops-lead" {
		t.Fatalf("handled card display = %v, want ops-lead", got)
	}
	if got := findCard(m, "sess-plain-long-id-bbbb"); got == nil || got.displayID() != "ENG-200" {
		t.Fatalf("unhandled card display = %v, want ENG-200", got)
	}
	if got := findCard(m, "abcdef0123456789-long"); got == nil || got.displayID() != "abcdef01" {
		t.Fatalf("bare card display = %v, want short id abcdef01", got)
	}
	if got := findCard(m, "held-handled-long-id-dddd"); got == nil || got.displayID() != "held-lead" {
		t.Fatalf("held handled card display = %v, want held-lead", got)
	}

	// Card title and detail title follow the handle.
	now := time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC)
	handled := findCard(m, "sess-handled-long-id-aaaa")
	rows := cardText(*handled, false, true, now, 80)
	if !strings.Contains(rows[0], "ops-lead") {
		t.Errorf("handled card first row = %q, want ops-lead", rows[0])
	}
	if got := detailTitle(*handled, true, theme.DefaultTheme(), 80); !strings.Contains(got, "ops-lead") {
		t.Errorf("detail title = %q, want ops-lead", got)
	}
	if held := findCard(m, "held-handled-long-id-dddd"); held == nil {
		t.Fatal("held handled card missing from snapshot")
	} else {
		heldRows := cardText(*held, false, true, now, 80)
		if !strings.Contains(heldRows[0], "held-lead") {
			t.Errorf("held card first row = %q, want held-lead", heldRows[0])
		}
		if got := m.labels["held-handled-long-id-dddd"]; got != "held-lead" {
			t.Errorf("held stream label = %q, want held-lead", got)
		}
	}

	// Stream prefix follows the handle on the real tail path.
	evPath := filepath.Join(wtA, state.AgentDirName, "events.jsonl")
	writeEvents(t, evPath, agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "pnpm test"}})
	drainTails(t, m)
	evPathB := filepath.Join(wtB, state.AgentDirName, "events.jsonl")
	writeEvents(t, evPathB, agent.ToolUseEvent{ToolName: "Read", Input: map[string]any{"file_path": "a.go"}})
	drainTails(t, m)
	lines := m.stream.View(120, 0)
	foundHandle, foundIssue := false, false
	for _, line := range lines {
		if strings.Contains(line, "[ops-lead]") {
			foundHandle = true
		}
		if strings.Contains(line, "[ENG-200]") {
			foundIssue = true
		}
	}
	if !foundHandle {
		t.Errorf("stream missing [ops-lead] prefix: %q", lines)
	}
	if !foundIssue {
		t.Errorf("stream missing [ENG-200] fallback prefix: %q", lines)
	}
	if got := m.labels["sess-handled-long-id-aaaa"]; got != "ops-lead" {
		t.Errorf("stream label = %q, want ops-lead", got)
	}
}

// TestModel_HandleEnricherCalledOnlyFromSnapshot pins that the hook runs on
// the snapshot path only: tail ticks never call it.
func TestModel_HandleEnricherCalledOnlyFromSnapshot(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "s")
	writeState(t, wt, state.State{SessionID: "sess-one", StartedAt: 1})
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "sess-one", State: "running", Repository: "o/a", WorktreePath: wt, IssueIdentifier: "ENG-1"},
		},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	counting := &countingHandleEnricher{m: map[string]string{"sess-one": "ops-lead"}}
	m := newHandleTestModel(t, fd, counting)

	m.applySnapshot(m.src.Snapshot())
	afterSnapshot := counting.count()
	if afterSnapshot == 0 {
		t.Fatal("enricher was not consulted on snapshot")
	}
	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")
	writeEvents(t, evPath, agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test"}})
	drainTails(t, m)
	writeEvents(t, evPath, agent.AssistantTextEvent{Text: "more output"})
	drainTails(t, m)
	if got := counting.count(); got != afterSnapshot {
		t.Errorf("enricher calls grew across tail ticks: %d -> %d", afterSnapshot, got)
	}
	// A second snapshot consults the hook again (late fills are picked up).
	m.applySnapshot(m.src.Snapshot())
	if got := counting.count(); got <= afterSnapshot {
		t.Errorf("second snapshot did not re-read handles: %d -> %d", afterSnapshot, got)
	}
}

// TestDisplayID_HandleSanitized pins that an untrusted handle renders only
// through cleanText: escapes and controls never reach the card or stream.
func TestDisplayID_HandleSanitized(t *testing.T) {
	c := SessionCard{SessionID: "sess-1", IssueIdentifier: "ENG-9", Handle: "\x1b[31mred\x1b[0m"}
	if got := c.displayID(); got != "red" {
		t.Errorf("ansi handle display = %q, want red", got)
	}
	c = SessionCard{SessionID: "sess-1", IssueIdentifier: "ENG-9", Handle: "  \t\n "}
	if got := c.displayID(); got != "ENG-9" {
		t.Errorf("whitespace handle display = %q, want ENG-9 fallback", got)
	}
	c = SessionCard{SessionID: "sess-1", IssueIdentifier: "ENG-9", Handle: "a\nb"}
	if got := c.displayID(); got != "a b" {
		t.Errorf("newline handle display = %q, want collapsed", got)
	}
	// Grouping never follows the handle.
	c = SessionCard{SessionID: "sess-1", IssueIdentifier: "ENG-9", Handle: "ops-lead"}
	if got := c.GroupKey(); got != "ENG-9" {
		t.Errorf("GroupKey = %q, want ENG-9 (handle must not regroup)", got)
	}
}

// TestHandleEnricher_ContextRoundTrip pins the embedder seam: the hook
// travels on the command context and defaults to nil.
func TestHandleEnricher_ContextRoundTrip(t *testing.T) {
	if got := HandleEnricherFrom(context.Background()); got != nil {
		t.Errorf("empty context carries %v, want nil", got)
	}
	h := mapHandleEnricher{"a": "ops-lead"}
	ctx := WithHandleEnricher(context.Background(), h)
	if got := HandleEnricherFrom(ctx); got == nil || got.Handle("a") != "ops-lead" {
		t.Errorf("round trip failed, got %v", got)
	}
}

// TestDisplayID_PreferenceOrder pins handle > issue > short id for both
// live and held rows.
func TestDisplayID_PreferenceOrder(t *testing.T) {
	tests := []struct {
		name string
		card SessionCard
		want string
	}{
		{"handle wins over issue", SessionCard{SessionID: "sess-long-id-aaaa", IssueIdentifier: "ENG-1", Handle: "ops-lead"}, "ops-lead"},
		{"issue fallback", SessionCard{SessionID: "sess-long-id-aaaa", IssueIdentifier: "ENG-1"}, "ENG-1"},
		{"short id fallback", SessionCard{SessionID: "abcdef0123456789"}, "abcdef01"},
		{"held handle", SessionCard{SessionID: "sess-long-id-aaaa", DaemonState: "unknown", IssueIdentifier: "ENG-1", Handle: "ops-lead"}, "ops-lead"},
		{"held issue fallback", SessionCard{SessionID: "sess-long-id-aaaa", DaemonState: "unknown", IssueIdentifier: "ENG-1"}, "ENG-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.card.displayID(); got != tc.want {
				t.Errorf("displayID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestModel_HandleMapChangeUpdatesStreamPrefix pins that a handle arriving
// between two index snapshots updates both the card identity and the merged
// stream prefix on the real snapshot + tail path.
func TestModel_HandleMapChangeUpdatesStreamPrefix(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "s")
	writeState(t, wt, state.State{SessionID: "sess-late-handle", StartedAt: 1})
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "sess-late-handle", State: "running", Repository: "o/a", WorktreePath: wt, IssueIdentifier: "ENG-42"},
		},
		status: &afclient.DaemonStatusResponse{MaxSessions: 8},
	}
	handles := mapHandleEnricher{}
	m := newHandleTestModel(t, fd, handles)

	m.applySnapshot(m.src.Snapshot())
	if got := findCard(m, "sess-late-handle"); got == nil || got.displayID() != "ENG-42" {
		display := "<missing>"
		if got != nil {
			display = got.displayID()
		}
		t.Fatalf("before fill display = %q, want ENG-42", display)
	}
	if got := m.labels["sess-late-handle"]; got != "ENG-42" {
		t.Fatalf("before fill label = %q, want ENG-42", got)
	}

	handles["sess-late-handle"] = "late-lead"
	m.applySnapshot(m.src.Snapshot())
	if got := findCard(m, "sess-late-handle"); got == nil || got.displayID() != "late-lead" {
		display := "<missing>"
		if got != nil {
			display = got.displayID()
		}
		t.Fatalf("after fill display = %q, want late-lead", display)
	}
	if got := m.labels["sess-late-handle"]; got != "late-lead" {
		t.Fatalf("after fill label = %q, want late-lead", got)
	}

	evPath := filepath.Join(wt, state.AgentDirName, "events.jsonl")
	writeEvents(t, evPath, agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "go test"}})
	drainTails(t, m)
	lines := m.stream.View(120, 0)
	found := false
	for _, line := range lines {
		if strings.Contains(line, "[late-lead]") {
			found = true
		}
		if strings.Contains(line, "[ENG-42]") {
			t.Errorf("stale fallback prefix survives handle fill: %q", line)
		}
	}
	if !found {
		t.Errorf("stream missing [late-lead] prefix after fill: %q", lines)
	}
}
