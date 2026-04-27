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
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd()
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
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

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

		if err := savePID(governorPIDName, childPID); err != nil {
			t.Fatalf("save pid: %v", err)
		}

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd()
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
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)

		// Write a PID file for a non-existent process using savePID
		// so it lands in the correct pidDir (respects HOME).
		if err := savePID(governorPIDName, 99999999); err != nil {
			t.Fatalf("savePID: %v", err)
		}

		stop := captureOSStdout(t)

		cmd := newGovernorStatusCmd()
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

		// PID file should have been removed: loadPID must fail after cleanup.
		if _, err := loadPID(governorPIDName); err == nil {
			t.Error("expected stale PID file to be removed after status command")
		}
	})
}
