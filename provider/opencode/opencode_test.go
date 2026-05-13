package opencode

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	if _, err := New(Options{
		Endpoint: srv.URL,
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil),
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
		Endpoint: srv.URL,
		Getenv:   fakeEnv(nil),
		LookPath: fakeLookPath(nil),
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
	if caps.SupportsMessageInjection {
		t.Error("SupportsMessageInjection: want false (inject not yet wired)")
	}
	if caps.SupportsSessionResume {
		t.Error("SupportsSessionResume: want false")
	}
	if caps.SupportsToolPlugins {
		t.Error("SupportsToolPlugins: want false (opencode manages its own tools)")
	}
	if caps.AcceptsAllowedToolsList {
		t.Error("AcceptsAllowedToolsList: want false")
	}
	if caps.AcceptsMcpServerSpec {
		t.Error("AcceptsMcpServerSpec: want false (opencode uses its own plugin system)")
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

// TestProvider_Spawn_HTTPMode_NotImplemented verifies that Spawn returns
// ErrSpawnFailed in HTTP-server mode (not yet wired).
func TestProvider_Spawn_HTTPMode_NotImplemented(t *testing.T) {
	t.Parallel()
	// HTTP-server mode: binary is empty.
	p := &Provider{endpoint: "http://localhost:7700", apiKey: ""}
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "anything"})
	if h != nil {
		t.Fatal("Spawn: want nil handle for HTTP-server mode (not yet wired)")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn err: want ErrSpawnFailed, got %v", err)
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

// TestProvider_Spawn_FakeCLI exercises the full Spawn → Handle →
// events pipeline using a fake `opencode` script that outputs
// OpenCode NDJSON events.
func TestProvider_Spawn_FakeCLI_NDJSON(t *testing.T) {
	t.Parallel()

	scriptPath := writeFakeOpenCodeScript(t)
	p := &Provider{binary: scriptPath}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	h, err := p.Spawn(ctx, agent.Spec{Prompt: "list files"})
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("Spawn: returned nil handle")
	}
	defer func() { _ = h.Stop(context.Background()) }()

	// Drain events until terminal ResultEvent + idle, or hard deadline.
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
		t.Error("events: want InitEvent (from step_start)")
	}
	if !gotAssistant {
		t.Error("events: want AssistantTextEvent (from text event)")
	}
	if !gotResult {
		t.Error("events: want ResultEvent (from step_finish reason=stop)")
	}
}

// collectUntilResult drains events from h until a terminal ResultEvent
// is seen and a 300 ms idle elapses, or until the hard deadline fires.
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
	if len(evs) != 1 {
		t.Fatalf("want 1 event (ResultEvent), got %d", len(evs))
	}
	r, ok := evs[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("want ResultEvent, got %T", evs[0])
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

func TestMapOpenCodeLine_StepFinish_ToolCalls_EmitsNothing(t *testing.T) {
	t.Parallel()
	line := []byte(`{"type":"step_finish","sessionID":"ses_x","part":{"type":"step-finish","reason":"tool-calls"}}`)
	evs := mapOpenCodeLine(line)
	if len(evs) != 0 {
		t.Errorf("want no events for tool-calls step_finish, got %d", len(evs))
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
	if !contains(argv, "--dangerously-skip-permissions") {
		t.Errorf("want --dangerously-skip-permissions in %v", argv)
	}
}

func TestBuildOpenCodeArgs_NonAutonomous(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Autonomous: false})
	if contains(argv, "--dangerously-skip-permissions") {
		t.Errorf("want no --dangerously-skip-permissions in %v", argv)
	}
}

func TestBuildOpenCodeArgs_Cwd(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Cwd: "/workspace"})
	if !contains(argv, "--dir") {
		t.Errorf("want --dir in %v", argv)
	}
	idx := indexOf(argv, "--dir")
	if idx < 0 || idx+1 >= len(argv) || argv[idx+1] != "/workspace" {
		t.Errorf("want --dir /workspace, got %v", argv)
	}
}

func TestBuildOpenCodeArgs_Model(t *testing.T) {
	t.Parallel()
	argv := buildOpenCodeArgs(agent.Spec{Model: "anthropic/claude-opus-4"})
	if !contains(argv, "--model") {
		t.Errorf("want --model in %v", argv)
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

func TestProvider_Resume_Unsupported(t *testing.T) {
	t.Parallel()
	p := mustNew(t)
	_, err := p.Resume(context.Background(), "session", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume err: want ErrUnsupported, got %v", err)
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

// writeFakeOpenCodeScript creates a temporary shell script that emits
// minimal OpenCode NDJSON events and returns its path.
func writeFakeOpenCodeScript(t *testing.T) string {
	t.Helper()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH — skipping fake-CLI test: %v", err)
	}

	const ndjsonFixture = `{"type":"step_start","sessionID":"ses_opencode_test_001","part":{"type":"step-start"}}
{"type":"text","sessionID":"ses_opencode_test_001","part":{"type":"text","text":"Hello from opencode fake"}}
{"type":"step_finish","sessionID":"ses_opencode_test_001","part":{"type":"step-finish","reason":"stop","tokens":{"total":50,"input":40,"output":10},"cost":0}}
`
	f, err := os.CreateTemp("", "fake-opencode-*.sh")
	if err != nil {
		t.Fatalf("create fake opencode script: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	script := "#!" + sh + "\ncat > /dev/null\nprintf '%s' " + shellQuote(ndjsonFixture)
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write fake opencode script: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fake opencode script: %v", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		t.Fatalf("chmod fake opencode script: %v", err)
	}
	return f.Name()
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
	cmd := exec.Command(scriptPath)
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
