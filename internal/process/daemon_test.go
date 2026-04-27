//go:build !windows

package process_test

import (
	"os"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/internal/process"
)

func TestDaemonize_ChildDetection(t *testing.T) {
	// Set AF_DAEMON=1 — Daemonize should detect it is running as the child.
	t.Setenv("AF_DAEMON", "1")

	isChild, childPID, err := process.Daemonize()
	if err != nil {
		t.Fatalf("Daemonize() with AF_DAEMON=1: %v", err)
	}
	if !isChild {
		t.Error("Daemonize() isChild = false, want true when AF_DAEMON=1")
	}
	if childPID != 0 {
		t.Errorf("Daemonize() childPID = %d, want 0 when AF_DAEMON=1", childPID)
	}
}

// TestDaemonize_ParentPath exercises the parent branch by ensuring Daemonize
// returns (false, pid>0, nil) when AF_DAEMON is not set. The spawned child is
// immediately released — we don't wait for it.
func TestDaemonize_ParentPath(t *testing.T) {
	if os.Getenv("AF_DAEMON") == "1" {
		// Prevent accidental infinite spawn if the test binary itself is run as daemon.
		t.Skip("already in daemon child; skip parent path test")
	}

	// Ensure AF_DAEMON is unset.
	t.Setenv("AF_DAEMON", "")

	isChild, childPID, err := process.Daemonize()
	if err != nil {
		t.Fatalf("Daemonize(): %v", err)
	}
	if isChild {
		// We should not be in the child path because AF_DAEMON was empty.
		t.Fatal("Daemonize() isChild = true, want false when AF_DAEMON is unset")
	}
	if childPID <= 0 {
		t.Errorf("Daemonize() childPID = %d, want > 0", childPID)
	}
}
