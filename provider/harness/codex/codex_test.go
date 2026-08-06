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

func TestNew_HandshakeFailureReturnsProviderUnavailable(t *testing.T) {
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

	tempRoot := t.TempDir()
	_, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		HandshakeTimeout: 200 * time.Millisecond,
		configTempDir:    tempRoot,
	})
	if err == nil {
		t.Fatalf("expected handshake error, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("read config temp root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("handshake failure leaked owned config boundary: %v", entries)
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
	writeAt, readAt, threadAt := index("config/batchWrite"), index("config/read"), index("thread/start")
	if writeAt < 0 || readAt <= writeAt || threadAt <= readAt {
		t.Fatalf("method order = %v, want batchWrite -> config/read -> thread/start", methods)
	}
	if len(writes) != 2 {
		t.Fatalf("empty session writes = %d, want explicit apply + terminal clear", len(writes))
	}
	if len(active) != 0 {
		t.Fatalf("active MCP after terminal cleanup = %v, want empty", active)
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
	ownedHome := p.config.home
	ownedConfig := p.config.configPath
	if sameResolvedPath(ownedHome, ambientHome) {
		t.Fatal("provider reused ambient CODEX_HOME")
	}
	if err := p.config.validate(); err != nil {
		t.Fatalf("owned boundary validation: %v", err)
	}

	desired := mcpServersConfig([]agent.MCPServerConfig{
		{Name: "live_stdio", Command: "/usr/bin/false", Args: []string{"nonce"}},
		{Name: "live_http", Type: "http", URL: "http://127.0.0.1:9/mcp", Headers: map[string]string{"X-Probe": "nonce"}},
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
