package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/internal/inline"
)

// executeStatus builds a fresh root command, invokes Cobra with
// "status <args...>", and returns the resulting error. Cobra's own
// SetOut/SetErr buffers are assigned to discard help/error output
// unless the caller needs them.
func executeStatus(t *testing.T, args []string) error {
	t.Helper()
	cmd, _ := newRootCmd()
	cmd.SetArgs(append([]string{"status"}, args...))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// captureOSStdout swaps os.Stdout (and inline.DataWriter, which is
// captured at package init from os.Stdout) with a pipe. Returns a
// function the caller must invoke to end capture; it closes the
// writer, drains the reader, restores stdout, and returns the
// captured bytes.
func captureOSStdout(t *testing.T) (stop func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	origDataWriter := inline.DataWriter
	os.Stdout = w
	inline.DataWriter = w

	bufCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		bufCh <- buf.String()
	}()

	var restored bool
	stop = func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout = origStdout
		inline.DataWriter = origDataWriter
		_ = w.Close()
		out := <-bufCh
		_ = r.Close()
		return out
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func TestStatusCommand(t *testing.T) {
	t.Run("defaults_from_help", func(t *testing.T) {
		cmd, _ := newRootCmd()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs([]string{"status", "--help"})

		if err := cmd.Execute(); err != nil {
			t.Fatalf("status --help should exit 0, got: %v", err)
		}

		out := buf.String()
		for _, want := range []string{"--json", "--watch", "--interval", "3s"} {
			if !strings.Contains(out, want) {
				t.Errorf("help output missing %q; got:\n%s", want, out)
			}
		}
	})

	t.Run("mock_default_mode", func(t *testing.T) {
		stop := captureOSStdout(t)

		err := executeStatus(t, []string{"--mock"})
		out := stop()

		if err != nil {
			t.Fatalf("status --mock: %v", err)
		}
		if !strings.Contains(out, "workers |") {
			t.Errorf("stdout missing 'workers |' substring; got:\n%s", out)
		}
	})

	t.Run("mock_json_mode", func(t *testing.T) {
		stop := captureOSStdout(t)

		err := executeStatus(t, []string{"--mock", "--json"})
		out := stop()

		if err != nil {
			t.Fatalf("status --mock --json: %v", err)
		}

		var stats afclient.StatsResponse
		if err := json.Unmarshal([]byte(out), &stats); err != nil {
			t.Fatalf("stdout is not valid JSON: %v\nstdout: %q", err, out)
		}
		if stats.WorkersOnline <= 0 {
			t.Errorf("stats.WorkersOnline = %d, want > 0", stats.WorkersOnline)
		}
		if stats.SessionCountToday <= 0 {
			t.Errorf("stats.SessionCountToday = %d, want > 0", stats.SessionCountToday)
		}
	})

	t.Run("invalid_interval_returns_error", func(t *testing.T) {
		// Capture stdout so the initial watch iteration (if it somehow
		// runs) doesn't pollute the test log.
		_ = captureOSStdout(t)

		err := executeStatus(t, []string{"--mock", "--watch", "--interval", "notaduration"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), `invalid interval "notaduration"`) {
			t.Errorf("error = %q, want substring %q", err.Error(), `invalid interval "notaduration"`)
		}
	})
}
