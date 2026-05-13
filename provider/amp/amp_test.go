package amp

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
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

func TestNew_MissingKey_ReturnsProviderUnavailable(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(map[string]string{"amp": "/usr/local/bin/amp"}),
	})
	if err == nil {
		t.Fatal("expected error when AMP_API_KEY is unset")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), EnvAPIKey) {
		t.Fatalf("err: want %s in message, got %v", EnvAPIKey, err)
	}
}

func TestNew_BinaryNotFound_ReturnsProviderUnavailable(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		APIKey:   "test-key",
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil), // amp not on PATH
	})
	if err == nil {
		t.Fatal("expected error when amp binary not found")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("err: want ErrProviderUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "amp") {
		t.Fatalf("err: want binary name in message, got %v", err)
	}
}

func TestNew_OptionsKeyWins(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		APIKey:   "explicit-key",
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/amp"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "explicit-key" {
		t.Fatalf("apiKey: want %q, got %q", "explicit-key", p.apiKey)
	}
}

func TestNew_FallsBackToEnv(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		Getenv:   fakeEnv(map[string]string{EnvAPIKey: "env-key"}),
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/amp"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.apiKey != "env-key" {
		t.Fatalf("apiKey: want %q, got %q", "env-key", p.apiKey)
	}
}

func TestNew_CustomBinary(t *testing.T) {
	t.Parallel()
	var probed string
	p, err := New(Options{
		Binary: "amp-custom",
		APIKey: "key",
		Getenv: fakeEnv(nil),
		LookPath: fakeLookPath(map[string]string{
			"amp-custom": "/opt/bin/amp-custom",
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = probed
	if p.binary != "/opt/bin/amp-custom" {
		t.Errorf("binary: want resolved path, got %q", p.binary)
	}
}

func TestNew_DefaultBinary_ProbesDefaultName(t *testing.T) {
	t.Parallel()
	var probed string
	_, _ = New(Options{
		APIKey: "key",
		Getenv: fakeEnv(nil),
		LookPath: func(name string) (string, error) {
			probed = name
			return "/x", nil
		},
	})
	if probed != DefaultBinary {
		t.Errorf("LookPath probed %q, want %q", probed, DefaultBinary)
	}
}

func TestProvider_Name(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	if got := p.Name(); got != agent.ProviderAmp {
		t.Fatalf("Name: want %q, got %q", agent.ProviderAmp, got)
	}
}

func TestProvider_Capabilities(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	caps := p.Capabilities()

	// These remain false in v1.0.0.
	if caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want false (between-turn inject not yet wired)")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume: want false")
	}
	if caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want false (amp manages tools via settings.json)")
	}
	if caps.EmitsSubagentEvents {
		t.Error("EmitsSubagentEvents: want false")
	}
	if caps.SupportsReasoningEffort {
		t.Error("SupportsReasoningEffort: want false (amp uses --mode, not --effort)")
	}
	if caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want false (amp does not expose --allowedTools)")
	}

	// AcceptsMcpServerSpec is true: amp --mcp-config uses same JSON format.
	if !caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want true (amp --mcp-config accepts same JSON format)")
	}
	if caps.HumanLabel != "Amp" {
		t.Errorf("HumanLabel: want %q, got %q", "Amp", caps.HumanLabel)
	}
}

// TestProvider_Spawn_BinaryNotFound verifies that Spawn returns
// ErrSpawnFailed when the binary cannot be exec'd (e.g. fake path).
func TestProvider_Spawn_BinaryNotFound(t *testing.T) {
	t.Parallel()
	// Construct a provider pointing at a non-existent binary.
	p := &Provider{binary: "/nonexistent/amp-fake-binary", apiKey: "key"}
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

// TestProvider_Spawn_FakeCLI_StreamJSON exercises the full Spawn →
// Handle → events pipeline using a fake `amp` script that outputs
// Claude Code-compatible stream-json JSONL. This validates that the
// claude JSONL mapper is correctly wired for amp.
func TestProvider_Spawn_FakeCLI_StreamJSON(t *testing.T) {
	t.Parallel()

	// Write a fake amp script that emits a minimal stream-json session.
	scriptPath := writeFakeAmpScript(t)

	p := &Provider{binary: scriptPath, apiKey: "test-key"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "say hello"})
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("Spawn: returned nil handle")
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// Drain events with a terminal-idle deadline: after observing a
	// ResultEvent, wait 200 ms for any stragglers then stop. The
	// claude Handle keeps the events channel open after the subprocess
	// exits (to allow Inject), so we cannot range until close here.
	events := collectUntilResult(t, h, 5*time.Second)

	var gotInit, gotAssistant, gotResult bool
	for _, ev := range events {
		switch ev.(type) {
		case agent.InitEvent:
			gotInit = true
		case agent.AssistantTextEvent:
			gotAssistant = true
		case agent.ResultEvent:
			gotResult = true
		}
	}
	if !gotInit {
		t.Error("events: want InitEvent")
	}
	if !gotAssistant {
		t.Error("events: want AssistantTextEvent")
	}
	if !gotResult {
		t.Error("events: want ResultEvent")
	}
}

// collectUntilResult drains events from h until a terminal ResultEvent
// is seen and an idle period elapses, or until the hard deadline fires.
func collectUntilResult(t *testing.T, h agent.Handle, hardDeadline time.Duration) []agent.Event {
	t.Helper()
	var got []agent.Event
	timer := time.NewTimer(hardDeadline)
	defer timer.Stop()
	for {
		var idleCh <-chan time.Time
		for _, ev := range got {
			if _, ok := ev.(agent.ResultEvent); ok {
				idle := time.NewTimer(300 * time.Millisecond)
				defer idle.Stop()
				idleCh = idle.C
				break
			}
		}
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-idleCh:
			return got
		case <-timer.C:
			t.Logf("collectUntilResult: hard deadline after %d events", len(got))
			return got
		}
	}
}

// TestBuildAmpArgs_Autonomous verifies that --dangerously-allow-all is
// included when Autonomous is true.
func TestBuildAmpArgs_Autonomous(t *testing.T) {
	t.Parallel()
	argv := buildAmpArgs(agent.Spec{Autonomous: true}, "")
	if !contains(argv, "--dangerously-allow-all") {
		t.Errorf("want --dangerously-allow-all in %v", argv)
	}
}

// TestBuildAmpArgs_NonAutonomous verifies that --dangerously-allow-all
// is absent when Autonomous is false.
func TestBuildAmpArgs_NonAutonomous(t *testing.T) {
	t.Parallel()
	argv := buildAmpArgs(agent.Spec{Autonomous: false}, "")
	if contains(argv, "--dangerously-allow-all") {
		t.Errorf("want no --dangerously-allow-all in %v", argv)
	}
}

// TestBuildAmpArgs_MCPConfig verifies that --mcp-config is appended
// when an MCP config path is provided.
func TestBuildAmpArgs_MCPConfig(t *testing.T) {
	t.Parallel()
	argv := buildAmpArgs(agent.Spec{}, "/tmp/mcp.json")
	if !contains(argv, "--mcp-config") {
		t.Errorf("want --mcp-config in %v", argv)
	}
	idx := indexOf(argv, "--mcp-config")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "/tmp/mcp.json" {
		t.Errorf("want --mcp-config /tmp/mcp.json, got %v", argv)
	}
}

// TestBuildAmpArgs_Model verifies --model flag.
func TestBuildAmpArgs_Model(t *testing.T) {
	t.Parallel()
	argv := buildAmpArgs(agent.Spec{Model: "claude-opus-4"}, "")
	if !contains(argv, "--model") {
		t.Errorf("want --model in %v", argv)
	}
	idx := indexOf(argv, "--model")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "claude-opus-4" {
		t.Errorf("want --model claude-opus-4, got %v", argv)
	}
}

// TestBuildAmpArgs_AlwaysContainsCoreBits verifies the invariant that
// every Spawn always includes -x, --stream-json, --no-ide, --no-notifications.
func TestBuildAmpArgs_AlwaysContainsCoreBits(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-x", "--stream-json", "--no-ide", "--no-notifications"} {
		argv := buildAmpArgs(agent.Spec{}, "")
		if !contains(argv, flag) {
			t.Errorf("want %q in base argv %v", flag, argv)
		}
	}
}

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	_, err := p.Resume(context.Background(), "amp-session-1", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err: want wrapping ErrUnsupported, got %v", err)
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
		APIKey:   "test-key",
		LookPath: fakeLookPath(map[string]string{DefaultBinary: "/usr/local/bin/amp"}),
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

// writeFakeAmpScript creates a temporary shell script that emits a
// minimal Claude Code-compatible stream-json JSONL session to stdout
// and returns its path. The script ignores all arguments.
func writeFakeAmpScript(t *testing.T) string {
	t.Helper()

	// Verify that a shell is available.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH — skipping fake-CLI test: %v", err)
	}

	const jsonlFixture = `{"type":"system","subtype":"init","session_id":"amp-session-test-001","cwd":"/tmp","tools":[]}
{"type":"assistant","session_id":"amp-session-test-001","message":{"role":"assistant","content":[{"type":"text","text":"Hello from amp fake"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Hello from amp fake","session_id":"amp-session-test-001","total_cost_usd":0,"num_turns":1}
`
	f, err := os.CreateTemp("", "fake-amp-*.sh")
	if err != nil {
		t.Fatalf("create fake amp script: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	// Write a script that reads stdin (consumes the prompt) then emits
	// the JSONL fixture.
	script := "#!" + sh + "\ncat > /dev/null\nprintf '%s' " + shellQuote(jsonlFixture)
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write fake amp script: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fake amp script: %v", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		t.Fatalf("chmod fake amp script: %v", err)
	}
	return f.Name()
}

// shellQuote single-quotes s for safe use in a shell script.
func shellQuote(s string) string {
	// Replace each single-quote with '\'' (end quote, literal quote, reopen quote).
	replaced := strings.ReplaceAll(s, "'", `'\''`)
	return "'" + replaced + "'"
}

// TestFakeAmpScript_ProducesExpectedLines is a sanity check that the
// fake amp script emits the expected JSONL lines. This catches issues
// with the script template itself, not the provider.
func TestFakeAmpScript_ProducesExpectedLines(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeAmpScript(t)
	cmd := exec.Command(scriptPath)
	// Provide a dummy stdin so the script's cat can drain it.
	cmd.Stdin = strings.NewReader("test prompt")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake amp script exited non-zero: %v\nstdout: %s", err, out)
	}
	lines := 0
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		if scanner.Text() != "" {
			lines++
		}
	}
	if lines < 3 {
		t.Errorf("expected at least 3 JSONL lines from fake script, got %d", lines)
	}
}
