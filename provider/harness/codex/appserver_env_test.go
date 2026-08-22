package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	// codexFakeAppServerEnv switches the package test binary into a minimal
	// `codex app-server` role (see TestMain in mcp_live_fixture_test.go).
	codexFakeAppServerEnv = "DONMAI_CODEX_FAKE_APP_SERVER"
	// codexFakeAppServerDumpEnv names the file the fake app-server writes its
	// OWN observed environment into, so a test can assert what the child
	// actually received rather than what the parent intended to send.
	codexFakeAppServerDumpEnv = "DONMAI_CODEX_FAKE_APP_SERVER_DUMP"

	// codexFakeAppServerThreadID is the thread id the fake hands back so a
	// Spawn can complete against it.
	codexFakeAppServerThreadID = "thread-env-overlay"
)

// codexEnvProbeKeys are the only variables the fake reports. Keeping the dump
// to a fixed, test-owned key set means no inherited host value — and no
// credential — is ever written to disk by the fixture.
var codexEnvProbeKeys = []string{"CODEX_HOME", "DONMAI_API_URL", "DONMAI_SESSION_ID"}

// runCodexFakeAppServer is the child-process entry point. It records the
// environment it was handed, then speaks just enough of the app-server
// JSON-RPC surface to carry a Spawn/Resume through thread start.
func runCodexFakeAppServer() {
	if dump := os.Getenv(codexFakeAppServerDumpEnv); dump != "" {
		observed := map[string][]string{}
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			for _, probe := range codexEnvProbeKeys {
				if key == probe {
					observed[key] = append(observed[key], value)
				}
			}
		}
		if body, err := json.Marshal(observed); err == nil {
			//nolint:gosec // G703: dump is a t.TempDir() path handed to this child
			// by the parent test process; the fixture never runs outside `go test`.
			_ = os.WriteFile(filepath.Clean(dump), body, 0o600)
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if len(request.ID) == 0 || string(request.ID) == "null" {
			continue // notification (`initialized`) — no response by contract
		}
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  codexFakeAppServerResult(request.Method),
		})
	}
}

func codexFakeAppServerResult(method string) any {
	switch method {
	case "initialize":
		// The provider refuses to proceed unless the app-server confirms the
		// isolated home it was pointed at, so echo the delivered CODEX_HOME.
		return map[string]any{"codexHome": os.Getenv("CODEX_HOME")}
	case "config/read":
		return map[string]any{
			"config":  map[string]any{codexMCPConfigKeyPath: map[string]any{}},
			"origins": map[string]any{},
		}
	case "mcpServerStatus/list":
		return map[string]any{"data": []any{}}
	case "thread/start":
		return map[string]any{"thread": map[string]any{"id": codexFakeAppServerThreadID}}
	default:
		return map[string]any{}
	}
}

// codexEnvProbe starts a provider whose app-server is this test binary in fake
// mode, with a malformed platform origin planted in the ambient parent
// environment. It returns the provider plus the path the child dumps its own
// environment to.
func codexEnvProbe(t *testing.T) (*Provider, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	dump := filepath.Join(t.TempDir(), "app-server-env.json")

	// The exact shape a live headless dispatch hit: an ambient DONMAI_API_URL
	// carrying a trailing delimiter, injected into the box before the runner ran.
	t.Setenv("DONMAI_API_URL", codexAmbientMalformedAPIURL)

	p, err := New(Options{
		CodexBin: self,
		Cwd:      t.TempDir(),
		Env: map[string]string{
			codexFakeAppServerEnv:     "1",
			codexFakeAppServerDumpEnv: dump,
		},
		HandshakeTimeout: 20 * time.Second,
		RPCTimeout:       20 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, dump
}

func readCodexEnvDump(t *testing.T, path string) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temp path
	if err != nil {
		t.Fatalf("read app-server env dump: %v", err)
	}
	var observed map[string][]string
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatalf("decode app-server env dump: %v", err)
	}
	return observed
}

const (
	// codexAmbientMalformedAPIURL is the shape observed in a live headless box:
	// a credential-snapshot-injected origin with a trailing delimiter. It must
	// never reach the agent.
	codexAmbientMalformedAPIURL = "https://agent.example.com;"
	// codexCanonicalAPIURL stands in for the runner-owned platform origin the
	// session's QueuedWork resolves (runner/loop.go buildSessionEnv).
	codexCanonicalAPIURL = "https://platform.example.com"
)

// TestSpawnOverlaysSessionEnvOntoAppServerChild pins the boundary: the headless
// app-server child must resolve the runner's per-session Spec.Env, not the
// ambient process environment the box happened to be provisioned with.
//
// RED proof: drop the sessionEnv layer from mergeEnv's ComposeChildEnv call
// (or stop threading spec.Env through ensureHeadlessReady/startLocked) and the
// child reports the ambient malformed origin instead of the canonical one.
func TestSpawnOverlaysSessionEnvOntoAppServerChild(t *testing.T) {
	p, dump := codexEnvProbe(t)

	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "overlay probe",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"DONMAI_API_URL":    codexCanonicalAPIURL,
			"DONMAI_SESSION_ID": "session-env-overlay",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	observed := readCodexEnvDump(t, dump)
	if got := observed["DONMAI_API_URL"]; !reflect.DeepEqual(got, []string{codexCanonicalAPIURL}) {
		t.Fatalf("app-server child DONMAI_API_URL = %v, want exactly [%s]", got, codexCanonicalAPIURL)
	}
	if got := observed["DONMAI_SESSION_ID"]; !reflect.DeepEqual(got, []string{"session-env-overlay"}) {
		t.Fatalf("app-server child DONMAI_SESSION_ID = %v, want the runner's session id", got)
	}
	// The owned config home still wins over every caller-supplied layer.
	if got := observed["CODEX_HOME"]; len(got) != 1 || !sameResolvedPath(got[0], p.config.home) {
		t.Fatalf("app-server child CODEX_HOME = %v, want the provider's isolated home", got)
	}
}

// TestResumeOverlaysSessionEnvOntoAppServerChild pins the same overlay on the
// resume entry point: a session that reattaches to an existing thread starts
// the app-server through the identical boundary and must not inherit the
// ambient origin either.
func TestResumeOverlaysSessionEnvOntoAppServerChild(t *testing.T) {
	p, dump := codexEnvProbe(t)

	h, err := p.Resume(t.Context(), "thread-existing", agent.Spec{
		Prompt: "overlay probe",
		Cwd:    t.TempDir(),
		Env:    map[string]string{"DONMAI_API_URL": codexCanonicalAPIURL},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	observed := readCodexEnvDump(t, dump)
	if got := observed["DONMAI_API_URL"]; !reflect.DeepEqual(got, []string{codexCanonicalAPIURL}) {
		t.Fatalf("resumed app-server child DONMAI_API_URL = %v, want exactly [%s]", got, codexCanonicalAPIURL)
	}
}
