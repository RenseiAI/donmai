package afcli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestGovernorStopCommand(t *testing.T) {
	t.Run("no_pid_file_returns_not_running", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

		cmd := newGovernorStopCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected error when no PID file exists, got nil")
		}
		if !strings.Contains(err.Error(), "not running") {
			t.Errorf("error should mention 'not running'; got: %v", err)
		}
	})

	t.Run("stale_pid_file_cleans_up", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

		// Use savePID to write a PID for a non-existent process (large PID).
		// This ensures we use the correct pidDir path (respects HOME).
		if err := savePID(governorPIDName, 99999999); err != nil {
			t.Fatalf("savePID: %v", err)
		}

		cmd := newGovernorStopCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		execErr := cmd.Execute()
		if execErr == nil {
			t.Fatal("expected error for stale PID, got nil")
		}

		// PID file should be removed: loadPID must fail after cleanup.
		if _, err := loadPID(governorPIDName); err == nil {
			t.Error("expected stale PID file to be removed after stop command")
		}
	})

	t.Run("running_process_terminates_cleanly", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

		// Spawn a long-lived child process.
		child := exec.Command("sleep", "3600")
		if err := child.Start(); err != nil {
			t.Fatalf("start child process: %v", err)
		}
		childPID := child.Process.Pid
		t.Cleanup(func() {
			// Best-effort cleanup in case test fails before stop completes.
			_ = child.Process.Kill()
			_ = child.Wait()
		})

		// Save its PID so the stop command can find it.
		if err := savePID(governorPIDName, childPID); err != nil {
			t.Fatalf("save pid: %v", err)
		}

		cmd := newGovernorStopCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("stop command returned error: %v", err)
		}

		// PID file should be removed: loadPID must fail after successful stop.
		if _, err := loadPID(governorPIDName); err == nil {
			t.Error("expected PID file to be removed after successful stop")
		}

		// Verify the process is actually gone by waiting on it.
		_ = child.Wait()
	})
}
