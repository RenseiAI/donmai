package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

// This file pins the interactive named-session bootstrap half of the
// bounded-stderr-capture change: startNamedInteractiveAppServer (the process
// itself), namedInteractiveAppServer.close (the unexpected-exit case that
// feeds ptycli's cleanup-failure ResultEvent), and spawnNamedInteractivePTY's
// naming-failure path in interactive.go (the exact handle.Stop(ctx) call site
// item 2(b) names).

const (
	// codexFakeNamedAppServerEnv switches the package test binary into a
	// minimal `codex app-server --listen unix://<socket>` role: a websocket
	// JSON-RPC server on the socket named by --listen, speaking just enough
	// of the bootstrap handshake (initialize / initialized) to drive
	// startNamedInteractiveAppServer's fresh-session path.
	codexFakeNamedAppServerEnv = "DONMAI_CODEX_FAKE_NAMED_APP_SERVER"
	// codexFakeNamedAppServerStderrEnv carries base64-encoded bytes the fake
	// writes to its own stderr at its configured crash point.
	codexFakeNamedAppServerStderrEnv = "DONMAI_CODEX_FAKE_NAMED_APP_SERVER_STDERR"
	// codexFakeNamedAppServerCrashModeEnv selects when the fake dies:
	//   "before_listen" — writes stderr and os.Exit(1)s before ever binding
	//                     the --listen socket (simulates a crash so early
	//                     even the raw dial never succeeds).
	//   "after_init"    — completes initialize and reads the "initialized"
	//                     notification, THEN writes stderr and os.Exit(1)s —
	//                     simulating a crash (e.g. during MCP server
	//                     startup) after the bootstrap has already
	//                     succeeded and the session is considered live.
	codexFakeNamedAppServerCrashModeEnv = "DONMAI_CODEX_FAKE_NAMED_APP_SERVER_CRASH"

	// codexFakePTYClientEnv makes the package test binary exit 0 immediately
	// — the exact "PTY client exits 0" bug signature this change exists to
	// stop hiding: a downstream --remote client that only observes a
	// dropped connection looks like a clean, successful exit.
	codexFakePTYClientEnv = "DONMAI_CODEX_FAKE_PTY_CLIENT"

	// codexFakeNamedAppServerDiagnostic and codexFakeNamedAppServerSecret are
	// the fixed diagnostic line and secret every test in this file plants in
	// the fake's stderr and then asserts on: the diagnostic must survive
	// into the excerpt, and the secret must not.
	codexFakeNamedAppServerDiagnostic = `fatal: failed to start MCP server "fixture": exit status 1`
	codexFakeNamedAppServerSecret     = "sk-do-not-leak-this-interactive-secret"
)

// codexFakeNamedAppServerStderrEncoded base64-encodes the fixed diagnostic +
// bearer-token fixture every test in this file plants in the fake's stderr.
func codexFakeNamedAppServerStderrEncoded() string {
	raw := codexFakeNamedAppServerDiagnostic + "\nAuthorization: Bearer " + codexFakeNamedAppServerSecret + "\n"
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// runCodexFakeNamedAppServer is the child-process entry point wired into
// TestMain (mcp_live_fixture_test.go). It never returns for the
// "before_listen" crash mode or once serving begins — the caller always sees
// this process end via os.Exit, matching how a real process dies.
func runCodexFakeNamedAppServer() {
	writeStderr := func() {
		encoded := os.Getenv(codexFakeNamedAppServerStderrEnv)
		if encoded == "" {
			return
		}
		if raw, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			_, _ = os.Stderr.Write(raw)
		}
	}
	crashMode := os.Getenv(codexFakeNamedAppServerCrashModeEnv)
	if crashMode == "before_listen" {
		writeStderr()
		os.Exit(1)
	}

	socketPath := ""
	for i, a := range os.Args {
		if a == "--listen" && i+1 < len(os.Args) {
			socketPath = strings.TrimPrefix(os.Args[i+1], "unix://")
		}
	}
	if socketPath == "" {
		os.Exit(2)
	}
	_ = os.Remove(socketPath) //nolint:gosec // G703: socketPath is this fixture's own --listen arg, a test-owned temp path
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		os.Exit(3)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := context.Background()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var inbound rpcInbound
			if json.Unmarshal(raw, &inbound) != nil {
				continue
			}
			switch inbound.Method {
			case "initialize":
				result, _ := json.Marshal(map[string]any{"codexHome": os.Getenv("CODEX_HOME")})
				body, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: inbound.ID, Result: result})
				if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
					return
				}
			case "initialized":
				if crashMode == "after_init" {
					writeStderr()
					os.Exit(1)
				}
			}
		}
	})
	_ = (&http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}).Serve(listener)
}

// codexFakeNamedAppServerOpts is the fixed Options every test in this file
// drives startNamedInteractiveAppServer with: short timeouts so a stuck
// fixture fails the test instead of hanging the suite.
var codexFakeNamedAppServerOpts = Options{HandshakeTimeout: 2 * time.Second, RPCTimeout: 2 * time.Second}

func codexAssertStderrExcerpt(t *testing.T, err error, wantContains string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, wantContains) {
		t.Fatalf("error does not contain %q: %v", wantContains, err)
	}
	if !strings.Contains(msg, "app-server stderr:") && !strings.Contains(msg, "stderr:") {
		t.Fatalf("error does not carry a labeled stderr excerpt: %v", err)
	}
	if !strings.Contains(msg, codexFakeNamedAppServerDiagnostic) {
		t.Fatalf("error dropped the diagnostic line the excerpt exists to preserve: %v", err)
	}
	if strings.Contains(msg, codexFakeNamedAppServerSecret) {
		t.Fatalf("error leaked the raw bearer token: %v", err)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("error shows no redaction marker at all: %v", err)
	}
}

// TestStartNamedInteractiveAppServer_CrashBeforeListenSurfacesExcerpt pins
// item 2(b): when the bootstrap app-server dies before the --listen socket
// is even bound (the earliest possible crash point — before any dial can
// ever succeed), the returned error still carries a bounded, redacted
// excerpt of what it printed.
//
// RED proof: remove the withAppServerStderr(...) wrapping at this call's
// dial-failure return in interactive_name.go (replace with the bare `err`)
// and this test fails — the returned error names only the dial timeout,
// nothing from stderr. Verified by reverting that call site: FAILED
// ("error does not contain..."), then PASSED again after restoring — see
// the completion report for the exact quotes.
func TestStartNamedInteractiveAppServer_CrashBeforeListenSurfacesExcerpt(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	spec := agent.Spec{SessionName: "chief-of-staff", Cwd: t.TempDir()}
	childEnv := map[string]string{
		codexFakeNamedAppServerEnv:          "1",
		codexFakeNamedAppServerCrashModeEnv: "before_listen",
		codexFakeNamedAppServerStderrEnv:    codexFakeNamedAppServerStderrEncoded(),
	}

	_, spawnErr := startNamedInteractiveAppServer(
		t.Context(), self, codexFakeNamedAppServerOpts, spec, interactiveLaunch{}, childEnv, t.TempDir(),
	)
	codexAssertStderrExcerpt(t, spawnErr, codexFakeNamedAppServerDiagnostic)
}

// TestNamedInteractiveAppServer_CloseSurfacesExcerptOnUnexpectedExit pins the
// CORE fix, item 2(a): a bootstrap app-server that completes its handshake
// successfully (the session is considered live) and THEN dies on its own —
// before anything ever asked it to — makes close() return an
// excerpt-bearing error instead of nil. That error is exactly what
// ptycli.Handle.run's cleanup step turns into the session's terminal
// ResultEvent (ErrorSubtype "cleanup_failed"), which is how this reaches a
// session's failure evidence even though the downstream --remote PTY client
// itself would otherwise have exited 0 and reported nothing wrong.
//
// RED proof: in close(), delete the `if s.exitErr != nil { ... }` branch
// that calls unexpectedAppServerExitError and this test fails — close()
// returns nil for a process that crashed on its own. Verified by deleting
// that branch: FAILED ("close() returned nil..."), then PASSED again after
// restoring — see the completion report.
func TestNamedInteractiveAppServer_CloseSurfacesExcerptOnUnexpectedExit(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	spec := agent.Spec{SessionName: "chief-of-staff", Cwd: t.TempDir()}
	childEnv := map[string]string{
		codexFakeNamedAppServerEnv:          "1",
		codexFakeNamedAppServerCrashModeEnv: "after_init",
		codexFakeNamedAppServerStderrEnv:    codexFakeNamedAppServerStderrEncoded(),
	}

	server, startErr := startNamedInteractiveAppServer(
		t.Context(), self, codexFakeNamedAppServerOpts, spec, interactiveLaunch{}, childEnv, t.TempDir(),
	)
	if startErr != nil {
		t.Fatalf("startNamedInteractiveAppServer: %v", startErr)
	}

	// Wait for the fake's self-triggered crash rather than racing it with a
	// sleep: exitDone is closed exactly once cmd.Wait() returns.
	select {
	case <-server.exitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap app-server did not exit within 5s of completing its handshake")
	}

	closeErr := server.close()
	if closeErr == nil {
		t.Fatal("close() returned nil for a process that crashed on its own before close was ever called")
	}
	if !strings.Contains(closeErr.Error(), "unexpectedly") {
		t.Fatalf("close() error does not describe the exit as unexpected: %v", closeErr)
	}
	codexAssertStderrExcerpt(t, closeErr, codexFakeNamedAppServerDiagnostic)
}

// TestSpawnNamedInteractivePTY_NamingFailureAttachesStderrExcerpt pins item
// 2(b) at the EXACT call site the task names: interactive.go's
// spawnNamedInteractivePTY, the handle.Stop(ctx) branch that runs when
// finishNamingLiveInteractiveThread fails. server.client is left nil so that
// call fails immediately and deterministically ("connection is not open")
// instead of depending on a real websocket handshake; server.stderr is
// pre-populated directly, exercising exactly the formatting this change
// added around that Stop() call without needing a full fake app-server or a
// real PTY-attach protocol.
//
// bin is this test binary running in "fake PTY client" mode
// (codexFakePTYClientEnv): it exits 0 immediately, the documented bug
// signature ("the PTY client exits 0") this whole change exists to stop
// hiding — proving the excerpt reaches the caller even when the PTY side
// itself reports nothing wrong.
//
// RED proof: in spawnNamedInteractivePTY, drop the `if excerpt != ""`
// wrapping and return the bare wrapped error instead of the excerpt-bearing
// one. Verified directly: FAILED ("error does not contain..."), then PASSED
// again after restoring — see the completion report.
func TestSpawnNamedInteractivePTY_NamingFailureAttachesStderrExcerpt(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	raw := codexFakeNamedAppServerDiagnostic + "\nAuthorization: Bearer " + codexFakeNamedAppServerSecret + "\n"
	stderrBuf := newBoundedBuffer(appServerStderrRetentionBytes)
	if _, err := stderrBuf.Write([]byte(raw)); err != nil {
		t.Fatalf("seed stderr buffer: %v", err)
	}

	server := &namedInteractiveAppServer{
		remoteURL: "unix:///dev/null",
		cmd:       exec.Command("true"), // never started; close()'s escalation path is not exercised here.
		socketDir: t.TempDir(),
		stderr:    stderrBuf,
	}

	spec := agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         map[string]string{codexFakePTYClientEnv: "1"},
	}

	_, spawnErr := spawnNamedInteractivePTY(
		t.Context(), self, Options{HandshakeTimeout: 2 * time.Second}, spec, interactiveLaunch{}, server,
		func() error { return nil },
	)
	codexAssertStderrExcerpt(t, spawnErr, codexFakeNamedAppServerDiagnostic)
}
