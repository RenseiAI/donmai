package codex

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// codexFakeAppServerCrashStderr is the exact bytes a "crashed during MCP
// server startup" app-server would leave behind: a diagnostic line plus a
// bearer token this test proves gets scrubbed before it ever reaches a log
// line or a returned error.
const codexFakeAppServerCrashStderr = "fatal: failed to start MCP server \"fixture\": exit status 1\n" +
	"Authorization: Bearer sk-do-not-leak-this-headless-secret\n"

// codexHeadlessCrashProvider builds a Provider whose app-server is this test
// binary in fake mode, configured to write codexFakeAppServerCrashStderr to
// its own stderr and then os.Exit(1) immediately — before ever answering the
// initialize handshake. That is the exact ordering the bug this change fixes
// is about: the app-server dies during its own startup, before completing
// the handshake headless callers depend on.
func codexHeadlessCrashProvider(t *testing.T) *Provider {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	p, err := New(Options{
		CodexBin: self,
		Cwd:      t.TempDir(),
		Env: map[string]string{
			codexFakeAppServerEnv:      "1",
			codexFakeAppServerCrashEnv: "1",
			codexFakeAppServerStderrEnv: base64.StdEncoding.EncodeToString(
				[]byte(codexFakeAppServerCrashStderr),
			),
		},
		HandshakeTimeout: 5 * time.Second,
		RPCTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

// TestSpawnSurfacesAppServerStderrExcerptOnCrash pins item 2(b) of the
// bounded-stderr-capture change for the HEADLESS lane: when the shared
// app-server dies before completing its handshake, Spawn's returned error
// carries a bounded, redacted excerpt of what it printed — not nothing, and
// not the raw secret it printed alongside the real diagnostic.
//
// RED proof: revert failStartLocked's excerpt-attachment (or point
// p.appServerStderr at a fresh, never-written buffer) and this test fails —
// the returned error stops naming "app-server stderr:" or the diagnostic
// text entirely. Verified by reverting captureAppServerStderr's wiring in
// startLocked back to the retired drainStderr(stderr) discard: this test
// failed (error carried no diagnostic text at all), then passed again after
// restoring it — see the completion report for the exact revert/run/restore.
func TestSpawnSurfacesAppServerStderrExcerptOnCrash(t *testing.T) {
	p := codexHeadlessCrashProvider(t)

	_, err := p.Spawn(t.Context(), agent.Spec{Prompt: "crash probe", Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("Spawn succeeded against an app-server that crashed before the handshake completed")
	}
	msg := err.Error()
	if !strings.Contains(msg, "app-server stderr:") {
		t.Fatalf("error does not carry an app-server stderr excerpt: %v", err)
	}
	if !strings.Contains(msg, `failed to start MCP server "fixture"`) {
		t.Fatalf("error dropped the diagnostic line the excerpt exists to preserve: %v", err)
	}
	if strings.Contains(msg, "sk-do-not-leak-this-headless-secret") {
		t.Fatalf("error leaked the raw bearer token: %v", err)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("error shows no redaction marker at all: %v", err)
	}
}

// slogRecorder is a minimal slog.Handler that records every record it
// receives, guarded by a mutex since Provider.watchExit's log call runs on a
// background goroutine.
type slogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *slogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *slogRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}
func (r *slogRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *slogRecorder) WithGroup(string) slog.Handler      { return r }

func (r *slogRecorder) snapshot() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slog.Record(nil), r.records...)
}

// findRecord returns the first recorded line whose message contains want, or
// nil.
func findRecord(records []slog.Record, want string) *slog.Record {
	for i := range records {
		if strings.Contains(records[i].Message, want) {
			return &records[i]
		}
	}
	return nil
}

// TestWatchExitLogsExactlyOnceOnAppServerExit pins item 3 for the headless
// lane: the shared app-server's exit — successful handshake, then killed —
// produces exactly one structured log line carrying the redacted excerpt,
// regardless of the JSON-RPC client-close race (readLoop's own EOF-triggered
// Stop can win that race; the log line does not depend on it).
//
// RED proof: comment out watchExit's call to p.logAppServerExit(err) and
// this test's "exactly one" assertion fails (findRecord returns nil) — see
// the completion report for the revert/run/restore actually performed.
func TestWatchExitLogsExactlyOnceOnAppServerExit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	dump := t.TempDir() + "/dump.json"
	p, err := New(Options{
		CodexBin: self,
		Cwd:      t.TempDir(),
		Env: map[string]string{
			codexFakeAppServerEnv:     "1",
			codexFakeAppServerDumpEnv: dump,
		},
		HandshakeTimeout: 5 * time.Second,
		RPCTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "watch exit probe", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	recorder := &slogRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill fake app-server: %v", err)
	}
	// watchExit runs on its own goroutine; wait for its log line rather than
	// racing it. processDone is fed from the exact same p.cmd.Wait() call
	// watchExit's own logAppServerExit already ran before, so its arrival
	// proves the log line already landed.
	select {
	case <-p.processDone:
	case <-time.After(5 * time.Second):
		t.Fatal("app-server exit was not observed within 5s")
	}
	// logAppServerExit runs synchronously before watchExit reads p.processDone
	// is fed... no: processDone is fed BEFORE logAppServerExit runs, so give
	// the log call a brief moment to land.
	deadline := time.Now().Add(2 * time.Second)
	var records []slog.Record
	for time.Now().Before(deadline) {
		records = recorder.snapshot()
		if findRecord(records, "codex: app-server process exited") != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec := findRecord(records, "codex: app-server process exited")
	if rec == nil {
		t.Fatalf("no \"codex: app-server process exited\" log line observed; got %d records", len(records))
	}
	var matches int
	for _, r := range records {
		if r.Message == "codex: app-server process exited" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("app-server exit logged %d times, want exactly 1", matches)
	}
	if rec.Level != slog.LevelWarn {
		t.Fatalf("exit log level = %v, want Warn (this was an unrequested kill, not a Shutdown)", rec.Level)
	}
}
