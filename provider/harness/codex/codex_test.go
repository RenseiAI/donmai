package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// startTestProvider starts the app-server New now defers, for tests that drive
// provider internals (client RPCs, MCP leases) instead of going through
// Spawn/Resume. The deferral itself is covered by the Spawn-boundary tests.
func startTestProvider(t *testing.T, p *Provider) {
	t.Helper()
	if err := p.ensureStarted(); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
}

func TestSpawn_HandshakeFailureReturnsProviderUnavailable(t *testing.T) {
	// Build pipes where the "server" never responds. New() no longer starts
	// the app-server, so the first Spawn is where the initialize handshake
	// times out — and it must still surface ErrProviderUnavailable (wrapped by
	// ErrSpawnFailed) and leave no config boundary behind.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
	})
	// Drain the request side so writes do not deadlock.
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	tempRoot := t.TempDir()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		HandshakeTimeout: 200 * time.Millisecond,
		configTempDir:    tempRoot,
	})
	if err != nil {
		t.Fatalf("New must defer the handshake, got %v", err)
	}
	_, err = p.Spawn(t.Context(), agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err == nil {
		t.Fatalf("expected handshake error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("expected ErrSpawnFailed, got %v", err)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("read config temp root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("handshake failure leaked owned config boundary: %v", entries)
	}
}

func TestHostSessionAuthDefersCredentialAndAppServerUntilPreparedSpawn(t *testing.T) {
	root := t.TempDir()
	hostHome := filepath.Join(root, "host")
	boundaryParent := filepath.Join(root, "boundaries")
	for _, dir := range []string{hostHome, boundaryParent} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Base(dir), err)
		}
	}
	hostAuth := filepath.Join(hostHome, codexAuthFileName)
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	t.Setenv("CODEX_HOME", hostHome)

	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		HostSessionAuth:  true,
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    boundaryParent,
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New deferred host-session provider: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	if p.client != nil || p.started {
		t.Fatal("host-session provider started app-server during constructor probing")
	}
	if _, err := os.Lstat(filepath.Join(p.config.home, codexAuthFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host credential was projected during constructor probing: %v", err)
	}

	go fs.run(t, "thread-host-session-deferred")
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "deferred auth", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("prepared host-session Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	if p.client == nil || !p.started {
		t.Fatal("prepared headless Spawn did not initialize app-server")
	}
	hostInfo, err := os.Stat(hostAuth)
	if err != nil {
		t.Fatalf("stat host auth: %v", err)
	}
	linkedInfo, err := os.Stat(p.config.authPath)
	if err != nil {
		t.Fatalf("stat isolated auth: %v", err)
	}
	if !os.SameFile(hostInfo, linkedInfo) {
		t.Fatal("prepared Spawn did not project the pinned host credential inode")
	}
}

func TestHostSessionAuthShutdownBeforeSpawnDoesNotProjectCredential(t *testing.T) {
	hostHome := t.TempDir()
	hostAuth := filepath.Join(hostHome, codexAuthFileName)
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	t.Setenv("CODEX_HOME", hostHome)

	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		HostSessionAuth: true,
		skipProcess:     true,
		stdinOverride:   stdinW,
		stdoutOverride:  stdoutR,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New deferred host-session provider: %v", err)
	}
	if err := p.Shutdown(t.Context()); err != nil {
		fs.close()
		t.Fatalf("Shutdown before Spawn: %v", err)
	}
	defer fs.close()

	_, err = p.Spawn(t.Context(), agent.Spec{Prompt: "must not start", Cwd: t.TempDir()})
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("Spawn after Shutdown error = %v, want spawn and unavailable sentinels", err)
	}
	if p.client != nil || p.config.authPath != "" {
		t.Fatal("Spawn after Shutdown initialized app-server or projected a credential")
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
	// Tool-use surface (002 v2): MCPServers wired via isolated config/batchWrite;
	// AllowedTools NOT wired (codex routes per-tool permission via the
	// approval bridge, Spec.PermissionConfig). Declared honestly.
	if !caps.AcceptsMcpServerSpec {
		t.Errorf("AcceptsMcpServerSpec: want true (isolated mcp_servers delivery wired); got false")
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

func TestProvider_UndeclaredMCPReadbackFailsBeforeThreadAndDestroysBoundary(t *testing.T) {
	const secretSentinel = "undeclared-readback-secret-must-not-surface"
	root := t.TempDir()
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
				params, _ := msg["params"].(map[string]any)
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"filePath": params["filePath"], "status": "ok", "version": "fake",
				}})
			case method == "config/read" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"config": map[string]any{codexMCPConfigKeyPath: map[string]any{
						"undeclared": map[string]any{"command": secretSentinel},
					}},
				}})
			case method == "thread/start" && hasID:
				threadStarted.Store(true)
				fs.replyOK(t, idRaw)
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true, configTempDir: root,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	ownedHome := p.config.home

	_, err = p.Spawn(t.Context(), agent.Spec{Prompt: "must not start", Cwd: t.TempDir()})
	var adaptationErr *agent.ToolAdaptationError
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &adaptationErr) {
		t.Fatalf("Spawn error = %v, want typed pre-thread MCP denial", err)
	}
	if strings.Contains(err.Error(), secretSentinel) {
		t.Fatalf("Spawn error leaked readback contents: %v", err)
	}
	if threadStarted.Load() {
		t.Fatal("thread/start was sent after undeclared MCP readback")
	}
	if _, statErr := os.Stat(ownedHome); !os.IsNotExist(statErr) {
		t.Fatalf("owned home survives failed activation proof: stat err=%v", statErr)
	}
	_, err = p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
	adaptationErr = nil
	if !errors.As(err, &adaptationErr) {
		t.Fatalf("poisoned provider acquire error = %v, want typed denial", err)
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
	if len(writes) != 4 {
		t.Fatalf("config/batchWrite count = %d, want apply+clear per session (4); writes=%v", len(writes), writes)
	}
	if !strings.Contains(writes[0], "alpha") || strings.Contains(writes[0], "beta") {
		t.Fatalf("first MCP write = %s, want alpha only", writes[0])
	}
	if strings.Contains(writes[1], "alpha") || strings.Contains(writes[1], "beta") {
		t.Fatalf("second MCP write = %s, want empty cleanup", writes[1])
	}
	if !strings.Contains(writes[2], "beta") || strings.Contains(writes[2], "alpha") {
		t.Fatalf("third MCP write = %s, want beta only", writes[2])
	}
	if strings.Contains(writes[3], "alpha") || strings.Contains(writes[3], "beta") {
		t.Fatalf("fourth MCP write = %s, want empty cleanup", writes[3])
	}
	for _, write := range writes {
		if !strings.Contains(write, `"keyPath":"mcp_servers"`) || !strings.Contains(write, `"filePath":"`+p.config.configPath+`"`) {
			t.Fatalf("write escaped owned snake-case config boundary: %s", write)
		}
	}
}

func TestProvider_EmptyMCPBaselineIsWrittenAndVerifiedBeforeThreadStart(t *testing.T) {
	p, fs := newTestProvider(t)
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "empty", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)

	fs.mu.Lock()
	methods := append([]string(nil), fs.methods...)
	writes := append([]string(nil), fs.mcpWrites...)
	active := fs.activeMCP
	fs.mu.Unlock()
	index := func(method string) int {
		for i, got := range methods {
			if got == method {
				return i
			}
		}
		return -1
	}
	writeAt := index("config/batchWrite")
	readAt := index("config/read")
	statusAt := index("mcpServerStatus/list")
	threadAt := index("thread/start")
	if writeAt < 0 || readAt <= writeAt || statusAt <= readAt || threadAt <= statusAt {
		t.Fatalf("method order = %v, want batchWrite -> config/read -> mcpServerStatus/list -> thread/start", methods)
	}
	if len(writes) != 2 {
		t.Fatalf("empty session writes = %d, want explicit apply + terminal clear", len(writes))
	}
	if len(active) != 0 {
		t.Fatalf("active MCP after terminal cleanup = %v, want empty", active)
	}
}

func TestProvider_WaitsForMCPInventoryBeforeThreadStart(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	var active map[string]any
	var statusCalls atomic.Int32
	var statusCallsAtThreadStart atomic.Int32
	var cleanupStatusCalls atomic.Int32
	var clearing atomic.Bool

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
				params, _ := msg["params"].(map[string]any)
				edits, _ := params["edits"].([]any)
				edit, _ := edits[0].(map[string]any)
				active, _ = edit["value"].(map[string]any)
				clearing.Store(len(active) == 0)
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"filePath": params["filePath"], "status": "ok", "version": "fake",
				}})
			case method == "config/read" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"config": map[string]any{codexMCPConfigKeyPath: active}, "origins": map[string]any{},
				}})
			case method == "mcpServerStatus/list" && hasID:
				if clearing.Load() {
					call := cleanupStatusCalls.Add(1)
					data := fakeMCPStatusData(map[string]any{"ambient": map[string]any{}})
					if call == 1 {
						// A removed provider-managed server may briefly remain in
						// Codex's status cache after the config reload.
						data = append(data, fakeMCPStatusData(map[string]any{"required": map[string]any{}})...)
					}
					fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"data": data}})
					continue
				}
				call := statusCalls.Add(1)
				data := fakeMCPStatusData(active)
				for _, server := range data {
					if server["name"] == "required" && call == 1 {
						// Codex can list a configured server before its initialize
						// handshake has supplied serverInfo. That is not ready yet.
						server["serverInfo"] = nil
					}
				}
				// The status inventory can also include Codex-owned servers that
				// are not part of the isolated mcp_servers configuration.
				data = append(data, fakeMCPStatusData(map[string]any{"ambient": map[string]any{}})...)
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"data": data}})
			case method == "thread/start" && hasID:
				statusCallsAtThreadStart.Store(statusCalls.Load())
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"thread": map[string]any{"id": "thread-mcp-ready"},
				}})
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true, RPCTimeout: 125 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "wait for MCP", Cwd: t.TempDir(),
		MCPServers: []agent.MCPServerConfig{{Name: "required", Command: "server"}},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := statusCallsAtThreadStart.Load(); got != 2 {
		t.Fatalf("mcpServerStatus/list calls before thread/start = %d, want 2 (starting then ready)", got)
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := cleanupStatusCalls.Load(); got != 2 {
		t.Fatalf("cleanup status calls = %d, want 2 (retired server present then absent)", got)
	}
}

func TestProvider_MCPStartupTimeoutFailsBeforeThreadStart(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	var active map[string]any
	var statusCalls atomic.Int32
	var threadStarted atomic.Bool
	const secret = "mcp-activation-secret-must-not-leak"
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
				params, _ := msg["params"].(map[string]any)
				edits, _ := params["edits"].([]any)
				edit, _ := edits[0].(map[string]any)
				active, _ = edit["value"].(map[string]any)
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"filePath": params["filePath"], "status": "ok", "version": "fake",
				}})
			case method == "config/read" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"config": map[string]any{codexMCPConfigKeyPath: active}, "origins": map[string]any{},
				}})
			case method == "mcpServerStatus/list" && hasID:
				statusCalls.Add(1)
				data := fakeMCPStatusData(map[string]any{
					"ready":      map[string]any{},
					"waiting":    map[string]any{},
					"unexpected": map[string]any{},
				})
				for _, server := range data {
					if server["name"] == "waiting" {
						server["serverInfo"] = nil
					}
				}
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"data": data}})
			case method == "thread/start" && hasID:
				threadStarted.Store(true)
				fs.replyOK(t, idRaw)
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true, RPCTimeout: 125 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	_, err = p.Spawn(t.Context(), agent.Spec{
		Prompt: "must not start", Cwd: t.TempDir(),
		MCPServers: []agent.MCPServerConfig{
			{Name: "ready", Command: "server"},
			{Name: "waiting", Command: "server", Env: map[string]string{"TOKEN": secret}},
			{Name: "absent", Command: "server"},
		},
	})
	var adaptationErr *agent.ToolAdaptationError
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &adaptationErr) {
		t.Fatalf("Spawn error = %v, want typed pre-thread MCP denial", err)
	}
	wantDetail := `isolated config activation deadline exceeded; ready=["ready"]; uninitialized=["waiting"]; absent=["absent"]; unexpected=1`
	if adaptationErr.Detail != wantDetail {
		t.Fatalf("adaptation detail = %q, want %q", adaptationErr.Detail, wantDetail)
	}
	if strings.Contains(adaptationErr.Detail, secret) {
		t.Fatalf("adaptation detail leaked MCP configuration secret: %q", adaptationErr.Detail)
	}
	if threadStarted.Load() {
		t.Fatal("thread/start was sent before the required MCP server initialized")
	}
	if statusCalls.Load() < 2 {
		t.Fatalf("mcpServerStatus/list calls = %d, want polling before timeout", statusCalls.Load())
	}
}

func TestListActiveMCPServers_PaginatesReadyInventory(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
	var calls atomic.Int32
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
			case method == "mcpServerStatus/list" && hasID:
				call := calls.Add(1)
				params, _ := msg["params"].(map[string]any)
				if params["detail"] != "full" {
					t.Errorf("status inventory detail = %v, want full initialize metadata", params["detail"])
				}
				if call == 1 {
					if _, exists := params["cursor"]; exists {
						t.Errorf("first status page unexpectedly carried a cursor: %v", params)
					}
					fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
						"data": fakeMCPStatusData(map[string]any{"alpha": map[string]any{}}), "nextCursor": "page-2",
					}})
					continue
				}
				if params["cursor"] != "page-2" {
					t.Errorf("second status page cursor = %v, want page-2", params["cursor"])
				}
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"data": fakeMCPStatusData(map[string]any{"beta": map[string]any{}}),
				}})
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
	startTestProvider(t, p)
	active, err := p.listActiveMCPServers(t.Context())
	if err != nil {
		t.Fatalf("listActiveMCPServers: %v", err)
	}
	if !sameMCPServerNames(active, map[string]struct{}{"alpha": {}, "beta": {}}) || calls.Load() != 2 {
		t.Fatalf("active=%v calls=%d, want alpha+beta across two pages", active, calls.Load())
	}
}

func TestProvider_SameMCPConfigSharesLeaseAndClearsAfterLastRelease(t *testing.T) {
	p, fs := newTestProvider(t)
	desired := map[string]any{"same": map[string]any{"command": "/usr/bin/false"}}
	cwd := t.TempDir()
	releaseOne, err := p.acquireMCPConfig(t.Context(), desired, cwd)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	releaseTwo, err := p.acquireMCPConfig(t.Context(), desired, cwd)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	fs.mu.Lock()
	writesBeforeRelease := len(fs.mcpWrites)
	fs.mu.Unlock()
	if writesBeforeRelease != 1 {
		t.Fatalf("same-config concurrent acquires wrote %d times, want 1", writesBeforeRelease)
	}
	if err := releaseOne(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	fs.mu.Lock()
	writesAfterOne := len(fs.mcpWrites)
	fs.mu.Unlock()
	if writesAfterOne != 1 {
		t.Fatalf("first of two releases cleared live config; writes=%d", writesAfterOne)
	}
	if err := releaseTwo(); err != nil {
		t.Fatalf("last release: %v", err)
	}
	fs.mu.Lock()
	writesAfterTwo := len(fs.mcpWrites)
	active := fs.activeMCP
	fs.mu.Unlock()
	if writesAfterTwo != 2 || len(active) != 0 {
		t.Fatalf("last release writes=%d active=%v, want one clear and empty", writesAfterTwo, active)
	}
}

func TestHandle_StopReportsMCPReadbackCleanupFailureAndPoisonsProvider(t *testing.T) {
	const secretSentinel = "cleanup-readback-secret-must-not-surface"
	fs, stdinW, stdoutR := newFakeServer()
	var active map[string]any
	var clearing bool
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
				params, _ := msg["params"].(map[string]any)
				edits, _ := params["edits"].([]any)
				edit, _ := edits[0].(map[string]any)
				value, _ := edit["value"].(map[string]any)
				clearing = len(value) == 0
				active = value
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"filePath": params["filePath"], "status": "ok", "version": "fake",
				}})
			case method == "config/read" && hasID:
				readback := active
				if clearing {
					readback = map[string]any{"unexpected": map[string]any{"command": secretSentinel}}
				}
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"config": map[string]any{codexMCPConfigKeyPath: readback}, "origins": map[string]any{},
				}})
			case method == "mcpServerStatus/list" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"data": fakeMCPStatusData(active),
				}})
			case method == "thread/start" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"thread": map[string]any{"id": "thread-stop-cleanup"},
				}})
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()

	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	ownedHome := p.config.home
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "hold", Cwd: t.TempDir(),
		MCPServers: []agent.MCPServerConfig{{Name: "owned", Command: "/usr/bin/false"}},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	err = h.Stop(t.Context())
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("Stop error = %v, want typed MCP cleanup failure", err)
	}
	if strings.Contains(err.Error(), secretSentinel) {
		t.Fatalf("Stop error leaked config readback: %v", err)
	}
	events := drainEvents(t, h.Events(), time.Second)
	if len(events) != 1 {
		t.Fatalf("Stop events = %v, want one cleanup failure", kindsOf(events))
	}
	cleanup, ok := events[0].(agent.ErrorEvent)
	if !ok || cleanup.Code != "mcp_cleanup_failed" {
		t.Fatalf("Stop event = %#v, want observable mcp_cleanup_failed", events[0])
	}
	if _, statErr := os.Stat(ownedHome); !os.IsNotExist(statErr) {
		t.Fatalf("owned home survives failed clear: stat err=%v", statErr)
	}
	_, err = p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
	adaptationErr = nil
	if !errors.As(err, &adaptationErr) {
		t.Fatalf("poisoned provider acquire error = %v, want typed denial", err)
	}
}

func TestProvider_IncompatibleMCPConfigDeniedWhileLeaseIsLive(t *testing.T) {
	p, _ := newTestProvider(t)
	alpha := map[string]any{"alpha": map[string]any{"command": "server-alpha"}}
	beta := map[string]any{"beta": map[string]any{"command": "server-beta"}}

	release, err := p.acquireMCPConfig(t.Context(), alpha, t.TempDir())
	if err != nil {
		t.Fatalf("acquire alpha: %v", err)
	}
	defer func() { _ = release() }()

	_, err = p.acquireMCPConfig(t.Context(), beta, t.TempDir())
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed || adaptationErr.Channel != agent.ToolChannelMCPServer {
		t.Fatalf("incompatible live config error = %v, want typed MCP application denial", err)
	}
}

func TestLiveCodex_IsolatedConfigReadWriteClearAndAmbientDigestUnchanged(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex binary not on PATH")
	}
	root := t.TempDir()
	ambientHome := filepath.Join(root, "ambient-codex-home")
	fakeHome := filepath.Join(root, "fake-home")
	for _, dir := range []string{ambientHome, fakeHome} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Base(dir), err)
		}
	}
	ambientConfig := filepath.Join(ambientHome, "config.toml")
	ambientBytes := []byte("[mcp_servers.ambient_probe]\ncommand = \"/usr/bin/false\"\n")
	if err := os.WriteFile(ambientConfig, ambientBytes, 0o600); err != nil {
		t.Fatalf("write fake ambient config: %v", err)
	}
	if err := os.Chmod(ambientConfig, 0o600); err != nil {
		t.Fatalf("chmod fake ambient config: %v", err)
	}
	before := sha256.Sum256(ambientBytes)
	t.Setenv("CODEX_HOME", ambientHome)

	p, err := New(Options{CodexBin: bin, Cwd: root, Env: map[string]string{"HOME": fakeHome}})
	if err != nil {
		t.Fatalf("New(real isolated app-server): %v", err)
	}
	startTestProvider(t, p)
	ownedHome := p.config.home
	ownedConfig := p.config.configPath
	if sameResolvedPath(ownedHome, ambientHome) {
		t.Fatal("provider reused ambient CODEX_HOME")
	}
	if err := p.config.validate(); err != nil {
		t.Fatalf("owned boundary validation: %v", err)
	}

	httpFixture := newCodexFakeMCPHTTP(t)
	desired := mcpServersConfig([]agent.MCPServerConfig{
		{Name: "live_stdio", Command: os.Args[0], Env: map[string]string{codexFakeMCPStdioEnv: "1"}},
		{Name: "live_http", Type: "http", URL: httpFixture.server.URL, Headers: map[string]string{"X-Probe": "nonce"}},
	})
	release, err := p.acquireMCPConfig(t.Context(), desired, root)
	if err != nil {
		_ = p.Shutdown(context.Background())
		t.Fatalf("live acquire: %v", err)
	}
	body, err := os.ReadFile(ownedConfig)
	if err != nil {
		t.Fatalf("read owned config: %v", err)
	}
	if !strings.Contains(string(body), "[mcp_servers.live_stdio]") || !strings.Contains(string(body), "[mcp_servers.live_http.http_headers]") {
		t.Fatalf("owned config omitted proven native stdio/HTTP shapes: %s", body)
	}
	if httpFixture.initialized.Load() == 0 || !httpFixture.headerSeen.Load() {
		t.Fatal("live HTTP MCP fixture was not initialized with its configured header")
	}
	if err := release(); err != nil {
		t.Fatalf("live clear/readback: %v", err)
	}
	afterBytes, err := os.ReadFile(ambientConfig)
	if err != nil {
		t.Fatalf("read fake ambient config after probe: %v", err)
	}
	after := sha256.Sum256(afterBytes)
	if before != after || string(afterBytes) != string(ambientBytes) {
		t.Fatal("fake ambient user config changed during isolated live probe")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Stat(ownedHome); !os.IsNotExist(err) {
		t.Fatalf("owned CODEX_HOME survives Shutdown: stat err=%v", err)
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
	ownedHome := p.config.home
	defer fs.close()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Stat(ownedHome); !os.IsNotExist(err) {
		t.Fatalf("Shutdown left owned config home: stat err=%v", err)
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
	ownedHome := p.config.home
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
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, statErr := os.Stat(ownedHome)
		if os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("app-server crash left owned config home: stat err=%v", statErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandle_AppServerCrashReportsConfigDestructionFailure(t *testing.T) {
	root := t.TempDir()
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-crash-cleanup")
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		configTempDir: root,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	startTestProvider(t, p)
	client := p.client
	t.Cleanup(func() {
		client.Stop(errors.New("test complete"))
		fs.close()
	})

	release, err := p.acquireMCPConfig(t.Context(), map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("acquire baseline: %v", err)
	}
	h := newHandle(p, client, agent.Spec{}, HandleOptions{})
	h.mcpRelease = release
	otherInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("stat replacement parent identity: %v", err)
	}
	p.config.parentInfo = otherInfo
	// Model the crash state after Client.Stop has recorded stream closure:
	// release cannot clear through RPC and must destroy the owned boundary.
	p.client = nil
	h.failNow(errors.New("synthetic app-server crash"))

	events := drainEvents(t, h.Events(), time.Second)
	if len(events) != 1 {
		t.Fatalf("crash events = %v, want one cleanup failure", kindsOf(events))
	}
	cleanup, ok := events[0].(agent.ErrorEvent)
	if !ok || cleanup.Code != "mcp_cleanup_failed" {
		t.Fatalf("crash event = %#v, want observable mcp_cleanup_failed", events[0])
	}
	if err := p.Shutdown(context.Background()); err == nil || !strings.Contains(err.Error(), "parent identity changed") {
		t.Fatalf("Shutdown error = %v, want persistent cleanup failure", err)
	}
}

func TestProvider_StartFailureClearsAppliedMCPBeforeReturning(t *testing.T) {
	fs, stdinW, stdoutR := newFakeServer()
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
				params, _ := msg["params"].(map[string]any)
				edits, _ := params["edits"].([]any)
				edit, _ := edits[0].(map[string]any)
				value, _ := edit["value"].(map[string]any)
				fs.mu.Lock()
				fs.activeMCP = value
				fs.mcpWrites = append(fs.mcpWrites, method)
				fs.mu.Unlock()
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"filePath": params["filePath"], "status": "ok", "version": "fake",
				}})
			case method == "config/read" && hasID:
				fs.mu.Lock()
				active := fs.activeMCP
				fs.mu.Unlock()
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"config": map[string]any{codexMCPConfigKeyPath: active}, "origins": map[string]any{},
				}})
			case method == "mcpServerStatus/list" && hasID:
				fs.mu.Lock()
				active := fs.activeMCP
				fs.mu.Unlock()
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{
					"data": fakeMCPStatusData(active),
				}})
			case method == "thread/start" && hasID:
				fs.write(t, map[string]any{"jsonrpc": "2.0", "id": idRaw, "error": map[string]any{
					"code": -32600, "message": "synthetic thread failure",
				}})
			case hasID:
				fs.replyOK(t, idRaw)
			}
		}
	}()
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	_, err = p.Spawn(t.Context(), agent.Spec{
		Cwd: t.TempDir(), MCPServers: []agent.MCPServerConfig{{Name: "owned", Command: "/usr/bin/false"}},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn error = %v, want ErrSpawnFailed", err)
	}
	fs.mu.Lock()
	active := fs.activeMCP
	writes := len(fs.mcpWrites)
	fs.mu.Unlock()
	if len(active) != 0 || writes != 2 {
		t.Fatalf("start failure cleanup active=%v writes=%d, want empty after apply+clear", active, writes)
	}
}

func TestMCPConfigReadbackRejectsUnexpectedExtraServer(t *testing.T) {
	raw := json.RawMessage(`{"config":{"mcp_servers":{"expected":{"command":"/usr/bin/false"},"ambient":{"command":"/usr/bin/true"}}}}`)
	desired := map[string]any{"expected": map[string]any{"command": "/usr/bin/false"}}
	if mcpConfigReadbackMatches(raw, desired) {
		t.Fatal("readback with undeclared ambient MCP server was accepted")
	}
	if mcpConfigReadbackMatches(json.RawMessage(`{"config":{}}`), map[string]any{}) {
		t.Fatal("readback without an explicit mcp_servers value proved an empty baseline")
	}
}

func TestMCPConfigReadbackRequiresHeadlessMCPApprovalSeed(t *testing.T) {
	raw := json.RawMessage(`{"config":{"mcp_servers":{"requested":{"command":"server"}}}}`)
	desired := map[string]any{"requested": map[string]any{
		"command":                     "server",
		"default_tools_approval_mode": codexMCPToolsApprovalApprove,
	}}
	if mcpConfigReadbackMatches(raw, desired) {
		t.Fatal("readback without the requested MCP approval seed was accepted")
	}
}
