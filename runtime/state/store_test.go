package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runtime/state"
)

func TestReadNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()
	_, err := s.Read(dir)
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestWriteReadRoundtrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()
	in := &state.State{
		IssueID:           "issue-uuid",
		IssueIdentifier:   "REN-1",
		SessionID:         "sess-1",
		ProviderName:      agent.ProviderClaude,
		ProviderSessionID: "claude-uuid",
		WorkType:          "development",
		CurrentStep:       "spawning",
		AttemptCount:      1,
		StartedAt:         1700000000000,
		PID:               12345,
		WorkerID:          "worker-1",
	}
	if err := s.Write(dir, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out.IssueID != "issue-uuid" || out.IssueIdentifier != "REN-1" || out.AttemptCount != 1 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	if out.LastUpdatedAt == 0 {
		t.Fatalf("Write should populate LastUpdatedAt")
	}
}

func TestUpdateCreatesAndIncrements(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()

	got, err := s.Update(dir, func(st *state.State) error {
		st.IssueIdentifier = "REN-7"
		st.AttemptCount = 1
		return nil
	})
	if err != nil {
		t.Fatalf("Update first: %v", err)
	}
	if got.AttemptCount != 1 || got.IssueIdentifier != "REN-7" {
		t.Fatalf("first update: %+v", got)
	}

	got2, err := s.Update(dir, func(st *state.State) error {
		st.AttemptCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Update second: %v", err)
	}
	if got2.AttemptCount != 2 {
		t.Fatalf("expected AttemptCount=2, got %d", got2.AttemptCount)
	}
	if got2.IssueIdentifier != "REN-7" {
		t.Fatalf("Update lost prior fields: %+v", got2)
	}
}

func TestUpdateConcurrentSerializes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()

	if err := s.Write(dir, &state.State{IssueIdentifier: "REN-1", AttemptCount: 0}); err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	var failures atomic.Int64
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Update(dir, func(st *state.State) error {
				st.AttemptCount++
				return nil
			}); err != nil {
				failures.Add(1)
				t.Errorf("Update: %v", err)
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatal("Update returned errors under contention")
	}
	final, err := s.Read(dir)
	if err != nil {
		t.Fatalf("Read final: %v", err)
	}
	if final.AttemptCount != n {
		t.Fatalf("expected AttemptCount=%d after %d updates, got %d", n, n, final.AttemptCount)
	}
}

func TestReadMalformedWrapsSentinel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, state.AgentDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Path(dir), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := state.NewStore()
	_, err := s.Read(dir)
	if !errors.Is(err, state.ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
}

func TestUpdateRecoversFromMalformed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, state.AgentDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.Path(dir), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := state.NewStore()
	out, err := s.Update(dir, func(st *state.State) error {
		st.IssueIdentifier = "REN-RECOVER"
		return nil
	})
	if err != nil {
		t.Fatalf("Update on malformed: %v", err)
	}
	if out.IssueIdentifier != "REN-RECOVER" {
		t.Fatalf("expected recovery write, got %+v", out)
	}
}

func TestReadExpectIdentifierMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()
	if err := s.Write(dir, &state.State{IssueIdentifier: "REN-OTHER"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.ReadExpect(dir, "REN-MINE")
	if !errors.Is(err, state.ErrIdentifierMismatch) {
		t.Fatalf("expected ErrIdentifierMismatch, got %v", err)
	}
	if st == nil || st.IssueIdentifier != "REN-OTHER" {
		t.Fatalf("ReadExpect should still return loaded state for forensics, got %+v", st)
	}
}

func TestReadExpectMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()
	if err := s.Write(dir, &state.State{IssueIdentifier: "REN-MINE"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.ReadExpect(dir, "REN-MINE")
	if err != nil {
		t.Fatalf("ReadExpect: %v", err)
	}
	if st.IssueIdentifier != "REN-MINE" {
		t.Fatalf("got %+v", st)
	}
}

func TestPathHelpers(t *testing.T) {
	t.Parallel()

	got := state.Path("/tmp/wt")
	want := filepath.Join("/tmp/wt", ".agent", "state.json")
	if got != want {
		t.Fatalf("Path: got %q want %q", got, want)
	}
}

func TestWriteAtomicNoTempLeftover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := state.NewStore()
	if err := s.Write(dir, &state.State{IssueIdentifier: "REN-1"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, state.AgentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != state.StateFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only state.json, got %v", names)
	}
}
