package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// fakeEnv returns a Getenv stub that reads from the supplied map.
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// fakeLookPath returns a LookPath stub that resolves names from the
// supplied map; returns exec.ErrNotFound for any name not in the map.
func fakeLookPath(resolved map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if path, ok := resolved[name]; ok {
			return path, nil
		}
		return "", exec.ErrNotFound
	}
}

// ─── Construction tests (CLI mode) ───────────────────────────────────────────

func TestNew_CLIMode_BinaryFound(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/opencode"}),
		Getenv:   fakeEnv(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.binary != "/usr/local/bin/opencode" {
		t.Errorf("binary: want resolved path, got %q", p.binary)
	}
	if p.endpoint != "" {
		t.Errorf("endpoint: want empty in CLI mode, got %q", p.endpoint)
	}
}

func TestNew_CLIMode_CustomBinary(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		Binary:   "opencode-custom",
		LookPath: fakeLookPath(map[string]string{"opencode-custom": "/opt/bin/opencode-custom"}),
		Getenv:   fakeEnv(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.binary != "/opt/bin/opencode-custom" {
		t.Errorf("binary: want resolved path, got %q", p.binary)
	}
}

// ─── Construction tests (HTTP-server fallback) ─────────────────────────────

func TestNew_HTTPMode_LiveServer_Succeeds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	p, err := New(Options{
		Endpoint: srv.URL,
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil), // no binary
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.endpoint != srv.URL {
		t.Errorf("endpoint: want %q, got %q", srv.URL, p.endpoint)
	}
	if p.binary != "" {
		t.Errorf("binary: want empty in HTTP mode, got %q", p.binary)
	}
}

func TestNew_HTTPMode_404IsLive(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	// Inject the server's own client so the probe doesn't race against the
	// shared http.DefaultTransport's CloseIdleConnections — observed flaking
	// under parallel httptest load on Go 1.25 CI runners.
	if _, err := New(Options{
		Endpoint:   srv.URL,
		Getenv:     fakeEnv(nil),
		LookPath:   fakeLookPath(nil),
		HTTPClient: srv.Client(),
	}); err != nil {
		t.Fatalf("New: want nil for 404, got %v", err)
	}
}

func TestNew_HTTPMode_5xxFailsAsUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(Options{
		Endpoint:   srv.URL,
		Getenv:     fakeEnv(nil),
		LookPath:   fakeLookPath(nil),
		HTTPClient: srv.Client(),
	})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
}

func TestNew_HTTPMode_ConnectionRefused(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		Endpoint: "http://127.0.0.1:1",
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil),
	})
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
}

func TestNew_HTTPMode_EnvEndpointFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := New(Options{
		Getenv:   fakeEnv(map[string]string{EnvEndpoint: srv.URL}),
		LookPath: fakeLookPath(nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.endpoint != srv.URL {
		t.Errorf("endpoint: want %q (from env), got %q", srv.URL, p.endpoint)
	}
}

func TestNew_HTTPMode_APIKeyForwardedAsBearer(t *testing.T) {
	t.Parallel()
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, err := New(Options{
		Endpoint: srv.URL,
		APIKey:   "secret-token",
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil),
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization: want %q, got %q", "Bearer secret-token", gotAuth)
	}
}

func TestNew_SkipProbeBypassesNetwork(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		Endpoint:  "http://invalid.example.test:9",
		SkipProbe: true,
		Getenv:    fakeEnv(nil),
		LookPath:  fakeLookPath(nil),
	})
	if err != nil {
		t.Fatalf("New with SkipProbe: %v", err)
	}
	if p.endpoint != "http://invalid.example.test:9" {
		t.Errorf("endpoint: want preserved, got %q", p.endpoint)
	}
}

// ─── Provider interface tests ─────────────────────────────────────────────────

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if got := p.Name(); got != agent.ProviderOpenCode {
		t.Fatalf("Name: want %q, got %q", agent.ProviderOpenCode, got)
	}
}

func TestProvider_Capabilities(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	caps := p.Capabilities()
	// Lane B (07 §7) flips these four to true.
	if !caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want true (Lane B Inject)")
	}
	if !caps.SupportsSessionResume {
		t.Error("SupportsSessionResume: want true (Lane B Resume)")
	}
	if caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want false (opencode plugins are not donmai tool plugins)")
	}
	if !caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want true (Lane B opencode.json permission map)")
	}
	if !caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want true (per-session project MCP config)")
	}
	// SupportsReasoningEffort: true — mapped to --variant.
	if !caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort: want true (mapped to --variant)")
	}
	if caps.HumanLabel != "OpenCode" {
		t.Errorf("HumanLabel: want %q, got %q", "OpenCode", caps.HumanLabel)
	}
}

// ─── Spawn tests ─────────────────────────────────────────────────────────────

// TestProvider_Spawn_HTTPMode_AttachUnreachable verifies that Spawn in
// attach mode (binary empty, endpoint set) takes Lane B and fails with
// ErrSpawnFailed when the attached server is unreachable (create-session
// dial refused), rather than hanging.
func TestProvider_Spawn_HTTPMode_AttachUnreachable(t *testing.T) {
	t.Parallel()
	// Attach mode: binary is empty, endpoint points at a dead port.
	p := &Provider{endpoint: "http://127.0.0.1:1", apiKey: ""}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "anything"})
	if h != nil {
		_ = h.Stop(ctx)
		t.Fatal("Spawn: want nil handle when attached server unreachable")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want ErrSpawnFailed, got %v", err)
	}
}

func TestProvider_UseServerLane_ProjectConfigFieldsUseConfiguredCLI(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "/tmp/opencode"}
	tests := []struct {
		name string
		spec agent.Spec
	}{
		{"endpoint", agent.Spec{Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, BaseURL: "http://127.0.0.1:9/v1"}}},
		{"allowed tools", agent.Spec{AllowedTools: []string{"Read"}}},
		{"disallowed tools", agent.Spec{DisallowedTools: []string{"Write"}}},
		{"empty permission config", agent.Spec{PermissionConfig: &agent.PermissionConfig{}}},
		{"allow permission config", agent.Spec{PermissionConfig: &agent.PermissionConfig{DefaultDecision: "allow"}}},
		{"mcp servers", agent.Spec{MCPServers: []agent.MCPServerConfig{{Name: "tools", Command: "server"}}}},
		{"mcp tool names", agent.Spec{MCPToolNames: []string{"mcp__tools__read"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if p.useServerLane(tt.spec) {
				t.Fatal("project-config-bearing one-shot unexpectedly routed to the incompatible v2 server lane")
			}
		})
	}
	if p.useServerLane(agent.Spec{Prompt: "plain one-shot", Model: "provider/model"}) {
		t.Fatal("plain one-shot unexpectedly routed away from Lane A")
	}
}

func TestProvider_ConfiguredCLIOwnsAndCleansProjectConfig(t *testing.T) {
	bin := writeFakeOpenCodeScript(t)
	configRoot := t.TempDir()
	p := &Provider{binary: bin, configTempDir: configRoot}
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:          "configured one-shot",
		Cwd:             t.TempDir(),
		Autonomous:      true,
		AllowedTools:    []string{"Read"},
		DisallowedTools: []string{"Write"},
		MCPServers: []agent.MCPServerConfig{{
			Name: "tools", Command: "/usr/bin/false",
		}},
		MCPToolNames: []string{"mcp__tools__read"},
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyLocal, Model: "fixture-model", BaseURL: "http://127.0.0.1:9/v1",
			Protocol: agent.ProtoOpenAIChat, Host: agent.HostLocal, Mechanism: agent.AuthNone, Auth: agent.AuthLocal,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cliHandle, ok := h.(*openCodeHandle)
	if !ok {
		t.Fatalf("Spawn handle = %T, want *openCodeHandle", h)
	}
	configuredPath := ""
	for _, entry := range cliHandle.cmd.Env {
		if strings.HasPrefix(entry, OCConfigEnvVar+"=") {
			configuredPath = strings.TrimPrefix(entry, OCConfigEnvVar+"=")
			break
		}
	}
	if configuredPath == "" {
		t.Fatal("configured CLI child env omitted OPENCODE_CONFIG")
	}
	info, err := os.Stat(configuredPath)
	if err != nil {
		t.Fatalf("stat owned config before Stop: %v", err)
	}
	if got := info.Mode().Perm(); got != openCodeConfigMode {
		t.Fatalf("owned config mode = %04o, want %04o", got, openCodeConfigMode)
	}
	parent := filepath.Dir(configuredPath)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat owned config boundary: %v", err)
	}
	if got := parentInfo.Mode().Perm(); got != openCodeHomeMode {
		t.Fatalf("owned config boundary mode = %04o, want %04o", got, openCodeHomeMode)
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(parent); !os.IsNotExist(err) {
		t.Fatalf("owned config boundary survived Stop: %v", err)
	}
}

func TestProvider_ConfiguredCLIManyEnvEntriesGrowWithoutCapacitySum(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: writeFakeOpenCodeScript(t), configTempDir: t.TempDir()}
	specEnv := make(map[string]string, 4096)
	for i := 0; i < 4096; i++ {
		specEnv[fmt.Sprintf("SAFE_%04d", i)] = "value"
	}
	h, err := p.spawnCLI(t.Context(), agent.Spec{
		Prompt: "large env", Cwd: t.TempDir(), Env: specEnv,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyLocal, Model: "fixture-model", BaseURL: "http://127.0.0.1:9/v1",
			Protocol: agent.ProtoOpenAIChat, Host: agent.HostLocal, Mechanism: agent.AuthNone, Auth: agent.AuthLocal,
		},
	})
	if err != nil {
		t.Fatalf("spawnCLI: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	joined := strings.Join(h.cmd.Env, "\x00")
	if !strings.Contains(joined, "SAFE_4095=value") || !strings.Contains(joined, OCConfigEnvVar+"=") {
		t.Fatal("large configured CLI environment omitted tail or owned config entry")
	}
}

func TestProvider_ExternalAttachPolicyAndMCPFailWithDeniedReceipt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spec    agent.Spec
		channel agent.ToolLifecycleChannel
	}{
		{"allowed tools", agent.Spec{AllowedTools: []string{"Read"}}, agent.ToolChannelAllowedTools},
		{"disallowed tools", agent.Spec{DisallowedTools: []string{"Write"}}, agent.ToolChannelDisallowedTools},
		{"permission config", agent.Spec{PermissionConfig: &agent.PermissionConfig{DefaultDecision: "allow"}}, agent.ToolChannelPermissionConfig},
		{"mcp servers", agent.Spec{MCPServers: []agent.MCPServerConfig{{Name: "tools", Command: "server"}}}, agent.ToolChannelMCPServer},
		{"mcp tool names", agent.Spec{MCPToolNames: []string{"mcp__tools__read"}}, agent.ToolChannelMCPToolNames},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clientBuilt := false
			p := &Provider{
				endpoint: "http://attached.invalid",
				clientFactory: func(_, _ string) serverClient {
					clientBuilt = true
					return newFakeClient()
				},
			}
			var receipts []agent.ToolLifecycleReceipt
			spec := tt.spec
			spec.OnToolLifecycleAdapted = func(receipt agent.ToolLifecycleReceipt) error {
				receipts = append(receipts, receipt)
				return nil
			}
			_, err := p.Spawn(t.Context(), spec)
			if !errors.Is(err, agent.ErrSpawnFailed) {
				t.Fatalf("Spawn error = %v, want ErrSpawnFailed", err)
			}
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialDeliveryUnsupported || adaptationErr.Channel != tt.channel {
				t.Fatalf("Spawn error = %v, want typed delivery denial on %s", err, tt.channel)
			}
			if len(receipts) != 1 || receipts[0].Decision != "denied" {
				t.Fatalf("receipts = %+v, want one denied receipt", receipts)
			}
			if clientBuilt {
				t.Fatal("external client was built before policy/MCP denial")
			}
		})
	}
}

func TestProvider_ExternalAttachEndpointFailsBeforeAdaptationOrSession(t *testing.T) {
	t.Parallel()
	clientBuilt := false
	p := &Provider{
		endpoint: "http://attached.invalid",
		clientFactory: func(_, _ string) serverClient {
			clientBuilt = true
			return newFakeClient()
		},
	}
	receiptCalled := false
	_, err := p.Spawn(t.Context(), agent.Spec{
		Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, Model: "gpt-x", BaseURL: "http://compat/v1"},
		OnToolLifecycleAdapted: func(agent.ToolLifecycleReceipt) error {
			receiptCalled = true
			return nil
		},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) || !errors.Is(err, errExternalAttachConfigUnproven) {
		t.Fatalf("Spawn error = %v, want typed external-attach config denial", err)
	}
	if receiptCalled {
		t.Fatal("tool lifecycle receipt emitted before endpoint authority denial")
	}
	if clientBuilt {
		t.Fatal("external client was built before endpoint authority denial")
	}
}

// TestProvider_Spawn_BinaryNotFound verifies that Spawn returns
// ErrSpawnFailed when the binary path is invalid.
func TestProvider_Spawn_BinaryNotFound(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "/nonexistent/opencode-fake-binary"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "hello"})
	if h != nil {
		_ = h.Stop(ctx)
		t.Fatal("Spawn: want nil handle when binary not found")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("Spawn err: want ErrSpawnFailed, got %v", err)
	}
}

func TestProvider_ConfiguredCLISpawnFailureRemovesSecretConfigWithoutWorktreeResidue(t *testing.T) {
	const secretSentinel = "opencode-spawn-failure-bearer-must-not-surface"
	repo := t.TempDir()
	if output, err := testGitCommand(t, "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	tempRoot := t.TempDir()
	p := &Provider{binary: "/nonexistent/opencode-fake-binary", configTempDir: tempRoot}
	var receipts []agent.ToolLifecycleReceipt
	_, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "must not start",
		Cwd:    repo,
		MCPServers: []agent.MCPServerConfig{{
			Name: "platform", Type: "http", URL: "https://example.invalid/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + secretSentinel},
		}},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn error = %v, want ErrSpawnFailed", err)
	}
	if strings.Contains(err.Error(), secretSentinel) {
		t.Fatalf("Spawn error leaked MCP bearer: %v", err)
	}
	receiptJSON, marshalErr := json.Marshal(receipts)
	if marshalErr != nil {
		t.Fatalf("marshal receipts: %v", marshalErr)
	}
	if strings.Contains(string(receiptJSON), secretSentinel) {
		t.Fatalf("tool lifecycle receipt leaked MCP bearer: %s", receiptJSON)
	}
	entries, readErr := os.ReadDir(tempRoot)
	if readErr != nil {
		t.Fatalf("read config temp root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("spawn failure left owned config entries: %v", entries)
	}
	status, statusErr := testGitCommand(t, "-C", repo, "status", "--porcelain", "--untracked-files=all").Output()
	if statusErr != nil {
		t.Fatalf("git status: %v", statusErr)
	}
	if len(status) != 0 {
		t.Fatalf("spawn failure left worktree residue: %q", status)
	}
	p.mu.Lock()
	resources := len(p.resources)
	p.mu.Unlock()
	if resources != 0 {
		t.Fatalf("spawn failure left %d registered resources", resources)
	}
}

// TestProvider_Spawn_FakeCLI exercises the full Spawn → Handle →
// events pipeline using a fake `opencode` script that outputs
// OpenCode NDJSON events.
func TestProvider_Spawn_FakeCLI_NDJSON(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeOpenCodeScript(t)
	p := &Provider{binary: scriptPath}

	// 30s ceiling: in normal conditions the fixture completes in <50ms
	// (collectUntilResult returns immediately on ResultEvent). The
	// generous timeout exists only as a -race + full-suite-load safety
	// net so fork/exec + scanner setup latency under contention doesn't
	// time-bomb a deterministic NDJSON fixture.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "list files"})
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("Spawn: returned nil handle")
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// Drain events until we observe a terminal ResultEvent OR the
	// spawn ctx fires. The opencode Handle keeps the events channel
	// open after the subprocess exits (mirroring claude's Inject
	// support), so we cannot range-until-close here — but ResultEvent
	// (from step_finish reason=stop) is the terminal sentinel from
	// the NDJSON stream and the parent reader emits no further events
	// after it. Tying the deadline to the spawn ctx (channel-based)
	// instead of a wall-clock hardDeadline keeps the test resilient
	// under -race + full-suite load where fork/exec + bufio.Scanner
	// can take >5s before the first line reaches the consumer
	// goroutine.
	events := collectUntilResult(ctx, t, h)

	var initCount int
	var gotAssistant, gotResult bool
	for _, ev := range events {
		switch ev.(type) {
		case agent.InitEvent:
			initCount++
		case agent.AssistantTextEvent:
			gotAssistant = true
		case agent.ResultEvent:
			gotResult = true
		}
	}
	if initCount != 1 {
		t.Errorf("events: want exactly one InitEvent across model/tool steps, got %d", initCount)
	}
	if !gotAssistant {
		t.Error("events: want AssistantTextEvent (from text event)")
	}
	if !gotResult {
		t.Error("events: want ResultEvent (from step_finish reason=stop)")
	}
}

// TestProvider_Spawn_SuccessfulRun_ExactlyOneTerminal is the D-1
// regression. On a successful run (step_finish reason=stop → ResultEvent)
// the stdout reader must NOT append a spurious spawn_no_result ErrorEvent
// on scanner EOF. Before the fix, readStdout declared `terminal := false`
// and never set it, so every successful run emitted a second terminal
// event — violating the Provider contract ("exactly one terminal
// ResultEvent … then closes", agent/provider.go). This test drains past
// the terminal event and asserts the shared terminal-event ordering
// contract; it goes red on the pre-fix code and green after.
func TestProvider_Spawn_SuccessfulRun_ExactlyOneTerminal(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeOpenCodeScript(t)
	p := &Provider{binary: scriptPath}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "list files"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// Drain ALL events the reader emits. The fixture stream is finite and
	// the subprocess exits after step_finish; the handle keeps the events
	// channel open until Stop, so we read past the terminal ResultEvent
	// with an idle window to capture any spurious post-terminal event
	// (rather than let Stop's channel close race it away). The window is 5s
	// (not 2s) so fork/exec + bufio.Scanner setup under -race + full-suite
	// load cannot elapse the idle timer before the first line reaches the
	// consumer — the W0-flagged flake this test shares with the claude
	// conformance test (12-work-breakdown.md W0 item 2 completion note).
	events := drainWithIdle(ctx, h, 5*time.Second)

	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal-event contract violated: %v\nevents: %s", err, kindsOf(events))
	}
	for _, ev := range events {
		if er, ok := ev.(agent.ErrorEvent); ok && er.Code == "spawn_no_result" {
			t.Errorf("D-1 regression: spurious spawn_no_result ErrorEvent after successful run; events: %s", kindsOf(events))
		}
	}
}

// drainWithIdle collects events from h until the events channel closes,
// ctx fires, or no event arrives within idle. The opencode handle keeps
// its channel open after the subprocess exits (Stop closes it), so an
// idle gap is how the test observes that the reader has emitted everything
// it will — including any erroneous post-terminal event.
func drainWithIdle(ctx context.Context, h agent.Handle, idle time.Duration) []agent.Event {
	var got []agent.Event
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return got
		case <-ctx.Done():
			return got
		}
	}
}

func kindsOf(events []agent.Event) string {
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = string(ev.Kind())
	}
	return strings.Join(kinds, ",")
}

// collectUntilResult drains events from h until a terminal ResultEvent
// is observed, the events channel closes, or ctx fires. The fixture
// NDJSON stream is deterministic — step_start, text, step_finish — and
// the parent reader emits no events after the terminal step_finish
// reason=stop (which maps to ResultEvent), so we can return the moment
// we see it without an extra idle wait. ctx (the same one passed to
// Spawn) is the cancellation signal: tying the test deadline to the
// spawn lifetime instead of a hard-coded wall-clock value avoids the
// -race + full-suite-load flake where fork/exec + bufio.Scanner could
// take longer than a fixed deadline.
func collectUntilResult(ctx context.Context, t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var got []agent.Event
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
			if _, ok := ev.(agent.ResultEvent); ok {
				return got
			}
		case <-ctx.Done():
			t.Logf("collectUntilResult: ctx cancelled after %d events: %v", len(got), ctx.Err())
			return got
		}
	}
}

// ─── NDJSON mapper unit tests ─────────────────────────────────────────────────

func TestMapOpenCodeLine_StepStart_EmitsInit(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"step_start","sessionID":"ses_abc123","part":{"type":"step-start"}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	init, ok := evs[0].(agent.InitEvent)
	if !ok {
		t.Fatalf("want InitEvent, got %T", evs[0])
	}
	if init.SessionID != "ses_abc123" {
		t.Errorf("SessionID: want %q, got %q", "ses_abc123", init.SessionID)
	}
}

func TestMapOpenCodeLine_Text_EmitsAssistantText(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"text","sessionID":"ses_x","part":{"type":"text","text":"Hello world"}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	txt, ok := evs[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("want AssistantTextEvent, got %T", evs[0])
	}
	if txt.Text != "Hello world" {
		t.Errorf("Text: want %q, got %q", "Hello world", txt.Text)
	}
}

func TestMapOpenCodeLine_Text_Empty_EmitsNothing(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"text","sessionID":"ses_x","part":{"type":"text","text":""}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 0 {
		t.Errorf("want no events for empty text, got %d: %v", len(evs), evs)
	}
}

func TestMapOpenCodeLine_ToolUse_Completed(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"tool_use","sessionID":"ses_x","part":{"type":"tool","tool":"read","callID":"call_1","state":{"status":"completed","input":{"filePath":"/tmp"},"output":"contents"}}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 2 {
		t.Fatalf("want 2 events (ToolUse + ToolResult), got %d: %v", len(evs), evs)
	}
	tu, ok := evs[0].(agent.ToolUseEvent)
	if !ok {
		t.Fatalf("want ToolUseEvent first, got %T", evs[0])
	}
	if tu.ToolName != "read" {
		t.Errorf("ToolName: want %q, got %q", "read", tu.ToolName)
	}
	tr, ok := evs[1].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("want ToolResultEvent second, got %T", evs[1])
	}
	if tr.Content != "contents" {
		t.Errorf("Content: want %q, got %q", "contents", tr.Content)
	}
	if tr.IsError {
		t.Error("IsError: want false for status=completed")
	}
}

func TestMapOpenCodeLine_ToolUse_Pending(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"tool_use","sessionID":"ses_x","part":{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"pending","input":{}}}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event (ToolUse only) for pending state, got %d", len(evs))
	}
	if _, ok := evs[0].(agent.ToolUseEvent); !ok {
		t.Fatalf("want ToolUseEvent, got %T", evs[0])
	}
}

func TestMapOpenCodeLine_StepFinish_Stop_EmitsResult(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"step_finish","sessionID":"ses_x","part":{"type":"step-finish","reason":"stop","tokens":{"total":100,"input":80,"output":20},"cost":0.001}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 2 {
		t.Fatalf("want LlmCallEvent + ResultEvent, got %d", len(evs))
	}
	llm, ok := evs[0].(agent.LlmCallEvent)
	if !ok || llm.InputTokens != 80 || llm.OutputTokens != 20 || llm.UsageSource != agent.LlmUsageProvider {
		t.Fatalf("want provider LlmCallEvent(80/20), got %#v", evs[0])
	}
	r, ok := evs[1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("want ResultEvent, got %T", evs[1])
	}
	if !r.Success {
		t.Error("ResultEvent.Success: want true")
	}
	if r.Cost == nil {
		t.Error("ResultEvent.Cost: want non-nil")
	} else if r.Cost.InputTokens != 80 {
		t.Errorf("Cost.InputTokens: want 80, got %d", r.Cost.InputTokens)
	}
}

func TestMapOpenCodeLine_StepFinish_ToolCalls_EmitsLlmCall(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"step_finish","sessionID":"ses_x","part":{"type":"step-finish","reason":"tool-calls"}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want one LlmCallEvent for tool-calls step_finish, got %d", len(evs))
	}
	if llm, ok := evs[0].(agent.LlmCallEvent); !ok || llm.FinishReason != "tool-calls" {
		t.Fatalf("want tool-calls LlmCallEvent, got %#v", evs[0])
	}
}

func TestMapOpenCodeLine_UnknownType_EmitsSystemEvent(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"something_new","sessionID":"ses_x","part":{}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(agent.SystemEvent); !ok {
		t.Fatalf("want SystemEvent for unknown type, got %T", evs[0])
	}
}

func TestMapOpenCodeLine_MissingType_EmitsErrorEvent(t *testing.T) {
	t.Parallel()
	line := []byte(`{"sessionID":"ses_x","part":{}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev, ok := evs[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("want ErrorEvent, got %T", evs[0])
	}
	if ev.Code != "missing_type" {
		t.Errorf("Code: want %q, got %q", "missing_type", ev.Code)
	}
}

func TestMapOpenCodeLine_InvalidJSON_EmitsErrorEvent(t *testing.T) {
	t.Parallel()
	line := []byte(`not json`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev, ok := evs[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("want ErrorEvent for bad JSON, got %T", evs[0])
	}
	if ev.Code != "decode_envelope" {
		t.Errorf("Code: want %q, got %q", "decode_envelope", ev.Code)
	}
}

// ─── buildOpenCodeArgs unit tests ─────────────────────────────────────────────

func TestBuildOpenCodeArgs_CoreBits(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{})
	if !contains(argv, "run") {
		t.Errorf("want 'run' in %v", argv)
	}
	if !contains(argv, "--format") {
		t.Errorf("want '--format' in %v", argv)
	}
	if indexOf(argv, "--format")+1 < len(argv) && argv[indexOf(argv, "--format")+1] != "json" {
		t.Errorf("want --format json, got %v", argv)
	}
}

func TestBuildOpenCodeArgs_Autonomous(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Autonomous: true})
	// D-2: opencode 1.x has no --dangerously-skip-permissions; autonomy
	// maps to --auto (explicit denies stay enforced).
	if !contains(argv, "--auto") {
		t.Errorf("want --auto in %v", argv)
	}
	if contains(argv, "--dangerously-skip-permissions") {
		t.Errorf("want no --dangerously-skip-permissions (not a real opencode 1.x flag) in %v", argv)
	}
}

func TestBuildOpenCodeArgs_NonAutonomous(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Autonomous: false})
	if contains(argv, "--auto") {
		t.Errorf("want no --auto in %v", argv)
	}
}

func TestBuildOpenCodeArgs_Cwd_UsesExplicitDirFlag(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Cwd: "/workspace"})
	idx := indexOf(argv, "--dir")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "/workspace" {
		t.Errorf("want --dir /workspace to override inherited PWD, got %v", argv)
	}
}

func TestBuildOpenCodeArgs_Model(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Model: "anthropic/claude-opus-4"})
	if !contains(argv, "--model") {
		t.Errorf("want --model in %v", argv)
	}
}

func TestBuildOpenCodeArgs_EndpointModelUsesOwnedProviderRef(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{
		Model: "stale-model",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyLocal,
			Model:   "resolved-model",
		},
	})
	idx := indexOf(argv, "--model")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != OCProviderID+"/resolved-model" {
		t.Errorf("want --model %s/resolved-model, got %v", OCProviderID, argv)
	}
}

func TestBuildOpenCodeArgs_Effort_MapsToVariant(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Effort: agent.EffortHigh})
	if !contains(argv, "--variant") {
		t.Errorf("want --variant in %v", argv)
	}
	idx := indexOf(argv, "--variant")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "high" {
		t.Errorf("want --variant high, got %v", argv)
	}
}

// TestProvider_Resume_EmptySessionUnsupported: Resume is real on Lane B (07 §9),
// but an empty session id is still rejected with ErrUnsupported.
func TestProvider_Resume_EmptySessionUnsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	_, err := p.Resume(context.Background(), "", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume(\"\") err: want ErrUnsupported, got %v", err)
	}
}

// TestProvider_Resume_Wired: Resume brings up a Lane-B server and reattaches to
// the given session id (no CreateSession call — the session already exists).
func TestProvider_Resume_Wired(t *testing.T) {
	t.Parallel()
	fc := newFakeClient()
	fc.sessionID = "ses_resumed"
	p := &Provider{
		endpoint:      "http://attached.invalid",
		clientFactory: func(_, _ string) serverClient { return fc },
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "ses_resumed", agent.Spec{Prompt: "continue", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	if h.SessionID() != "ses_resumed" {
		t.Errorf("SessionID = %q, want ses_resumed", h.SessionID())
	}
	// Resume prompt carries Resume:true.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.prompts) != 1 || !fc.prompts[0].Resume {
		t.Errorf("resume prompts = %+v, want one with Resume=true", fc.prompts)
	}
}

func TestProvider_Shutdown_NoOp(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: want nil, got %v", err)
	}
}

// Compile-time assertion: Provider satisfies agent.Provider.
var _ agent.Provider = (*Provider)(nil)

// ─── helpers ─────────────────────────────────────────────────────────────────

func mustNew(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Options{
		SkipProbe: true,
		Getenv:    fakeEnv(nil),
		LookPath:  fakeLookPath(nil), // no binary → HTTP mode
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func indexOf(slice []string, s string) int {
	for i, v := range slice {
		if v == s {
			return i
		}
	}
	return -1
}

const fakeOpenCodeNDJSON = `{"type":"step_start","sessionID":"ses_opencode_test_001","part":{"type":"step-start"}}
{"type":"text","sessionID":"ses_opencode_test_001","part":{"type":"text","text":"Hello from opencode fake"}}
{"type":"step_start","sessionID":"ses_opencode_test_001","part":{"type":"step-start"}}
{"type":"step_finish","sessionID":"ses_opencode_test_001","part":{"type":"step-finish","reason":"stop","tokens":{"total":50,"input":40,"output":10},"cost":0}}
`

// fakeOpenCodeScript is the path to the package-wide fake `opencode` CLI, and
// fakeOpenCodeSkip is the reason it could not be built (empty when it was).
// Both are written exactly once by TestMain and only read afterwards.
var (
	fakeOpenCodeScript string
	fakeOpenCodeSkip   string
)

// TestMain builds the fake `opencode` CLI ONCE, before any test — and
// therefore before any test forks a subprocess.
//
// The old per-test helper created a fresh script with os.CreateTemp and then
// exec'd it, which made this package's -race CI run flaky:
//
//	--- FAIL: TestProvider_Spawn_SuccessfulRun_ExactlyOneTerminal (0.00s)
//	    opencode_test.go:359: Spawn: agent: spawn failed:
//	    cmd start: fork/exec /tmp/fake-opencode-2026265744.sh: text file busy
//
// (run 30117668435, merge_group for PR #215).
//
// ETXTBSY is the kernel refusing to execve a file that some process still
// holds open for writing. The window is not the writing test's own descriptor
// — that one is closed before the exec. It is that fork() copies the whole
// descriptor table, and O_CLOEXEC only clears a descriptor at the child's
// execve, not at fork. So while test A sits between CreateTemp and Close on
// its script, any *other* test in this package that spawns a subprocess forks
// a child that momentarily holds a writable descriptor to A's script. If A
// reaches execve first, the kernel sees a writer and returns ETXTBSY. This
// package has four test files that spawn subprocesses, and they run
// t.Parallel(), so the interleaving is routine; -race widens the window
// enough to hit it. Upstream: golang/go#22315.
//
// Writing the file with the exec bit off and chmod-ing after close (the
// mitigation used in provider/harness/agycli) does not close this window —
// ETXTBSY is decided by the writer count on the inode at execve time, not by
// the mode bits at write time. Creating the script before any test starts
// does close it: after TestMain returns, no writable descriptor to that inode
// exists anywhere, so no fork can copy one. The script is read-only and
// stateless, so every test can share the one file; concurrent execs of the
// same read-only executable are fine.
func TestMain(m *testing.M) {
	code := func() int {
		sh, err := exec.LookPath("sh")
		if err != nil {
			fakeOpenCodeSkip = "sh not found on PATH: " + err.Error()
			return m.Run()
		}

		dir, err := os.MkdirTemp("", "opencode-fixtures-")
		if err != nil {
			fakeOpenCodeSkip = "create fixture dir: " + err.Error()
			return m.Run()
		}
		defer func() { _ = os.RemoveAll(dir) }()

		path := filepath.Join(dir, "fake-opencode.sh")
		script := "#!" + sh + "\ncat > /dev/null\nprintf '%s' " + shellQuote(fakeOpenCodeNDJSON)
		// Written without the exec bit, then chmod-ed after the descriptor is
		// gone: belt-and-braces with the once-before-any-fork ordering above.
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			fakeOpenCodeSkip = "write fake opencode script: " + err.Error()
			return m.Run()
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
			fakeOpenCodeSkip = "chmod fake opencode script: " + err.Error()
			return m.Run()
		}
		fakeOpenCodeScript = path
		return m.Run()
	}()
	os.Exit(code)
}

// writeFakeOpenCodeScript returns the path to the shared fake OpenCode CLI
// that emits minimal OpenCode NDJSON events. The script is built once by
// TestMain; see the ETXTBSY note there for why it is not built per test.
func writeFakeOpenCodeScript(t *testing.T) string {
	t.Helper()
	if fakeOpenCodeScript == "" {
		t.Skipf("fake opencode CLI unavailable — skipping fake-CLI test: %s", fakeOpenCodeSkip)
	}
	return fakeOpenCodeScript
}

func shellQuote(s string) string {
	replaced := strings.ReplaceAll(s, "'", `'\''`)
	return "'" + replaced + "'"
}

// TestFakeOpenCodeScript_ProducesExpectedLines is a sanity check for
// the fake script template.
func TestFakeOpenCodeScript_ProducesExpectedLines(t *testing.T) {
	t.Parallel()
	scriptPath := writeFakeOpenCodeScript(t)
	cmd := exec.Command(scriptPath) //nolint:gosec // test fixture: subprocess path is t.TempDir()-scoped
	cmd.Stdin = strings.NewReader("test prompt")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake opencode script exited non-zero: %v\nstdout: %s", err, out)
	}
	lines := 0
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		if scanner.Text() != "" {
			lines++
		}
	}
	if lines < 3 {
		t.Errorf("expected at least 3 NDJSON lines from fake script, got %d", lines)
	}
}
