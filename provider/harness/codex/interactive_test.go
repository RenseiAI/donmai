package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	workspace := t.TempDir()
	// Every launch carries the startup-trust seed (trust.go) and the approval
	// seed (approvals_seed.go) ahead of the prompt; these cases pin what the
	// PROMPT adds on top of them.
	seed := launchSeedPrefixFor(t, workspace)
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{name: "empty prompt starts bare TUI", prompt: "", want: seed},
		{name: "non-empty prompt seeds the TUI", prompt: "fix the failing tests", want: append(slices.Clone(seed), "fix the failing tests")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interactiveArgs(agent.Spec{Cwd: workspace, Prompt: tt.prompt})
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

func TestInteractiveArgs_NamedSessionResumesNativeThreadByName(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	seed := launchSeedPrefixFor(t, workspace)
	want := append([]string{"resume"}, seed...)
	want = append(want, "chief-of-staff", "coordinate")
	got := interactiveArgs(agent.Spec{
		Cwd: workspace, SessionName: "chief-of-staff", Prompt: "coordinate",
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interactiveArgs = %q; want %q", got, want)
	}
}

func TestRemoteInteractiveArgs_AttachesNamedResumeToPreparedServer(t *testing.T) {
	t.Parallel()
	got := remoteInteractiveArgs(
		[]string{"resume", "--config", `model="gpt-5.6-sol"`, "chief-of-staff", "coordinate"},
		"unix:///tmp/codex.sock",
	)
	want := []string{"resume", "--remote", "unix:///tmp/codex.sock", "--config", `model="gpt-5.6-sol"`, "chief-of-staff", "coordinate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remoteInteractiveArgs = %q; want %q", got, want)
	}
}

func TestAppServerConfigArgs_ProjectsOnlyConfigAuthority(t *testing.T) {
	t.Parallel()
	got := appServerConfigArgs([]string{
		"resume", "--config", `model="gpt-5.6-sol"`, "--add-dir", "/work/other",
		"--config", `mcp_servers={}`, "--strict-config", "chief-of-staff", "coordinate",
	})
	want := []string{"--config", `model="gpt-5.6-sol"`, "--config", `mcp_servers={}`, "--strict-config"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("appServerConfigArgs = %q; want %q", got, want)
	}
}

// TestBuildInteractiveLaunch_Model is the regression guard for the
// platform-model-selection defect: the interactive TUI launch never read
// Spec.Model at all (unlike the headless app-server lane's
// threadStartParams/turnStartParams, which always set "model" via
// resolveModel), so a work order dispatched with a platform-resolved model
// (QueuedWork.ResolvedProfile.Model → Spec.Model via translateSpec) launched
// the TUI under whatever model codex's own config.toml/CLI default
// resolved to. There is no local mechanism to validate a model id before
// spawn, so this mapping is a pure pass-through: a rejected id surfaces as
// codex's own nonzero exit (ptycli.buildResult → a failed
// agent.ResultEvent), not a silent fallback. Deliberately NOT resolveModel:
// that helper also defaults to DefaultCodexModel / CODEX_MODEL(_TIER) when
// Spec.Model is empty, which would always emit an override and mask the
// "no platform selection" case the required no-flag behavior below pins.
func TestBuildInteractiveLaunch_Model(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()

	t.Run("model selected emits a config override", func(t *testing.T) {
		t.Parallel()
		launch, err := buildInteractiveLaunch(agent.Spec{Cwd: workspace, Model: "gpt-5.6-sol"})
		if err != nil {
			t.Fatalf("buildInteractiveLaunch: %v", err)
		}
		got, ok := decodeModelOverride(t, launch.argv)
		if !ok {
			t.Fatalf("argv omitted a model override: %q", launch.argv)
		}
		if got != "gpt-5.6-sol" {
			t.Errorf("model override = %q, want %q", got, "gpt-5.6-sol")
		}
	})

	t.Run("no model selected emits no override", func(t *testing.T) {
		t.Parallel()
		launch, err := buildInteractiveLaunch(agent.Spec{Cwd: workspace})
		if err != nil {
			t.Fatalf("buildInteractiveLaunch: %v", err)
		}
		// The TUI must be left to its own config.toml/CLI default model, not
		// silently widened — no model override of any shape.
		if _, ok := decodeModelOverride(t, launch.argv); ok {
			t.Errorf("argv carries a model override with no platform-selected model: %q", launch.argv)
		}
	})
}

// decodeModelOverride parses the `model=…` value out of an argv slice as
// real TOML, mirroring decodeTrustOverride (trust_test.go) and
// mcpOverrideFromArgs above so the assertion is about the semantic value,
// not a string match on quoting.
func decodeModelOverride(t *testing.T, argv []string) (string, bool) {
	t.Helper()
	for i, arg := range argv {
		if !strings.HasPrefix(arg, "model=") {
			continue
		}
		if i == 0 || argv[i-1] != "--config" {
			t.Fatalf("model override is not introduced by --config: %q", argv)
		}
		var decoded struct {
			Model string `toml:"model"`
		}
		if err := toml.Unmarshal([]byte(arg), &decoded); err != nil {
			t.Fatalf("model override is not semantic TOML: %v\n%s", err, arg)
		}
		return decoded.Model, true
	}
	return "", false
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
	workspace := t.TempDir()
	spec := agent.Spec{Cwd: workspace, Prompt: "hello", Env: map[string]string{"A": "1"}}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := append(launchSeedPrefixFor(t, workspace), "hello")
	if !slices.Equal(launch.argv, want) || slices.Contains(launch.argv, "--strict-config") {
		t.Fatalf("empty MCP launch = %q, want %q", launch.argv, want)
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

// An on-platform interactive session must never inherit the operator's
// external MCP registration. The platform-minted bearer and
// per-session endpoint are one authority; ambient config may neither coexist
// with it nor redirect identical tool names to an external facade.
func TestSpawnInteractive_IsolatesPoisonedGlobalMCPConfigAndHeaders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	root := t.TempDir()
	ambientHome := filepath.Join(root, "ambient-codex-home")
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{ambientHome, boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	const ambientConfig = `[mcp_servers.external]
url = "https://external.example.com/mcp"
[mcp_servers.external.env_http_headers]
Authorization = "POISON_EXTERNAL_AUTH"
X-Org = "POISON_EXTERNAL_ORG"
X-Project = "POISON_EXTERNAL_PROJECT"
`
	ambientConfigPath := filepath.Join(ambientHome, "config.toml")
	if err := os.WriteFile(ambientConfigPath, []byte(ambientConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	hostAuth := filepath.Join(ambientHome, codexAuthFileName)
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"host-login"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", ambientHome)
	t.Setenv("POISON_EXTERNAL_AUTH", "Bearer external-registration")
	t.Setenv("POISON_EXTERNAL_ORG", "org_wrong")
	t.Setenv("POISON_EXTERNAL_PROJECT", "proj_wrong")

	bin := writeFakeCodexScript(t, `
set -e
printf '%s' "$CODEX_HOME" > "$PWD/observed-home"
cp "$CODEX_HOME/config.toml" "$PWD/observed-config.toml"
printf '%s\n' "$@" > "$PWD/observed-argv"
for key in OPENAI_API_KEY CODEX_API_KEY CODEX_ACCESS_TOKEN; do printf '%s=%s\n' "$key" "${!key}"; done > "$PWD/observed-auth-env"
test -f "$CODEX_HOME/auth.json"
`)
	const sessionBearer = "session-mcp-bearer"
	h, err := SpawnInteractive(context.Background(), Options{
		CodexBin:      bin,
		configTempDir: boundaryRoot,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, []agent.MCPServerConfig{{
			Name: "donmai-platform",
			Type: "http",
			URL:  "https://platform.example.com/api/mcp/sess_project",
			Headers: map[string]string{
				"Authorization": "Bearer " + sessionBearer,
			},
		}}),
	}, agent.Spec{
		Cwd: workdir,
		Env: map[string]string{
			// A work-item or credential layer cannot override the runner-owned
			// boundary either.
			"CODEX_HOME": ambientHome,
		},
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform",
			Type: "http",
			URL:  "https://platform.example.com/api/mcp/sess_project",
			Headers: map[string]string{
				"Authorization": "Bearer " + sessionBearer,
			},
		}},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("SpawnInteractive: %v", err)
	}
	for ev := range h.Events() {
		if result, ok := ev.(agent.ResultEvent); ok && !result.Success {
			t.Fatalf("fake codex failed: %+v", result)
		}
	}

	observedHomeBytes, err := os.ReadFile(filepath.Join(workdir, "observed-home"))
	if err != nil {
		t.Fatal(err)
	}
	observedHome := string(observedHomeBytes)
	if observedHome == ambientHome || !sameResolvedPath(filepath.Dir(observedHome), boundaryRoot) {
		t.Fatalf("child CODEX_HOME = %q, want private boundary under %q", observedHome, boundaryRoot)
	}
	remainingBoundaries, err := os.ReadDir(boundaryRoot)
	if err != nil || len(remainingBoundaries) != 0 {
		t.Fatalf("private CODEX_HOME survived child exit: err=%v entries=%v", err, remainingBoundaries)
	}
	seenConfig, err := os.ReadFile(filepath.Join(workdir, "observed-config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(seenConfig), "external.example.com") || strings.Contains(string(seenConfig), "POISON_EXTERNAL") {
		t.Fatalf("ambient MCP authority entered private config:\n%s", seenConfig)
	}
	if !strings.Contains(string(seenConfig), codexConfigBaseline) || !strings.Contains(string(seenConfig), codexFileAuthConfig) {
		t.Fatalf("private config omitted its empty MCP/file-auth baseline:\n%s", seenConfig)
	}
	argv, err := os.ReadFile(filepath.Join(workdir, "observed-argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "/api/mcp/sess_project") || strings.Contains(string(argv), "external.example.com") || strings.Contains(string(argv), sessionBearer) {
		t.Fatalf("session MCP argv is not endpoint-exact and secret-free:\n%s", argv)
	}
	authEnv, err := os.ReadFile(filepath.Join(workdir, "observed-auth-env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range codexEnvironmentAuthKeys {
		if !strings.Contains(string(authEnv), key+"=\n") {
			t.Fatalf("child retained %s authority: %s", key, authEnv)
		}
	}
	unchanged, err := os.ReadFile(ambientConfigPath)
	if err != nil || string(unchanged) != ambientConfig {
		t.Fatalf("ambient config changed: err=%v body=%q", err, unchanged)
	}
}

func inventoryRunnerFor(t *testing.T, servers []agent.MCPServerConfig) interactiveMCPInventoryRunner {
	t.Helper()
	return func(_ context.Context, _ string, _ string, childEnv []string, configArgs, queryArgs []string) ([]byte, error) {
		for _, key := range codexEnvironmentAuthKeys {
			want := key + "="
			found := false
			for _, entry := range childEnv {
				if entry == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("effective-config child env did not clear %s", key)
			}
		}
		if !slices.ContainsFunc(configArgs, func(arg string) bool {
			return strings.Contains(arg, "trust_level=\"untrusted\"")
		}) {
			t.Fatalf("effective-config readback did not mark the project untrusted: %q", configArgs)
		}
		entries := make([]codexMCPInventoryEntry, 0, len(servers))
		for _, server := range servers {
			entry := codexMCPInventoryEntry{Name: server.Name, Enabled: true}
			switch strings.ToLower(strings.TrimSpace(server.Type)) {
			case "http":
				entry.Transport.Type = "streamable_http"
				entry.Transport.URL = server.URL
				entry.Transport.EnvHTTPHeaders = make(map[string]string, len(server.Headers))
				for header := range server.Headers {
					entry.Transport.EnvHTTPHeaders[header] = codexHTTPHeaderEnvName(server.Name, header)
				}
			default:
				entry.Transport.Type = "stdio"
				entry.Transport.Command = server.Command
				entry.Transport.Args = append([]string(nil), server.Args...)
				entry.Transport.EnvVars = sortedStringKeys(server.Env)
			}
			entries = append(entries, entry)
		}
		if slices.Contains(queryArgs, "list") {
			return json.Marshal(entries)
		}
		if len(queryArgs) >= 3 && queryArgs[1] == "get" {
			for _, entry := range entries {
				if entry.Name == queryArgs[2] {
					return json.Marshal(entry)
				}
			}
		}
		return nil, fmt.Errorf("unexpected fake inventory query: %q", queryArgs)
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
