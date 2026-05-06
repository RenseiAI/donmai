package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestNew_BinaryMissingReturnsProviderUnavailable(t *testing.T) {
	t.Parallel()
	// Force a binary name that is guaranteed not to exist.
	_, err := New(Options{
		CodexBin:         "this-binary-does-not-exist-anywhere-on-path-codex-12345",
		HandshakeTimeout: time.Second,
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "this-binary-does-not-exist") {
		t.Fatalf("error should mention the missing binary, got %q", err.Error())
	}
}

func TestNew_HandshakeFailureReturnsProviderUnavailable(t *testing.T) {
	t.Parallel()
	// Build pipes where the "server" never responds. New() should
	// time out on initialize and return ErrProviderUnavailable.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})
	// Drain the request side so writes do not deadlock.
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	_, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatalf("expected handshake error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
}

func TestProvider_NameAndCapabilities(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-NC")
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	if p.Name() != agent.ProviderCodex {
		t.Fatalf("expected provider name=%q, got %q", agent.ProviderCodex, p.Name())
	}
	caps := p.Capabilities()
	if caps.SupportsMessageInjection {
		t.Fatalf("expected SupportsMessageInjection=false")
	}
	if !caps.SupportsSessionResume {
		t.Fatalf("expected SupportsSessionResume=true")
	}
	if !caps.NeedsBaseInstructions {
		t.Fatalf("expected NeedsBaseInstructions=true")
	}
	if !caps.NeedsPermissionConfig {
		t.Fatalf("expected NeedsPermissionConfig=true")
	}
	if caps.ToolPermissionFormat != "codex" {
		t.Fatalf("expected ToolPermissionFormat=codex, got %q", caps.ToolPermissionFormat)
	}
	// Tool-use surface (002 v2): MCPServers wired via config/batchWrite;
	// AllowedTools NOT wired (codex routes per-tool permission via the
	// approval bridge, Spec.PermissionConfig). Declared honestly.
	if !caps.AcceptsMcpServerSpec {
		t.Errorf("AcceptsMcpServerSpec: want true (config/batchWrite mcpServers wired); got false")
	}
	if caps.AcceptsAllowedToolsList {
		t.Errorf("AcceptsAllowedToolsList: want false (codex uses approval bridge); got true")
	}
}

func TestProvider_ResumeRejectsEmptySessionID(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-RE")
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	_, err = p.Resume(context.Background(), "", agent.Spec{Cwd: "/tmp"})
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestProvider_ShutdownIsIdempotent(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-SH")
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	defer fs.close()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestProvider_AppServerCrashFailsLiveHandles(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()
	threadID := "thread-CRASH"

	// Server: respond to initialize/thread/start/turn/start, then
	// abruptly close stdout to simulate the codex app-server dying.
	var crashOnce sync.Once
	go func() {
		dec := json.NewDecoder(fs.stdin)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			method, _ := msg["method"].(string)
			idRaw, hasID := msg["id"]
			switch {
			case method == "initialize" && hasID:
				fs.replyOK(t, idRaw)
			case method == "thread/start" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0", "id": idRaw,
					"result": map[string]any{"thread": map[string]any{"id": threadID}},
				})
			case method == "turn/start" && hasID:
				fs.replyOK(t, idRaw)
				crashOnce.Do(func() {
					_ = fs.stdout.Close() // simulate crash
				})
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "hi", Cwd: "/tmp/wt", Autonomous: true})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// After the simulated crash the events channel should close
	// and emit an ErrorEvent with code app_server_crashed.
	got := drainEvents(t, h.Events(), 5*time.Second)
	var sawCrash bool
	for _, ev := range got {
		if ee, ok := ev.(agent.ErrorEvent); ok && ee.Code == "app_server_crashed" {
			sawCrash = true
		}
	}
	if !sawCrash {
		t.Fatalf("expected app_server_crashed ErrorEvent, got: %v", kindsOf(got))
	}
}
