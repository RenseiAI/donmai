package afcli

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestGovernorStatusCommand(t *testing.T) {
	t.Run("not_running_when_no_pid_file", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status command returned unexpected error: %v", err)
		}

		out := stop()
		if !strings.Contains(strings.ToLower(out), "not running") {
			t.Errorf("output should contain 'not running'; got: %q", out)
		}
	})

	t.Run("running_when_process_alive", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		// Spawn a long-lived child.
		child := exec.Command("sleep", "3600")
		if err := child.Start(); err != nil {
			t.Fatalf("start child process: %v", err)
		}
		childPID := child.Process.Pid
		t.Cleanup(func() {
			_ = child.Process.Kill()
			_ = child.Wait()
		})

		writeGovernorPID(t, childPID)

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status command returned unexpected error: %v", err)
		}

		out := stop()
		lowerOut := strings.ToLower(out)
		if !strings.Contains(lowerOut, "running") {
			t.Errorf("output should contain 'running'; got: %q", out)
		}
		pidStr := fmt.Sprintf("%d", childPID)
		if !strings.Contains(out, pidStr) {
			t.Errorf("output should contain PID %s; got: %q", pidStr, out)
		}
	})

	t.Run("stale_pid_cleans_up", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

		// Record a PID for a non-existent process.
		writeGovernorPID(t, 99999999)

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status command returned unexpected error: %v", err)
		}

		out := stop()
		lowerOut := strings.ToLower(out)
		if !strings.Contains(lowerOut, "stale") && !strings.Contains(lowerOut, "not running") {
			t.Errorf("output should mention 'stale' or 'not running'; got: %q", out)
		}

		// PID file should have been removed after cleanup.
		if !governorPIDGone(t) {
			t.Error("expected stale PID file to be removed after status command")
		}
	})

	// The migration window: a governor recorded by a pre-rename binary
	// (agentfactory PID dir) must still be reported as running.
	t.Run("legacy_pid_dir_reports_running", func(t *testing.T) {
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

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd("donmai")
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status command returned unexpected error: %v", err)
		}

		out := stop()
		if !strings.Contains(strings.ToLower(out), "is running") {
			t.Errorf("output should report the legacy-recorded governor as running; got: %q", out)
		}
	})
}
