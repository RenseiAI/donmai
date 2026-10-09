package hostwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/charmbracelet/x/ansi"
)

// journalFixture writes a session worktree whose state.json fences the run
// at offset 0 and whose journal holds n tool calls.
func journalFixture(t *testing.T, root, id string, n int) string {
	t.Helper()
	wt := filepath.Join(root, id)
	zero := int64(0)
	writeState(t, wt, state.State{SessionID: id, IssueIdentifier: "ENG-" + id, StartedAt: 1, EventLogStartOffset: &zero})
	var journal []byte
	for i := 0; i < n; i++ {
		line, err := agent.MarshalEvent(agent.ToolUseEvent{ToolName: "Read", Input: map[string]any{"file_path": fmt.Sprintf("f%d.go", i)}})
		if err != nil {
			t.Fatal(err)
		}
		journal = append(append(journal, line...), '\n')
	}
	// One write: the fixture's shape matters, not its write pattern.
	if err := os.WriteFile(filepath.Join(wt, state.AgentDirName, "events.jsonl"), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestPollTails_EveryTailerCatchesUpInOneTick: a watcher attaching to a
// busy host replays each session's current run. One tail tick must fold
// every session's backlog — not one session's slice while the others read
// "not reported" for seconds.
func TestPollTails_EveryTailerCatchesUpInOneTick(t *testing.T) {
	root := t.TempDir()
	var handles []afclient.DaemonSessionHandle
	for _, id := range []string{"a", "b", "c"} {
		handles = append(handles, afclient.DaemonSessionHandle{SessionID: id, State: "running", WorktreePath: journalFixture(t, root, id, 600)})
	}
	m := newTestModel(t, &fakeDaemon{sessions: handles}, "")
	m.applySnapshot(m.src.Snapshot())
	drainTails(t, m)
	for _, id := range []string{"a", "b", "c"} {
		if c := findCard(m, id); c == nil || c.ToolCalls != 600 || !c.Observed {
			t.Errorf("session %s after one tick: %+v", id, c)
		}
	}
}

// TestTailTick_PollsNeverOverlap: a catch-up poll can outlast the tail
// tick. A second poll started while the first is in flight races it over
// the same tailers, and whichever finishes last is applied last — so a card
// could fold an older activity (or cumulative cost and turns) over a newer
// one and keep it until the session's next event. While a poll is in
// flight, a tick must only reschedule itself.
func TestTailTick_PollsNeverOverlap(t *testing.T) {
	root := t.TempDir()
	wt := journalFixture(t, root, "busy", 3)
	journal := filepath.Join(wt, state.AgentDirName, "events.jsonl")
	m := newTestModel(t, &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "busy", State: "running", WorktreePath: wt}}}, "")
	m.applySnapshot(m.src.Snapshot())

	// A second tick fires before the first tick's poll has delivered.
	_, first := m.Update(tailTickMsg{})
	_, second := m.Update(tailTickMsg{})
	firstBatches := runTailCmds(first)
	writeEvents(t, journal, agent.ToolUseEvent{ToolName: "Edit", Input: map[string]any{"file_path": "newest.go"}})
	secondBatches := runTailCmds(second)
	// The later poll finishes first.
	for _, b := range append(secondBatches, firstBatches...) {
		m.Update(b)
	}
	// The next tick reads whatever is still unread.
	_, next := m.Update(tailTickMsg{})
	for _, b := range runTailCmds(next) {
		m.Update(b)
	}
	c := findCard(m, "busy")
	if c.ToolCalls != 4 || !strings.HasPrefix(c.LastActivity, "Edit") {
		t.Fatalf("after overlapping ticks: tools=%d activity=%q, want 4 tools ending at the newest Edit", c.ToolCalls, c.LastActivity)
	}
}

// runTailCmds runs a tick's commands synchronously and returns the tail
// batches they produced, dropping rescheduled ticks.
func runTailCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			out = append(out, runTailCmds(c)...)
		}
	case tailBatchMsg:
		out = append(out, msg)
	}
	return out
}

// TestAttach_SeedsLastOutputFromJournal: a quiet session whose run already
// wrote events shows how long ago it last wrote (the journal's mtime), not
// "never", while replayed events still never advance freshness.
func TestAttach_SeedsLastOutputFromJournal(t *testing.T) {
	root := t.TempDir()
	wt := journalFixture(t, root, "quiet", 3)
	journal := filepath.Join(wt, state.AgentDirName, "events.jsonl")
	now := time.Date(2026, 6, 13, 14, 2, 11, 0, time.UTC) // newTestModel's clock
	lastWrite := now.Add(-4 * time.Minute)
	if err := os.Chtimes(journal, lastWrite, lastWrite); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "quiet", State: "running", WorktreePath: wt}}}, "")
	m.applySnapshot(m.src.Snapshot())
	drainTails(t, m)
	c := findCard(m, "quiet")
	if !c.LastOutputAt.Equal(lastWrite) || !c.LastWorkAt.IsZero() {
		t.Fatalf("attach seed: output=%v work=%v, want output=%v and no work freshness", c.LastOutputAt, c.LastWorkAt, lastWrite)
	}
	rows := cardText(*c, false, true, now, 80)
	if !strings.HasPrefix(strings.TrimSpace(rows[4]), "4m ago · Read: f2.go") {
		t.Errorf("activity row = %q, want the journal age and last tool", rows[4])
	}
	// The seed survives index refreshes of the same run.
	m.applySnapshot(m.src.Snapshot())
	if c := findCard(m, "quiet"); !c.LastOutputAt.Equal(lastWrite) {
		t.Errorf("refresh dropped the seeded output time: %v", c.LastOutputAt)
	}
}

// TestAttach_NoSeedWithoutThisRunsBytes: no fence, or a fence at the end of
// the journal (no bytes from this run yet), means the mtime belongs to an
// unknown or earlier run and must not be attributed to this one.
func TestAttach_NoSeedWithoutThisRunsBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEvents(t, path, agent.SystemEvent{Subtype: "earlier-run"})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	end := info.Size()
	zero := int64(0)
	for name, offset := range map[string]*int64{"no fence": nil, "fence at end": &end} {
		if got := NewMetricTailer("s", path, offset, time.Now).AttachModTime(); !got.IsZero() {
			t.Errorf("%s: seeded %v", name, got)
		}
	}
	if NewMetricTailer("s", path, &zero, time.Now).AttachModTime().IsZero() {
		t.Error("a run with bytes in the journal must seed its mtime")
	}
}

// TestObserved_SystemNoticesAloneKeepToolsUnreported: an interactive
// terminal session's journal holds only system notices; its tool count is
// not reported, not a measured zero.
func TestObserved_SystemNoticesAloneKeepToolsUnreported(t *testing.T) {
	m := newTestModel(t, &fakeDaemon{}, "")
	m.cards = []SessionCard{{SessionID: "pty", DaemonState: "running"}}
	m.foldMetrics(TailEvent{SessionID: "pty", Event: agent.SystemEvent{Subtype: "interactive-session-started"}})
	if got := detailValue(detailFields(m.cards[0], time.Now()), "Tools"); got != "not reported" {
		t.Fatalf("system-only journal reported tools %q", got)
	}
	m.foldMetrics(TailEvent{SessionID: "pty", Event: agent.InitEvent{}})
	if got := detailValue(detailFields(m.cards[0], time.Now()), "Tools"); got != "0" {
		t.Fatalf("agent stream started: tools %q, want a measured 0", got)
	}
}

// TestStreamPane_FollowPauseAndWidth: following shows the newest rows;
// paused holds its window while lines keep arriving; rows are cut to the
// pane width in cells and the pane always returns exactly its height.
func TestStreamPane_FollowPauseAndWidth(t *testing.T) {
	s := newStreamPane()
	for i := 0; i < 5; i++ {
		s.Append(fmt.Sprintf("line %d", i))
	}
	if got := strings.Join(s.View(20, 2), "|"); got != "line 3|line 4" {
		t.Fatalf("follow window = %q", got)
	}
	s.SetFollowing(false)
	s.Append("line 5", "line 6")
	if got := strings.Join(s.View(20, 2), "|"); got != "line 3|line 4" {
		t.Fatalf("paused window moved: %q", got)
	}
	s.SetFollowing(true)
	if got := strings.Join(s.View(20, 3), "|"); got != "line 4|line 5|line 6" {
		t.Fatalf("resumed window = %q", got)
	}
	if got := s.View(20, 9); len(got) != 9 {
		t.Fatalf("pane height: %d rows", len(got))
	}
	s.Append("日本語のログ行がとても長い")
	if got := s.View(9, 1)[0]; ansi.StringWidth(got) > 9 || !strings.HasSuffix(got, "…") {
		t.Fatalf("cell truncation: %q (%d cells)", got, ansi.StringWidth(got))
	}
	s.max = 3
	s.Append("x", "y")
	if got := strings.Join(s.View(60, 0), "|"); got != "日本語のログ行がとても長い|x|y" {
		t.Fatalf("ring drop: %q", got)
	}
}
