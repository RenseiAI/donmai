package afcli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type eofNotifyingReader struct {
	reader io.Reader
	eof    chan struct{}
	once   sync.Once
}

func (r *eofNotifyingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.once.Do(func() { close(r.eof) })
	}
	return n, err
}

type notifyingLogWriter struct {
	mu     sync.Mutex
	text   strings.Builder
	notify chan struct{}
}

func (w *notifyingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.text.Write(p)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return n, err
}

func (w *notifyingLogWriter) contains(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Contains(w.text.String(), s)
}

func waitForLogText(w *notifyingLogWriter, want string, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if w.contains(want) {
			return true
		}
		select {
		case <-w.notify:
		case <-timer.C:
			return w.contains(want)
		}
	}
}

func TestDaemonLogsFollowReadsAppendAfterEOFAndCancels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("{\"level\":\"info\",\"msg\":\"initial sentinel\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	reader := &eofNotifyingReader{reader: f, eof: make(chan struct{})}
	output := &notifyingLogWriter{notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() { done <- followDaemonLog(ctx, reader, output, true, ticks) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("follow helper did not join during cleanup")
			}
		}
	}()

	if !waitForLogText(output, "initial sentinel", 2*time.Second) {
		t.Fatal("follow did not print the initial NDJSON line")
	}
	select {
	case <-reader.eof:
	case <-time.After(2 * time.Second):
		t.Fatal("initial scan never observed EOF")
	}
	appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(appendFile, "appended plain sentinel\n{\"level\":\"info\",\"msg\":\"appended JSON sentinel\"}\n"); err != nil {
		_ = appendFile.Close()
		t.Fatal(err)
	}
	if err = appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Now()
	if !waitForLogText(output, "appended plain sentinel", 2*time.Second) ||
		!waitForLogText(output, "appended JSON sentinel", 2*time.Second) {
		t.Fatal("follow did not read plain and NDJSON lines appended after EOF")
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatalf("follow after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow did not exit after context cancellation")
	}
}

func TestDaemonLogsFollowCommandCancels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("initial command line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := &notifyingLogWriter{notify: make(chan struct{}, 1)}
	cmd := newDaemonLogsCmd()
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--file", path, "--follow"})
	cmd.SetOut(output)
	cmd.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("follow command did not join during cleanup")
			}
		}
	}()
	if !waitForLogText(output, "initial command line", 2*time.Second) {
		t.Fatal("command did not print initial line")
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatalf("follow command after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow command did not exit after cancellation")
	}
}

func TestDaemonDrainHelpDescribesResumableState(t *testing.T) {
	cmd := newDaemonDrainCmd(defaultDaemonFactory)
	var output strings.Builder
	cmd.SetOut(&output)
	if err := cmd.Help(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "resum") || strings.Contains(output.String(), "before exiting") {
		t.Fatalf("drain help must describe a resumable daemon: %s", output.String())
	}
}
