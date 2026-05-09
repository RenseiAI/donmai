package daemon

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestSlogLineWriter_StdoutEmitsInfo verifies that the stdout writer
// emits an INFO record per line, tagging the message with sessionID
// and stream attributes.
func TestSlogLineWriter_StdoutEmitsInfo(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	w := newStdoutSlogWriter()
	w.WriteWorkerLine("sess-abc", "hello-stdout")

	rec := decodeSingle(t, buf)
	if rec.Level != "INFO" {
		t.Errorf("level = %q, want INFO", rec.Level)
	}
	if !strings.Contains(rec.Msg, "[child stdout sessionID=sess-abc] hello-stdout") {
		t.Errorf("msg = %q, want contains '[child stdout sessionID=sess-abc] hello-stdout'", rec.Msg)
	}
	if rec.SessionID != "sess-abc" {
		t.Errorf("sessionID attr = %q, want sess-abc", rec.SessionID)
	}
	if rec.Stream != "stdout" {
		t.Errorf("stream attr = %q, want stdout", rec.Stream)
	}
}

// TestSlogLineWriter_StderrEmitsWarn covers the stderr → WARN path.
func TestSlogLineWriter_StderrEmitsWarn(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	w := newStderrSlogWriter()
	w.WriteWorkerLine("sess-xyz", "hello-stderr")

	rec := decodeSingle(t, buf)
	if rec.Level != "WARN" {
		t.Errorf("level = %q, want WARN", rec.Level)
	}
	if !strings.Contains(rec.Msg, "[child stderr sessionID=sess-xyz] hello-stderr") {
		t.Errorf("msg = %q, want contains '[child stderr sessionID=sess-xyz] hello-stderr'", rec.Msg)
	}
	if rec.Stream != "stderr" {
		t.Errorf("stream attr = %q, want stderr", rec.Stream)
	}
}

// TestSpawner_DefaultsChildOutputToSlog is the integration-shaped test
// for the v0.5.1 fix. We spawn a tiny child that prints one line to
// stdout and one to stderr, then assert two records appear in a
// captured slog handler — one INFO, one WARN, both tagged with the
// session id.
//
// This exercises the full path: daemon.New → spawner option default →
// pumpLines → slogLineWriter.WriteWorkerLine → slog.Default().
func TestSpawner_DefaultsChildOutputToSlog(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()

	// Hand-build a spawner the way daemon.Start does — this is the
	// path the regression matters for. We do not call daemon.New
	// here because we only want to drive the spawner option-default
	// path, not real registration.
	stdoutW := newStdoutSlogWriter()
	stderrW := newStderrSlogWriter()
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2"},
		StdoutPrefixWriter:    stdoutW,
		StderrPrefixWriter:    stderrW,
	})

	if _, err := s.AcceptWork(SessionSpec{SessionID: "sess-1", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept work: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.ActiveCount() != 0 {
		t.Fatalf("session did not exit in time")
	}

	// cmd.Wait() returning + ActiveCount==0 does NOT mean the
	// pumpLines goroutines have flushed yet. They scan child
	// stdout/stderr asynchronously via bufio.Scanner; when the child
	// closes its pipe end, Scan returns false on the next read and
	// the goroutine exits — but only AFTER any buffered bytes have
	// been delivered. Under CI's -race overhead this can lag the
	// process exit by tens of milliseconds. Previously this
	// synchronised on a fixed 50ms sleep that proved unreliable on
	// Linux runners; poll the buffer for both expected records (or
	// an upper-bound timeout) so the test is timing-independent.
	pumpDeadline := time.Now().Add(3 * time.Second)
	var sawStdout, sawStderr bool
	var records []slogRecord
	for time.Now().Before(pumpDeadline) {
		records = decodeAll(t, bytes.NewBuffer(buf.Bytes()))
		sawStdout, sawStderr = false, false
		for _, r := range records {
			if r.Stream == "stdout" && r.Level == "INFO" && strings.Contains(r.Msg, "hello-stdout") && r.SessionID == "sess-1" {
				sawStdout = true
			}
			if r.Stream == "stderr" && r.Level == "WARN" && strings.Contains(r.Msg, "hello-stderr") && r.SessionID == "sess-1" {
				sawStderr = true
			}
		}
		if sawStdout && sawStderr {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawStdout {
		t.Errorf("missing INFO stdout record; records=%v", records)
	}
	if !sawStderr {
		t.Errorf("missing WARN stderr record; records=%v", records)
	}
}

// captureSlog swaps slog.Default() for a JSON handler over an
// in-memory buffer and returns the buffer and a restore func. Use
// defer restore() in tests.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf, func() { slog.SetDefault(prev) }
}

// slogRecord is the shape we care about for decoding.
type slogRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	SessionID string `json:"sessionID"`
	Stream    string `json:"stream"`
}

func decodeSingle(t *testing.T, buf *bytes.Buffer) slogRecord {
	t.Helper()
	recs := decodeAll(t, buf)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(recs), recs)
	}
	return recs[0]
}

func decodeAll(t *testing.T, buf *bytes.Buffer) []slogRecord {
	t.Helper()
	var out []slogRecord
	dec := json.NewDecoder(buf)
	for dec.More() {
		var r slogRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode slog record: %v", err)
		}
		out = append(out, r)
	}
	return out
}
