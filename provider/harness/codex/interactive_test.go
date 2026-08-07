package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/agent"
)

func TestInteractiveArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{name: "empty prompt starts bare TUI", prompt: "", want: nil},
		{name: "non-empty prompt seeds the TUI", prompt: "fix the failing tests", want: []string{"fix the failing tests"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interactiveArgs(agent.Spec{Prompt: tt.prompt})
			if len(got) != len(tt.want) {
				t.Fatalf("interactiveArgs(%q) = %v, want %v", tt.prompt, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("interactiveArgs(%q)[%d] = %q, want %q", tt.prompt, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuildInteractiveLaunch_MixedMCPIsDeterministicSemanticAndSecretFree(t *testing.T) {
	t.Parallel()
	const (
		stdioSecret = "STDIO_SECRET_DO_NOT_PUT_IN_ARGV"
		httpSecret  = "HTTP_SECRET_DO_NOT_PUT_IN_ARGV"
	)
	spec := agent.Spec{
		Prompt:             "fix the failing tests",
		SystemPromptAppend: "use \"repo\" rules",
		Env:                map[string]string{"BASE": "kept"},
		MCPServers: []agent.MCPServerConfig{
			{
				Name: "z http \"snow 雪\"", Type: "http", URL: "https://example.invalid/a?b=\"c\"",
				Headers: map[string]string{"X-\"Auth\"": httpSecret, "X-Second": "second-secret"},
			},
			{
				Name: "a stdio", Type: "stdio", Command: "/tmp/tool \"quoted\"",
				Args: []string{"--line", "one\ntwo", "雪"}, Env: map[string]string{"TOKEN": stdioSecret, "ALPHA": "first"},
			},
		},
	}
	originalEnv := cloneInteractiveEnv(spec.Env)
	originalServers := cloneMCPServersForTest(spec.MCPServers)

	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatalf("buildInteractiveLaunch: %v", err)
	}
	if !reflect.DeepEqual(spec.Env, originalEnv) || !reflect.DeepEqual(spec.MCPServers, originalServers) {
		t.Fatal("buildInteractiveLaunch mutated input maps or slices")
	}
	if got := launch.argv[len(launch.argv)-1]; got != spec.Prompt {
		t.Fatalf("last argv = %q, want prompt %q", got, spec.Prompt)
	}
	strict := slices.Index(launch.argv, "--strict-config")
	if strict < 0 || strict >= len(launch.argv)-1 {
		t.Fatalf("--strict-config missing or after prompt: %q", launch.argv)
	}
	override := mcpOverrideFromArgs(t, launch.argv)
	for _, secret := range []string{stdioSecret, httpSecret, "second-secret"} {
		if strings.Contains(strings.Join(launch.argv, "\x00"), secret) {
			t.Fatalf("secret %q leaked into argv: %q", secret, launch.argv)
		}
	}

	var decoded map[string]any
	if err := toml.Unmarshal([]byte(override), &decoded); err != nil {
		t.Fatalf("override is not semantic TOML: %v\n%s", err, override)
	}
	servers, ok := decoded["mcp_servers"].(map[string]any)
	if !ok || len(servers) != 2 {
		t.Fatalf("decoded mcp_servers = %#v", decoded["mcp_servers"])
	}
	stdio, ok := servers["a stdio"].(map[string]any)
	if !ok || stdio["command"] != "/tmp/tool \"quoted\"" {
		t.Fatalf("decoded stdio = %#v", servers["a stdio"])
	}
	http, ok := servers["z http \"snow 雪\""].(map[string]any)
	if !ok || http["url"] != "https://example.invalid/a?b=\"c\"" {
		t.Fatalf("decoded HTTP = %#v", servers["z http \"snow 雪\""])
	}
	if launch.env["TOKEN"] != stdioSecret || launch.env["ALPHA"] != "first" || launch.env["BASE"] != "kept" {
		t.Fatalf("child env omitted stdio/inherited values: %#v", launch.env)
	}
	for header, secret := range spec.MCPServers[0].Headers {
		name := codexHTTPHeaderEnvName(spec.MCPServers[0].Name, header)
		if launch.env[name] != secret || strings.Contains(override, secret) {
			t.Fatalf("header %q was not hoisted safely: env=%q override=%s", header, launch.env[name], override)
		}
	}

	reordered := spec
	reordered.MCPServers = []agent.MCPServerConfig{spec.MCPServers[1], spec.MCPServers[0]}
	reordered.MCPServers[1].Headers = map[string]string{"X-Second": "second-secret", "X-\"Auth\"": httpSecret}
	again, err := buildInteractiveLaunch(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launch.argv, again.argv) || !reflect.DeepEqual(launch.env, again.env) {
		t.Fatalf("launch is not deterministic:\nfirst=%q %#v\nagain=%q %#v", launch.argv, launch.env, again.argv, again.env)
	}
}

func TestBuildInteractiveLaunch_EmptyMCPAddsNoOverride(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{Prompt: "hello", Env: map[string]string{"A": "1"}}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launch.argv, []string{"hello"}) || slices.Contains(launch.argv, "--strict-config") {
		t.Fatalf("empty MCP launch = %q", launch.argv)
	}
	launch.env["A"] = "changed"
	if spec.Env["A"] != "1" {
		t.Fatal("child env aliases input Env")
	}
}

func TestBuildInteractiveLaunch_EnvConflictsFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec agent.Spec
	}{
		{
			name: "inherited process conflict",
			spec: agent.Spec{Env: map[string]string{"TOKEN": "parent"}, MCPServers: []agent.MCPServerConfig{{Name: "one", Command: "server", Env: map[string]string{"TOKEN": "child"}}}},
		},
		{
			name: "server to server conflict",
			spec: agent.Spec{MCPServers: []agent.MCPServerConfig{
				{Name: "one", Command: "server", Env: map[string]string{"TOKEN": "one"}},
				{Name: "two", Command: "server", Env: map[string]string{"TOKEN": "two"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildInteractiveLaunch(tt.spec)
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed || adaptationErr.Channel != agent.ToolChannelMCPServer {
				t.Fatalf("error = %v, want typed MCP application failure", err)
			}
		})
	}
}

func TestBuildInteractiveLaunch_MalformedMCPFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		servers []agent.MCPServerConfig
	}{
		{"empty name", []agent.MCPServerConfig{{Command: "server"}}},
		{"missing stdio command", []agent.MCPServerConfig{{Name: "stdio"}}},
		{"missing HTTP URL", []agent.MCPServerConfig{{Name: "http", Type: "http"}}},
		{"unknown type", []agent.MCPServerConfig{{Name: "future", Type: "future"}}},
		{"duplicate name", []agent.MCPServerConfig{{Name: "same", Command: "one"}, {Name: " same ", Command: "two"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildInteractiveLaunch(agent.Spec{MCPServers: tt.servers})
			var adaptationErr *agent.ToolAdaptationError
			if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed || adaptationErr.Channel != agent.ToolChannelMCPServer {
				t.Fatalf("error = %v, want typed MCP application failure", err)
			}
		})
	}
}

func TestSpawnInteractive_EnvConflictPersistsDenialBeforeZeroPTYSideEffects(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "spawned")
	bin := writeFakeCodexScript(t, `touch "$PWD/spawned"`)
	var decisions []string
	_, err := SpawnInteractive(context.Background(), Options{CodexBin: bin}, agent.Spec{
		Cwd:         workdir,
		Env:         map[string]string{"TOKEN": "parent"},
		MCPServers:  []agent.MCPServerConfig{{Name: "one", Command: "server", Env: map[string]string{"TOKEN": "child"}}},
		Interactive: &agent.InteractiveSpec{},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			decisions = append(decisions, receipt.Decision)
			return nil
		},
	})
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialApplicationFailed {
		t.Fatalf("error = %v, want typed application failure", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("Codex PTY side effect occurred: %v", statErr)
	}
	if !slices.Equal(decisions, []string{"ready", "denied"}) {
		t.Fatalf("receipt decisions = %v, want ready then denied", decisions)
	}
}

func TestSpawnInteractive_MalformedMCPDeniedByUnifiedPreparationBeforeZeroPTYSideEffects(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	marker := filepath.Join(workdir, "spawned")
	bin := writeFakeCodexScript(t, `touch "$PWD/spawned"`)
	var receipts []agent.ToolLifecycleReceipt
	_, err := SpawnInteractive(context.Background(), Options{CodexBin: bin}, agent.Spec{
		Cwd:         workdir,
		MCPServers:  []agent.MCPServerConfig{{Name: "duplicate", Command: "one"}, {Name: " duplicate ", Command: "two"}},
		Interactive: &agent.InteractiveSpec{},
		OnToolLifecycleAdapted: func(receipt agent.ToolLifecycleReceipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
	})
	var adaptationErr *agent.ToolAdaptationError
	if !errors.As(err, &adaptationErr) || adaptationErr.Code != agent.ToolDenialMalformedPlan {
		t.Fatalf("error = %v, want typed malformed-plan denial", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("Codex PTY side effect occurred: %v", statErr)
	}
	if len(receipts) != 1 || receipts[0].Decision != "denied" || len(receipts[0].Entries) != 1 || receipts[0].Entries[0].DenialCode != agent.ToolDenialMalformedPlan {
		t.Fatalf("persisted receipts = %+v, want one malformed-plan receipt", receipts)
	}
}

func mcpOverrideFromArgs(t *testing.T, args []string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" && strings.HasPrefix(args[i+1], "mcp_servers=") {
			return args[i+1]
		}
	}
	t.Fatalf("argv omitted MCP override: %q", args)
	return ""
}

func cloneMCPServersForTest(in []agent.MCPServerConfig) []agent.MCPServerConfig {
	out := make([]agent.MCPServerConfig, len(in))
	for i, server := range in {
		out[i] = server
		out[i].Args = append([]string(nil), server.Args...)
		out[i].Env = cloneInteractiveEnv(server.Env)
		out[i].Headers = cloneInteractiveEnv(server.Headers)
	}
	return out
}

func TestResolveCodexBinary_MissingReturnsError(t *testing.T) {
	t.Parallel()
	_, err := resolveCodexBinary("this-binary-does-not-exist-codex-interactive-test")
	if err == nil {
		t.Fatal("expected an error")
	}
}

// writeFakeCodexScript materializes a fake-codex shell script under a real
// PTY (the PATH-shim technique used across this repo's harness tests, e.g.
// provider/harness/agycli's handle_test.go newFakeProvider).
func writeFakeCodexScript(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	return bin
}

// TestSpawnInteractive_RunsFakeCLIUnderPTY proves SpawnInteractive drives the
// fake binary directly under a PTY WITHOUT ever touching the app-server
// JSON-RPC machinery (no live Provider/handshake is constructed here at
// all — the whole point of SpawnInteractive being a package-level function).
func TestSpawnInteractive_RunsFakeCLIUnderPTY(t *testing.T) {
	t.Parallel()
	bin := writeFakeCodexScript(t, `echo "argv: $@"`)

	h, err := SpawnInteractive(context.Background(), Options{CodexBin: bin}, agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("SpawnInteractive: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}

	deadline := time.After(15 * time.Second)
	gotResult := false
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if _, ok := ev.(agent.ResultEvent); ok {
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for events")
		}
	}
}

func TestSpawnInteractive_MissingBinary_WrapsErrSpawnFailed(t *testing.T) {
	t.Parallel()
	_, err := SpawnInteractive(context.Background(), Options{CodexBin: "this-binary-does-not-exist-codex-interactive-test"}, agent.Spec{
		Interactive: &agent.InteractiveSpec{},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("error = %v, want wrapping agent.ErrSpawnFailed", err)
	}
}

// TestProvider_Spawn_Interactive_NeverTouchesAppServer proves
// Provider.Spawn routes Spec.Interactive to the PTY path even though the
// Provider was constructed against a fake app-server whose handshake never
// answers anything beyond "initialize" — if Spawn's interactive branch fell
// through to the headless thread/start path, this would hang and time out.
func TestProvider_Spawn_Interactive_NeverTouchesAppServer(t *testing.T) {
	t.Parallel()
	fs, stdinW, stdoutR := newFakeServer()
	go fs.run(t, "thread-interactive")

	bin := writeFakeCodexScript(t, `echo "tui up"; sleep 0.1`)

	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
		CodexBin:       bin,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}
}

func TestProvider_Spawn_PreparedInteractiveConsumesSoleAuthorityBeforePTY(t *testing.T) {
	t.Parallel()
	for _, mutate := range []bool{false, true} {
		mutate := mutate
		name := "ready"
		if mutate {
			name = "authority-mismatch"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fs, stdinW, stdoutR := newFakeServer()
			go fs.run(t, "thread-prepared-interactive")
			workdir := t.TempDir()
			marker := filepath.Join(workdir, "spawned")
			bin := writeFakeCodexScript(t, `touch "$PWD/spawned"; echo "tui up"; sleep 0.1`)
			p, err := New(Options{skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR, CodexBin: bin})
			if err != nil {
				fs.close()
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				_ = p.Shutdown(context.Background())
				fs.close()
			})

			source := agent.Spec{
				PromptMode:  agent.PromptModeHumanControlled,
				Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
				Model:       "gpt-test",
				PromptPlan: &agent.PromptPlan{
					ContractVersion:  agent.PromptContractVersion,
					BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
					UserPrompt:       agent.PromptContent{ID: "prepared-human", Text: "actual human seed", Required: true},
				},
			}
			const operationalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			var materializations []agent.HarnessMaterialization
			for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
				materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
			}
			prepared, err := agent.CompilePreparedHarness(source, p.Manifest(), operationalDigest, nil, materializations)
			if err != nil {
				t.Fatalf("CompilePreparedHarness: %v", err)
			}
			materialized := source
			materialized.Cwd = workdir
			materialized.PreparedHarness = prepared
			promptCallbacks, toolCallbacks := 0, 0
			materialized.OnPromptAdapted = func(agent.PromptDeliveryReceipt) error {
				promptCallbacks++
				return errors.New("second prompt authority")
			}
			materialized.OnToolLifecycleAdapted = func(agent.ToolLifecycleReceipt) error { toolCallbacks++; return errors.New("second tool authority") }
			if mutate {
				materialized.Model = "authority-mismatch"
			}

			h, err := p.Spawn(context.Background(), materialized)
			if mutate {
				if err == nil {
					_ = h.Stop(context.Background())
					t.Fatal("authority mismatch reached PTY spawn")
				}
				if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
					t.Fatalf("PTY marker exists after authority mismatch: %v", statErr)
				}
			} else {
				if err != nil {
					t.Fatalf("prepared interactive Spawn: %v", err)
				}
				t.Cleanup(func() { _ = h.Stop(context.Background()) })
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, statErr := os.Stat(marker); statErr == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("prepared interactive PTY marker was not created")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if promptCallbacks != 0 || toolCallbacks != 0 {
				t.Fatalf("provider minted a second authority: prompt=%d tool=%d", promptCallbacks, toolCallbacks)
			}
		})
	}
}
