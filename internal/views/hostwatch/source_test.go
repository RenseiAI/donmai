package hostwatch

import (
	"encoding/json"
	"errors"
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

func TestSource_Snapshot_StateEnrichment(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "sess-1")
	writeState(t, wt, state.State{
		IssueIdentifier: "ENG-42",
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
	if scoped.Counters.Running != 1 {
		t.Errorf("scoped running: want 1, got %d", scoped.Counters.Running)
	}
	if scoped.Counters.QueueDepth != 2 {
		t.Errorf("queue: want 2, got %d", scoped.Counters.QueueDepth)
	}
	if scoped.Counters.Version != "0.39.0" {
		t.Errorf("version: want 0.39.0, got %q", scoped.Counters.Version)
	}
	// Unscoped: Running trusts the daemon's own active count.
	unscoped := NewSource(fd, &noopState{}, "").Snapshot()
	if unscoped.Counters.Running != 3 {
		t.Errorf("unscoped running: want 3 (daemon total), got %d", unscoped.Counters.Running)
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
