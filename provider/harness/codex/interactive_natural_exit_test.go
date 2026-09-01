package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

// codexFakePTYClientCreatesThreadEnv switches the package test binary into a
// role standing in for the REAL codex CLI's own --remote PTY attach: it
// dials the --remote socket named in its own argv, sends the one RPC this
// suite's fake app-server (interactive_name_stderr_test.go) needs to
// broadcast a thread/started notification, then exits 0 — a normal,
// successful, unattended session end. Wired into TestMain
// (mcp_live_fixture_test.go).
const codexFakePTYClientCreatesThreadEnv = "DONMAI_CODEX_FAKE_PTY_CLIENT_CREATES_THREAD"

// runCodexFakePTYClientCreatesThread is the child-process entry point. Its
// caller (TestMain) always ends this process via os.Exit(code) — returning
// the code here rather than calling os.Exit inline keeps every deferred
// close reachable on every exit path.
func runCodexFakePTYClientCreatesThread() int {
	remote := ""
	for i, a := range os.Args {
		if a == "--remote" && i+1 < len(os.Args) {
			remote = strings.TrimPrefix(os.Args[i+1], "unix://")
		}
	}
	if remote == "" {
		return 4
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", remote)
		},
	}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, "ws://localhost", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return 5
	}
	defer func() { _ = conn.CloseNow() }()
	one := 1
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "thread/start", ID: &one})
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		return 6
	}
	// Wait for the response so the app-server has actually processed
	// thread/start (and therefore already broadcast thread/started) before
	// this process exits and drops the connection — a real codex CLI
	// wouldn't race its own request either.
	_, _, _ = conn.Read(ctx)
	return 0
}

// TestSpawnInteractive_NamedPlatformSessionCleansUpOnNaturalExit is the
// deliverable-2 happy-path proof this package's existing suite left
// uncovered: every other named-session test either drives an explicit
// h.Stop(ctx) (interactive_test.go, integration_test.go) or a deliberate
// crash/failure path (interactive_name_stderr_test.go). None exercise a
// FULLY SUCCESSFUL fresh named session — naming completes, the PTY exits 0
// on its own — and confirm BOTH the isolated config home AND the bootstrap
// app-server's socket directory are actually gone afterward, with nothing
// external ever calling Stop().
//
// Draining h.Events() to closure (rather than calling Stop) is the point:
// ptycli.Handle.run calls the cleanup closure (server.close + config.remove)
// itself once the PTY's Done() fires, entirely independent of whether any
// caller ever invokes Stop — this test proves that async path actually
// completes on its own, matching how a caller that only observes
// isess.Done() and returns (runner/interactive_loop.go's dispatchInteractive)
// behaves in production.
func TestSpawnInteractive_NamedPlatformSessionCleansUpOnNaturalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named interactive sessions are unix-only (validateNamedInteractiveTransport)")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	spec := agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		// Both fixture roles' env vars ride on the same session-scoped Env
		// map production would use for exactly one real credential/config
		// concern each child inherits identically — see argvHasFlag's doc
		// comment in mcp_live_fixture_test.go for how the two children
		// (bootstrap app-server, PTY) tell their own role apart despite
		// sharing this same map.
		Env: map[string]string{
			codexFakeNamedAppServerEnv:         "1",
			codexFakePTYClientCreatesThreadEnv: "1",
			// A single, unambiguous environment credential source — see
			// resolveInteractiveCodexAuth — routes construction through the
			// environment-auth path (interactiveAuthSeeder below), never
			// anywhere near this machine's real ~/.codex/auth.json.
			"OPENAI_API_KEY": "fixture-key",
		},
	}

	var remoteURL string
	h, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      self,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		// Fakes the environment-auth projection with a direct file write —
		// the real seeder execs `codex login`, which this suite's fake
		// binary does not implement.
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
		interactiveNameServerStarted: func(url string) { remoteURL = url },
	}, spec)
	if err != nil {
		t.Fatalf("SpawnInteractive: %v", err)
	}
	if remoteURL == "" {
		t.Fatal("bootstrap app-server never reported its remote URL")
	}
	socketDir := filepath.Dir(strings.TrimPrefix(remoteURL, "unix://"))
	if _, err := os.Stat(socketDir); err != nil {
		t.Fatalf("bootstrap app-server socket directory is not live right after spawn: %v", err)
	}

	deadline := time.After(15 * time.Second)
	gotResult := false
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if result, ok := ev.(agent.ResultEvent); ok {
				if !result.Success {
					t.Fatalf("fake codex interactive session failed: %+v", result)
				}
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}

	if _, err := os.Stat(socketDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap app-server socket directory survived natural exit with nobody calling Stop: err=%v", err)
	}
	remainingBoundaries, err := os.ReadDir(boundaryRoot)
	if err != nil || len(remainingBoundaries) != 0 {
		t.Fatalf("isolated config home survived natural exit with nobody calling Stop: err=%v entries=%v", err, remainingBoundaries)
	}
}
