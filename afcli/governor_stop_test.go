package afcli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/process"
)

// writeGovernorPID records the governor PID through the same
// process.PIDFile that `governor start` writes — keeping the tests on
// the exact read/write path the commands share.
func writeGovernorPID(t *testing.T, pid int) *process.PIDFile {
	t.Helper()
	pf, err := process.NewPIDFile(governorPIDName)
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	if err := pf.Write(pid); err != nil {
		t.Fatalf("Write pid: %v", err)
	}
	return pf
}

// writeLegacyGovernorPID drops a governor PID file in the pre-rename
// (agentfactory) runtime directory, simulating a governor started by an
// older binary.
func writeLegacyGovernorPID(t *testing.T, base string, pid int) {
	t.Helper()
	dir := filepath.Join(base, "agentfactory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll legacy dir: %v", err)
	}
	path := filepath.Join(dir, governorPIDName+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile legacy pid: %v", err)
	}
}

// governorPIDGone reports whether no live governor PID record remains.
func governorPIDGone(t *testing.T) bool {
	t.Helper()
	pf, err := process.NewPIDFile(governorPIDName)
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	_, err = pf.Read()
	return errors.Is(err, process.ErrNotRunning) || errors.Is(err, process.ErrStalePID)
}

func TestGovernorStopCommand(t *testing.T) {
	t.Run("no_pid_file_returns_not_running", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		cmd := newGovernorStopCmd("donmai")
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
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		// Record a PID for a non-existent process (large PID).
		pf := writeGovernorPID(t, 99999999)

		cmd := newGovernorStopCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		execErr := cmd.Execute()
		if execErr == nil {
			t.Fatal("expected error for stale PID, got nil")
		}

		// PID file should be removed after cleanup.
		if _, err := pf.Read(); !errors.Is(err, process.ErrNotRunning) {
			t.Errorf("expected stale PID file removed after stop command, Read() = %v", err)
		}
	})

	t.Run("running_process_terminates_cleanly", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

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
		writeGovernorPID(t, childPID)

		cmd := newGovernorStopCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("stop command returned error: %v", err)
		}

		// PID record should be gone after a successful stop.
		if !governorPIDGone(t) {
			t.Error("expected PID file to be removed after successful stop")
		}

		// Verify the process is actually gone by waiting on it.
		_ = child.Wait()
	})

	// The migration window: a governor recorded by a pre-rename binary
	// (agentfactory PID dir) must still be stoppable after upgrade.
	t.Run("legacy_pid_dir_still_stoppable", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", tmp)

		child := exec.Command("sleep", "3600")
		if err := child.Start(); err != nil {
			t.Fatalf("start child process: %v", err)
		}
		t.Cleanup(func() {
			_ = child.Process.Kill()
			_ = child.Wait()
		})

		writeLegacyGovernorPID(t, tmp, child.Process.Pid)

		cmd := newGovernorStopCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("stop command returned error for legacy pid record: %v", err)
		}

		if !governorPIDGone(t) {
			t.Error("expected legacy PID record removed after successful stop")
		}
		_ = child.Wait()
	})
}
