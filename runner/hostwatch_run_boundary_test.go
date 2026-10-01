package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/state"
)

func TestRunStampsEventLogBoundaryBeforeFirstAppend(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for the runner boundary control")
	}
	h := newRunnerHarness(t)
	work := h.queuedWork("RUN-BOUNDARY-1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := h.runner.Run(ctx, work)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	st, err := h.runner.store.Read(result.WorktreePath)
	if err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if st.SessionID != work.SessionID || st.StartedAt != result.StartedAt ||
		st.EventLogStartOffset == nil || *st.EventLogStartOffset != 0 {
		t.Fatalf("first journal append preceded the run boundary: state=%+v resultStart=%d", st, result.StartedAt)
	}
	if st.LastHeartbeat <= 0 {
		t.Fatal("successful runner session refresh did not persist heartbeat")
	}
}

func TestStampRunJournalBoundaryFreshAndExactContinuation(t *testing.T) {
	worktree := t.TempDir()
	st := &state.State{SessionID: "old", StartedAt: 100, LastHeartbeat: 75, ProviderSessionID: "native"}
	if err := stampRunJournalBoundary(st, "new", 200, worktree); err != nil {
		t.Fatal(err)
	}
	if st.SessionID != "old" || st.StartedAt != 200 || st.EventLogStartOffset == nil ||
		*st.EventLogStartOffset != 0 || st.LastHeartbeat != 0 || st.ProviderSessionID != "native" {
		t.Fatalf("fresh run boundary/reset incorrect: %+v", st)
	}
	if err := os.MkdirAll(filepath.Join(worktree, state.AgentDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(worktree, state.AgentDirName, "events.jsonl")
	if err := os.WriteFile(journal, []byte("old event\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.SessionID = "new" // same exact run observation must retain its zero boundary
	st.LastHeartbeat = 123
	st.CurrentStep = "completed" // phase is not the run identity
	if err := stampRunJournalBoundary(st, "new", 200, worktree); err != nil {
		t.Fatal(err)
	}
	if st.EventLogStartOffset == nil || *st.EventLogStartOffset != 0 || st.LastHeartbeat != 123 {
		t.Fatalf("exact run continuation was restamped: %+v", st)
	}
	if err := stampRunJournalBoundary(st, "new", 201, worktree); err != nil {
		t.Fatal(err)
	}
	if st.StartedAt != 201 || st.EventLogStartOffset == nil ||
		*st.EventLogStartOffset != int64(len("old event\n")) || st.LastHeartbeat != 0 {
		t.Fatalf("same-ID replacement run inherited stale boundary/heartbeat: %+v", st)
	}
	st.LastHeartbeat = 456
	if err := stampRunJournalBoundary(st, "different", 201, worktree); err != nil {
		t.Fatal(err)
	}
	if st.LastHeartbeat != 0 {
		t.Fatalf("different session kept stale heartbeat: %+v", st)
	}
}

func TestStampRunJournalBoundaryUnsafeExistingFileIsUnknown(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		setup func(t *testing.T, journal string)
	}{
		{"directory", func(t *testing.T, journal string) {
			t.Helper()
			if err := os.Mkdir(journal, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, journal string) {
			t.Helper()
			target := filepath.Join(filepath.Dir(journal), "target")
			if err := os.WriteFile(target, []byte("old event\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, journal); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			worktree := t.TempDir()
			dir := filepath.Join(worktree, state.AgentDirName)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			fixture.setup(t, filepath.Join(dir, "events.jsonl"))
			st := &state.State{SessionID: "previous", StartedAt: 1, LastHeartbeat: 999}
			err := stampRunJournalBoundary(st, "current", 2, worktree)
			if !errors.Is(err, errRunJournalBoundaryUnavailable) ||
				strings.Contains(err.Error(), worktree) || st.EventLogStartOffset != nil ||
				st.StartedAt != 2 || st.LastHeartbeat != 0 {
				t.Fatalf("unsafe journal got a forged boundary or leaked path: state=%+v err=%v", st, err)
			}
		})
	}
}
