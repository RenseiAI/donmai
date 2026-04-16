package inline

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/api"
)

// captureStdout swaps os.Stdout with a pipe, returning the reader and a
// restore function registered via t.Cleanup. Tests may drain the reader
// in a goroutine and close the writer to signal EOF.
func captureStdout(t *testing.T) (reader *os.File, writer *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = orig
		_ = r.Close()
	})
	return r, w
}

// drainPipe reads from r into a buffer and signals done when r returns EOF.
func drainPipe(r io.Reader) (<-chan *bytes.Buffer, <-chan error) {
	bufCh := make(chan *bytes.Buffer, 1)
	errCh := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		bufCh <- &buf
		errCh <- err
	}()
	return bufCh, errCh
}

func TestRunWatch(t *testing.T) {
	// signal.Notify registers global handlers; subtests must run sequentially.
	t.Run("default_mode_pipes_output", func(t *testing.T) {
		r, w := captureStdout(t)
		bufCh, _ := drainPipe(r)

		ds := api.NewMockClient()
		done := make(chan error, 1)
		go func() {
			done <- RunWatch(ds, WatchConfig{Interval: 15 * time.Millisecond, JSON: false})
		}()

		// Let a few ticks happen before sending SIGINT.
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("send SIGINT: %v", err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunWatch returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RunWatch did not return within 2s after SIGINT")
		}

		// Close writer so drainPipe sees EOF.
		_ = w.Close()

		var buf *bytes.Buffer
		select {
		case buf = <-bufCh:
		case <-time.After(2 * time.Second):
			t.Fatal("drain did not complete within 2s")
		}

		out := buf.String()
		if !strings.Contains(out, "workers |") {
			t.Errorf("output missing 'workers |' substring; got:\n%s", out)
		}
	})

	t.Run("json_mode_emits_ndjson", func(t *testing.T) {
		r, w := captureStdout(t)
		bufCh, _ := drainPipe(r)

		ds := api.NewMockClient()
		done := make(chan error, 1)
		go func() {
			done <- RunWatch(ds, WatchConfig{Interval: 15 * time.Millisecond, JSON: true})
		}()

		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatalf("send SIGINT: %v", err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunWatch returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RunWatch did not return within 2s after SIGINT")
		}

		_ = w.Close()

		var buf *bytes.Buffer
		select {
		case buf = <-bufCh:
		case <-time.After(2 * time.Second):
			t.Fatal("drain did not complete within 2s")
		}

		out := buf.String()
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Fatalf("expected at least one NDJSON line; got:\n%s", out)
		}

		var stats api.StatsResponse
		if err := json.Unmarshal([]byte(lines[0]), &stats); err != nil {
			t.Fatalf("first line is not valid JSON: %v\nline: %q", err, lines[0])
		}
		if stats.WorkersOnline <= 0 {
			t.Errorf("stats.WorkersOnline = %d, want > 0", stats.WorkersOnline)
		}
	})
}
