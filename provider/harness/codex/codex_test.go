package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
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

func TestProvider_RequiredMCPMethodNotFoundFailsBeforeThreadStart(t *testing.T) {
	const secretSentinel = "secret-config-sentinel-must-not-surface"
	fs, stdinW, stdoutR := newFakeServer()
	var threadStarted atomic.Bool
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
			case method == "config/batchWrite" && hasID:
				fs.write(t, map[string]any{
					"jsonrpc": "2.0",
					"id":      idRaw,
					"error":   map[string]any{"code": -32601, "message": "Method not found; rejected " + secretSentinel},
				})
			case method == "thread/start" && hasID:
				threadStarted.Store(true)
				fs.replyOK(t, idRaw)
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	var receipts []agent.ToolLifecycleReceipt
	_, err = p.Spawn(t.Context(), agent.Spec{
		MCPServers: []agent.MCPServerConfig{{Name: "required", Command: "server"}},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn error = %v, want ErrSpawnFailed", err)
	}
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed || adaptationErr.Channel != agent.ToolChannelMCPServer {
		t.Fatalf("Spawn error = %v, want typed MCP application denial", err)
	}
	if !strings.Contains(err.Error(), "-32601") {
		t.Fatalf("Spawn error omitted exact RPC failure: %v", err)
	}
	if strings.Contains(err.Error(), secretSentinel) {
		t.Fatalf("Spawn error leaked app-server response content: %v", err)
	}
	if threadStarted.Load() {
		t.Fatal("thread/start was sent after required MCP configuration failed")
	}
	if len(receipts) != 2 || receipts[0].Decision != "ready" || receipts[1].Decision != "denied" {
		t.Fatalf("receipts = %+v, want ready admission followed by denied application", receipts)
	}
	receiptJSON, marshalErr := json.Marshal(receipts)
	if marshalErr != nil {
		t.Fatalf("marshal receipts: %v", marshalErr)
	}
	if strings.Contains(string(receiptJSON), secretSentinel) {
		t.Fatalf("receipt leaked app-server response content: %s", receiptJSON)
	}
}

func TestProvider_SequentialDifferentMCPConfigsAreBothApplied(t *testing.T) {
	p, fs := newTestProvider(t)

	spawnAndDrain := func(name string) {
		t.Helper()
		h, err := p.Spawn(t.Context(), agent.Spec{
			Prompt:     "use " + name,
			Cwd:        t.TempDir(),
			MCPServers: []agent.MCPServerConfig{{Name: name, Command: "server-" + name}},
		})
		if err != nil {
			t.Fatalf("Spawn(%s): %v", name, err)
		}
		_ = drainEvents(t, h.Events(), 5*time.Second)
	}

	spawnAndDrain("alpha")
	spawnAndDrain("beta")

	fs.mu.Lock()
	writes := append([]string(nil), fs.mcpWrites...)
	fs.mu.Unlock()
	if len(writes) != 2 {
		t.Fatalf("config/batchWrite count = %d, want 2; writes=%v", len(writes), writes)
	}
	if !strings.Contains(writes[0], "alpha") || strings.Contains(writes[0], "beta") {
		t.Fatalf("first MCP write = %s, want alpha only", writes[0])
	}
	if !strings.Contains(writes[1], "beta") || strings.Contains(writes[1], "alpha") {
		t.Fatalf("second MCP write = %s, want beta only", writes[1])
	}
}

func TestProvider_IncompatibleMCPConfigDeniedWhileLeaseIsLive(t *testing.T) {
	p, _ := newTestProvider(t)
	alpha := map[string]any{"alpha": map[string]any{"command": "server-alpha"}}
	beta := map[string]any{"beta": map[string]any{"command": "server-beta"}}

	release, err := p.acquireMCPConfig(t.Context(), alpha)
	if err != nil {
		t.Fatalf("acquire alpha: %v", err)
	}
	defer release()

	_, err = p.acquireMCPConfig(t.Context(), beta)
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed || adaptationErr.Channel != agent.ToolChannelMCPServer {
		t.Fatalf("incompatible live config error = %v, want typed MCP application denial", err)
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
