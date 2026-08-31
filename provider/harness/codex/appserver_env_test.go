package codex

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
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

	// codexFakeAppServerStderrEnv carries base64-encoded bytes the fake
	// writes to its own stderr before doing anything else — a fixture for
	// the bounded stderr capture (appserver_stderr.go). base64 avoids
	// fighting exec.Cmd.Env over embedded newlines/binary content.
	codexFakeAppServerStderrEnv = "DONMAI_CODEX_FAKE_APP_SERVER_STDERR"
	// codexFakeAppServerCrashEnv, when "1", makes the fake os.Exit(1)
	// immediately after writing its stderr — before ever reading a
	// request — simulating an app-server that dies during its own startup
	// (e.g. while starting an MCP server) instead of completing the
	// initialize handshake.
	codexFakeAppServerCrashEnv = "DONMAI_CODEX_FAKE_APP_SERVER_CRASH"
)

// codexEnvProbeKeys are the only variables the fake reports. Keeping the dump
// to a fixed, test-owned key set means no inherited host value — and no
// credential — is ever written to disk by the fixture.
var codexEnvProbeKeys = []string{"CODEX_HOME", "DONMAI_API_URL", "DONMAI_SESSION_ID"}

// runCodexFakeAppServer is the child-process entry point. It records the
// environment it was handed, then speaks just enough of the app-server
// JSON-RPC surface to carry a Spawn/Resume through thread start.
func runCodexFakeAppServer() {
	if encoded := os.Getenv(codexFakeAppServerStderrEnv); encoded != "" {
		if raw, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			_, _ = os.Stderr.Write(raw)
		}
	}
	if os.Getenv(codexFakeAppServerCrashEnv) == "1" {
		os.Exit(1)
	}
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

// codexSecondSessionEnv is the layer a DIFFERENT session would compose in the
// same box: a distinct session id, a distinct platform origin, and a bearer.
// The bearer is present so the fail-closed test can prove the rejection names
// keys without quoting any value it was handed.
var codexSecondSessionEnv = map[string]string{
	"DONMAI_API_URL":    "https://other-platform.example.com",
	"DONMAI_SESSION_ID": "session-number-two",
	"WORKER_AUTH_TOKEN": "rsk_second_session_bearer_do_not_echo",
}

// codexFirstSessionEnv is the layer the started app-server child is pinned to
// by the Spawn in codexStartedProvider.
var codexFirstSessionEnv = map[string]string{
	"DONMAI_API_URL":    codexCanonicalAPIURL,
	"DONMAI_SESSION_ID": "session-env-overlay",
}

// codexStartedProvider brings the deferred app-server up through a real
// headless Spawn, so the returned Provider is pinned to codexFirstSessionEnv
// exactly the way production pins it.
func codexStartedProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	p, dump := codexEnvProbe(t)
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "first session",
		Cwd:    t.TempDir(),
		Env:    maps.Clone(codexFirstSessionEnv),
	})
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	return p, dump
}

// assertSessionEnvConflict pins the shape of the refusal: it is a spawn
// failure, it is the session-env conflict specifically, it names the diverging
// keys, and it quotes NO value from either layer.
func assertSessionEnvConflict(t *testing.T, err error, wantKeys []string) {
	t.Helper()
	if err == nil {
		t.Fatal("divergent session env was accepted, want a fail-closed error")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("error %v does not wrap agent.ErrSpawnFailed", err)
	}
	if !errors.Is(err, errSessionEnvConflict) {
		t.Fatalf("error %v does not wrap errSessionEnvConflict", err)
	}
	message := err.Error()
	for _, key := range wantKeys {
		if !strings.Contains(message, key) {
			t.Fatalf("error %q does not name the diverging key %q", message, key)
		}
	}
	for _, layer := range []map[string]string{codexFirstSessionEnv, codexSecondSessionEnv} {
		for key, value := range layer {
			if strings.Contains(message, value) {
				t.Fatalf("error leaked the value of %s: %q", key, message)
			}
		}
	}
}

// TestSpawnRefusesDivergentSessionEnvOnStartedAppServer pins the one-session
// invariant the overlay depends on. mergeEnv's session layer is applied exactly
// once, at process start, so a second session served by the same child would
// silently run against the FIRST session's DONMAI_SESSION_ID and
// DONMAI_API_URL. That must fail closed, not succeed with the wrong routing.
//
// RED proof: drop the checkSessionEnvLocked call from ensureHeadlessReady and
// the second Spawn succeeds while the child's dump still reports session one's
// values — exactly the silent misroute this refuses.
func TestSpawnRefusesDivergentSessionEnvOnStartedAppServer(t *testing.T) {
	p, dump := codexStartedProvider(t)

	_, err := p.Spawn(t.Context(), agent.Spec{
		Prompt: "second session",
		Cwd:    t.TempDir(),
		Env:    maps.Clone(codexSecondSessionEnv),
	})
	assertSessionEnvConflict(t, err, []string{"DONMAI_API_URL", "DONMAI_SESSION_ID", "WORKER_AUTH_TOKEN"})

	// The refusal is what keeps the claim honest: the child is still the first
	// session's child, and nothing about the second session reached it.
	observed := readCodexEnvDump(t, dump)
	if got := observed["DONMAI_SESSION_ID"]; !reflect.DeepEqual(got, []string{codexFirstSessionEnv["DONMAI_SESSION_ID"]}) {
		t.Fatalf("app-server child DONMAI_SESSION_ID = %v, want the first session's id", got)
	}
}

// TestResumeRefusesDivergentSessionEnvOnStartedAppServer pins the same refusal
// on the resume entry point, which shares ensureHeadlessReady with Spawn.
func TestResumeRefusesDivergentSessionEnvOnStartedAppServer(t *testing.T) {
	p, _ := codexStartedProvider(t)

	_, err := p.Resume(t.Context(), codexFakeAppServerThreadID, agent.Spec{
		Prompt: "second session resume",
		Cwd:    t.TempDir(),
		Env:    maps.Clone(codexSecondSessionEnv),
	})
	assertSessionEnvConflict(t, err, []string{"DONMAI_API_URL", "DONMAI_SESSION_ID", "WORKER_AUTH_TOKEN"})
}

// TestResumeAcceptsIdenticalSessionEnv is the other half of the invariant: the
// check must not break the case it exists to protect. A session reattaching to
// its own thread recomposes an identical Spec.Env from the same QueuedWork, and
// that layer IS what the child carries, so it is served.
func TestResumeAcceptsIdenticalSessionEnv(t *testing.T) {
	p, _ := codexStartedProvider(t)

	h, err := p.Resume(t.Context(), codexFakeAppServerThreadID, agent.Spec{
		Prompt: "same session resume",
		Cwd:    t.TempDir(),
		Env:    maps.Clone(codexFirstSessionEnv),
	})
	if err != nil {
		t.Fatalf("Resume with the pinned session env: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
}

// TestInteractiveSpawnIsOutsideTheSessionEnvInvariant pins the scope boundary.
// The interactive spawn mode runs its own codex process under a PTY and never
// touches the shared app-server, so the one-child invariant does not apply to
// it and must not reject it. The spec below fails at the interactive path's own
// first gate (duplicate MCP server names) — which is the point: it got there,
// rather than being turned away by the headless env check.
func TestInteractiveSpawnIsOutsideTheSessionEnvInvariant(t *testing.T) {
	p, _ := codexStartedProvider(t)
	if !p.Manifest().Caps.SupportsInteractivePTY {
		t.Skip("codex no longer declares PTY transport; the interactive branch is unreachable")
	}

	_, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      "interactive session",
		Cwd:         t.TempDir(),
		Env:         maps.Clone(codexSecondSessionEnv),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		MCPServers: []agent.MCPServerConfig{
			{Name: "dupe", Type: "stdio", Command: "/bin/true"},
			{Name: "dupe", Type: "stdio", Command: "/bin/true"},
		},
	})
	if err == nil {
		t.Fatal("duplicate MCP server names should have failed the interactive spawn")
	}
	if errors.Is(err, errSessionEnvConflict) {
		t.Fatalf("interactive spawn was rejected by the headless session-env invariant: %v", err)
	}
}

// TestDivergentSessionEnvKeys covers the comparison itself, including the two
// shapes the integration tests cannot reach cheaply: a key present in only one
// layer, and the nil/empty equivalence that keeps a Provider started without a
// session layer from conflicting with a spec carrying an empty map.
func TestDivergentSessionEnvKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		pinned, want  map[string]string
		wantDivergent []string
	}{
		{name: "both nil", pinned: nil, want: nil},
		{name: "nil and empty are the same layer", pinned: nil, want: map[string]string{}},
		{name: "empty and nil are the same layer", pinned: map[string]string{}, want: nil},
		{
			name:   "identical layers",
			pinned: map[string]string{"DONMAI_API_URL": "https://platform.example.com", "DONMAI_SESSION_ID": "s1"},
			want:   map[string]string{"DONMAI_API_URL": "https://platform.example.com", "DONMAI_SESSION_ID": "s1"},
		},
		{
			name:          "changed value",
			pinned:        map[string]string{"DONMAI_API_URL": "https://platform.example.com"},
			want:          map[string]string{"DONMAI_API_URL": "https://other-platform.example.com"},
			wantDivergent: []string{"DONMAI_API_URL"},
		},
		{
			name:          "key added by the later session",
			pinned:        map[string]string{"DONMAI_SESSION_ID": "s1"},
			want:          map[string]string{"DONMAI_SESSION_ID": "s1", "GH_TOKEN": "gh_second"},
			wantDivergent: []string{"GH_TOKEN"},
		},
		{
			name:          "key dropped by the later session",
			pinned:        map[string]string{"DONMAI_SESSION_ID": "s1", "GH_TOKEN": "gh_first"},
			want:          map[string]string{"DONMAI_SESSION_ID": "s1"},
			wantDivergent: []string{"GH_TOKEN"},
		},
		{
			name:          "reported sorted, added and changed together",
			pinned:        map[string]string{"DONMAI_SESSION_ID": "s1", "ZED": "z"},
			want:          map[string]string{"DONMAI_SESSION_ID": "s2", "ALPHA": "a", "ZED": "z"},
			wantDivergent: []string{"ALPHA", "DONMAI_SESSION_ID"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := divergentSessionEnvKeys(tc.pinned, tc.want)
			if len(got) == 0 && len(tc.wantDivergent) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.wantDivergent) {
				t.Fatalf("divergentSessionEnvKeys = %v, want %v", got, tc.wantDivergent)
			}
		})
	}
}
