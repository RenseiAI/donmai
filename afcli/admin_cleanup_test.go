package afcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestRunWorktreeCleanupPreservesLeaseStateAndRetainedWorkarea(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	leaseDir := filepath.Join(root, ".terminal-leases")
	store, err := workarea.NewLeaseStore(workarea.StoreOptions{Dir: leaseDir})
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(root, "session-1")
	if err := os.MkdirAll(worktreePath, 0o750); err != nil {
		t.Fatal(err)
	}
	workareaID, err := workarea.IDForPath(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Acquire(context.Background(), workarea.AcquireSpec{
		SessionID:        "session-1",
		TerminalResultID: "result-1",
		WorkareaID:       workareaID,
		WorkareaPath:     worktreePath,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := runWorktreeCleanup(context.Background(), root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || result.Cleaned != 0 {
		t.Fatalf("cleanup result = %+v", result)
	}
	if _, err := os.Stat(leaseDir); err != nil {
		t.Fatalf("lease state directory removed: %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("retained workarea removed: %v", err)
	}
}
