package hostwatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/state"
)

// fakeDaemon is a test daemonLister.
type fakeDaemon struct {
	sessions []afclient.DaemonSessionHandle
	sessErr  error
	status   *afclient.DaemonStatusResponse
	statusEr error
	stats    *afclient.DaemonStatsResponse
}

func (f *fakeDaemon) GetSessions() ([]afclient.DaemonSessionHandle, error) {
	return f.sessions, f.sessErr
}

func (f *fakeDaemon) GetStatus() (*afclient.DaemonStatusResponse, error) {
	return f.status, f.statusEr
}

func (f *fakeDaemon) GetStats(_, _ bool) (*afclient.DaemonStatsResponse, error) {
	return f.stats, nil
}

// writeState writes a state.json into <worktree>/.agent/state.json.
func writeState(t *testing.T, worktree string, st state.State) {
	t.Helper()
	dir := filepath.Join(worktree, state.AgentDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, state.StateFileName), body, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestSource_Snapshot_ScopeFilter(t *testing.T) {
	tests := []struct {
		name      string
		scope     string
		repos     []string
		wantCount int
	}{
		{"no scope = all", "", []string{"o/a", "o/b"}, 2},
		{"exact match", "o/a", []string{"o/a", "o/b"}, 1},
		{"slug suffix of url", "donmai", []string{"https://github.com/RenseiAI/donmai", "o/b"}, 1},
		{"no match", "o/z", []string{"o/a", "o/b"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var handles []afclient.DaemonSessionHandle
			for i, repo := range tc.repos {
				handles = append(handles, afclient.DaemonSessionHandle{
					SessionID:  string(rune('a' + i)),
					State:      "running",
					Repository: repo,
				})
			}
			fd := &fakeDaemon{sessions: handles}
			src := NewSource(fd, &noopState{}, tc.scope)
			snap := src.Snapshot()
			if snap.Err != nil {
				t.Fatalf("unexpected err: %v", snap.Err)
			}
			if len(snap.Cards) != tc.wantCount {
				t.Fatalf("want %d cards, got %d", tc.wantCount, len(snap.Cards))
			}
		})
	}
}

func TestSource_Snapshot_HeldRowsRespectRepositoryScope(t *testing.T) {
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{
		{
			SessionID: "held-known", State: "unknown", AcceptedAt: "2026-09-27T12:00:00Z",
			ProjectName: "donmai", Repository: "https://github.com/RenseiAI/donmai", PID: 9876,
			WorktreePath: "/fixture/must-not-be-used",
		},
		{SessionID: "held-unrelated", State: "unknown", ProjectName: "other", Repository: "", PID: 0},
	}}

	state := &countingState{}
	scoped := NewSource(fd, state, "RenseiAI/donmai").Snapshot()
	if len(scoped.Cards) != 1 {
		t.Fatalf("scoped snapshot should retain only the matching held row, got %d", len(scoped.Cards))
	}
	c := scoped.Cards[0]
	if c.SessionID != "held-known" || c.DaemonState != "unknown" || c.PID != 0 || c.WorktreePath != "" {
		t.Fatalf("held row identity/state/path changed: %#v", c)
	}
	if c.ProjectName != "donmai" || c.Repository != "https://github.com/RenseiAI/donmai" {
		t.Fatalf("held row project/repository metadata lost: %#v", c)
	}
	if c.AcceptedAt != "2026-09-27T12:00:00Z" {
		t.Errorf("original AcceptedAt must be preserved, got %q", c.AcceptedAt)
	}
	if state.calls != 0 {
		t.Errorf("held row must not read workarea state, got %d reads", state.calls)
	}

	unscoped := NewSource(fd, &noopState{}, "").Snapshot()
	if len(unscoped.Cards) != 2 {
		t.Fatalf("unscoped snapshot should include both held rows, got %d", len(unscoped.Cards))
	}
}

func TestSourceCountersSeparateLiveFromHeldAndTerminal(t *testing.T) {
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{
		{SessionID: "live", State: "running", Repository: "o/a"},
		{SessionID: "starting", State: "starting", Repository: "o/a"},
		{SessionID: "held", State: "unknown", Repository: "o/a"},
		{SessionID: "terminal", State: "completed", Repository: "o/a"},
		{SessionID: "unreported", Repository: "o/a"},
		{SessionID: "other", State: "running", Repository: "o/b"},
	}}
	snap := NewSource(fd, nil, "o/a").Snapshot()
	if snap.Err != nil || snap.Counters.Sessions != 5 || snap.Counters.Running != 2 {
		t.Fatalf("scoped counts claimed unresolved/terminal rows as live: %+v", snap)
	}
}

func TestSource_Snapshot_StateEnrichment(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-1")
	writeState(t, wt, state.State{
		IssueIdentifier: "ENG-42",
		IssueTitle:      "Fix flaky retry in the upload worker",
		ProviderName:    "claude",
		WorkType:        "development",
		CurrentStep:     "streaming",
		StartedAt:       1700000000000,
		PID:             4242,
	})

	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID:    "sess-1",
			State:        "running",
			Repository:   "o/a",
			WorktreePath: wt,
		}},
	}
	src := NewSource(fd, state.NewStore(), "")
	snap := src.Snapshot()
	if len(snap.Cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(snap.Cards))
	}
	c := snap.Cards[0]
	if c.IssueIdentifier != "ENG-42" {
		t.Errorf("issue: want ENG-42, got %q", c.IssueIdentifier)
	}
	if c.IssueTitle != "Fix flaky retry in the upload worker" {
		t.Errorf("title: state.json title not read, got %q", c.IssueTitle)
	}
	if c.Provider != "claude" {
		t.Errorf("provider: want claude, got %q", c.Provider)
	}
	if c.WorkType != "development" {
		t.Errorf("workType: want development, got %q", c.WorkType)
	}
	if c.GroupKey() != "ENG-42" {
		t.Errorf("groupKey: want ENG-42, got %q", c.GroupKey())
	}
	if c.EventsPath() != filepath.Join(wt, state.AgentDirName, "events.jsonl") {
		t.Errorf("eventsPath: got %q", c.EventsPath())
	}
}

// TestSource_HandleIssueIdentifierLabelsBeforeState: a fresh session is
// labeled with its issue from the daemon's handle before its runner writes
// state.json, and the handle's identifier wins over state.
func TestSource_HandleIssueIdentifierLabelsBeforeState(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh") // no .agent yet: still provisioning
	written := filepath.Join(dir, "written")
	writeState(t, written, state.State{SessionID: "written", IssueIdentifier: "ENG-STALE"})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{
		{SessionID: "fresh", State: "running", WorktreePath: fresh, IssueIdentifier: "ENG-7"},
		{SessionID: "written", State: "running", WorktreePath: written, IssueIdentifier: "ENG-8"},
	}}
	cards := NewSource(fd, state.NewStore(), "").Snapshot().Cards
	got := map[string]string{}
	for _, c := range cards {
		got[c.SessionID] = c.displayID()
	}
	if got["fresh"] != "ENG-7" || got["written"] != "ENG-8" {
		t.Errorf("handle identifiers not used: %v", got)
	}
}

func TestSource_Snapshot_MissingStateIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "no-state") // no .agent/state.json written
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{{
			SessionID:    "sess-1",
			State:        "running",
			Repository:   "o/a",
			WorktreePath: wt,
		}},
	}
	src := NewSource(fd, state.NewStore(), "")
	snap := src.Snapshot()
	if len(snap.Cards) != 1 {
		t.Fatalf("missing state should still render a card; got %d", len(snap.Cards))
	}
	if snap.Cards[0].GroupKey() != "sess-1" {
		t.Errorf("groupKey fallback: want sess-1, got %q", snap.Cards[0].GroupKey())
	}
}

func TestSource_Snapshot_ListError(t *testing.T) {
	fd := &fakeDaemon{sessErr: errors.New("connection refused")}
	src := NewSource(fd, &noopState{}, "")
	snap := src.Snapshot()
	if snap.Err == nil {
		t.Fatal("want a snapshot error when GetSessions fails")
	}
}

func TestSource_Counters(t *testing.T) {
	fd := &fakeDaemon{
		sessions: []afclient.DaemonSessionHandle{
			{SessionID: "a", State: "running", Repository: "o/a"},
		},
		status: &afclient.DaemonStatusResponse{
			ActiveSessions: 3,
			MaxSessions:    8,
			UptimeSeconds:  120,
			Version:        "0.39.0",
		},
		stats: &afclient.DaemonStatsResponse{QueueDepth: 2},
	}
	// Scoped: Running reflects the scoped card count, not the daemon total.
	scoped := NewSource(fd, &noopState{}, "o/a").Snapshot()
	if scoped.Counters.Sessions != 1 {
		t.Errorf("scoped sessions: want 1, got %d", scoped.Counters.Sessions)
	}
	if scoped.Counters.QueueDepth != 2 {
		t.Errorf("queue: want 2, got %d", scoped.Counters.QueueDepth)
	}
	if scoped.Counters.Version != "0.39.0" {
		t.Errorf("version: want 0.39.0, got %q", scoped.Counters.Version)
	}
	// Slots are host-wide whatever the scope: the daemon's occupied/total.
	if scoped.Counters.HostActive != 3 || scoped.Counters.MaxSessions != 8 {
		t.Errorf("host slots: want 3/8, got %d/%d", scoped.Counters.HostActive, scoped.Counters.MaxSessions)
	}
	// Unscoped: Sessions counts the rows shown in the grid, including held rows.
	unscoped := NewSource(fd, &noopState{}, "").Snapshot()
	if unscoped.Counters.Sessions != 1 {
		t.Errorf("unscoped sessions: want 1 visible row, got %d", unscoped.Counters.Sessions)
	}
}

func TestRepoMatch(t *testing.T) {
	tests := []struct {
		scope, repo string
		want        bool
	}{
		{"", "anything", true},
		{"o/a", "o/a", true},
		{"o/a", "o/b", false},
		{"donmai", "https://github.com/RenseiAI/donmai", true},
		{"RenseiAI/donmai", "donmai", true}, // repo "donmai" is the "/donmai" suffix of the scope
		{"o/a", "", false},
		// The daemon reports clone URLs; the CWD scope is owner/name. A
		// ".git" suffix, a scheme, an SSH remote or case must not hide a
		// session that belongs to the scope.
		{"o/a", "https://github.com/o/a.git", true},
		{"o/a", "https://github.com/o/a/", true},
		{"o/a", "git@github.com:o/a.git", true},
		{"o/a", "ssh://git@github.com/o/a.git", true},
		{"O/A", "https://github.com/o/a.git", true},
		{"https://github.com/o/a.git", "git@github.com:o/a.git", true},
		{"o/a", "https://github.com/o/ab.git", false},
		{"o/a", "https://github.com/x/o/a-fork.git", false},
	}
	for _, tc := range tests {
		if got := repoMatch(tc.scope, tc.repo); got != tc.want {
			t.Errorf("repoMatch(%q,%q)=%v want %v", tc.scope, tc.repo, got, tc.want)
		}
	}
}

// noopState is a stateReader that always reports not-found.
type noopState struct{}

func (noopState) Read(string) (*state.State, error) { return nil, state.ErrNotFound }

type countingState struct{ calls int }

func (s *countingState) Read(string) (*state.State, error) {
	s.calls++
	return nil, state.ErrNotFound
}

func TestSourcePreservesAdmittedDisplayAxesAndRunOffset(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "session")
	zero := int64(0)
	writeState(t, wt, state.State{
		SessionID: "session", StartedAt: 12, EventLogStartOffset: &zero,
		ModelAuthor: "meta", EndpointOperator: "operator", Protocol: "openai-chat",
	})
	fd := &fakeDaemon{sessions: []afclient.DaemonSessionHandle{{
		SessionID: "session", State: "running", WorktreePath: wt,
		ModelProvider: "openai", ModelAuthor: "meta", EndpointOperator: "operator", Protocol: "openai-chat",
	}}}
	snap := NewSource(fd, state.NewStore(), "").Snapshot()
	if len(snap.Cards) != 1 {
		t.Fatalf("cards=%d", len(snap.Cards))
	}
	card := snap.Cards[0]
	if card.ModelProvider != "openai" || card.ModelAuthor != "meta" || card.EndpointOperator != "operator" ||
		card.Protocol != "openai-chat" || card.EventLogStartOffset == nil || *card.EventLogStartOffset != 0 {
		t.Fatalf("admitted axes or valid zero recovery offset lost: %+v", card)
	}
	*card.EventLogStartOffset = 99
	again := NewSource(fd, state.NewStore(), "").Snapshot().Cards[0]
	if again.EventLogStartOffset == nil || *again.EventLogStartOffset != 0 {
		t.Fatal("card mutated persisted run offset")
	}
}

func TestSourceDoesNotInferEndpointSurfaceFromJournalFence(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			dir := t.TempDir()
			zero := int64(0)
			st := state.State{SessionID: "surface", StartedAt: 1, ModelProvider: "openai"}
			if known {
				st.EventLogStartOffset = &zero
			}
			writeState(t, dir, st)
			source := NewSource(&fakeDaemon{sessions: []afclient.DaemonSessionHandle{{SessionID: "surface", WorktreePath: dir}}}, state.NewStore(), "")
			card := source.Snapshot().Cards[0]
			want := ""
			if card.ModelProvider != want {
				t.Fatalf("known writer=%v surface=%q want=%q", known, card.ModelProvider, want)
			}
		})
	}
}
