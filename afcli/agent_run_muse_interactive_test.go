package afcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestAgentRunRegistry_MuseInteractivePTY_StartInputResizeTeardown proves
// the agent-run wiring for the interactive terminal mode end to end: the
// production single-source ctor list carries the muse entry, the
// constructed provider's live manifest declares the interactive PTY
// spawn mode, and a session started through the production registry +
// production Spawn path starts (InitEvent), takes input (bytes written
// via WriteInput reach the child), honors resize (Snapshot reflects the
// new geometry), and tears down (terminal ResultEvent, stream closes).
//
// The binary is a fake muse script (answers the `--version` construction
// probe and the `exec` login probe, then runs a stdin-recording TUI
// stub), so the test is platform-free: no network, no cost, no operator
// login. RED proofs (each verified by mutating the production change and
// re-running): dropping the muse entry from agentRunProviderCtors fails
// the resolve below, and flipping the manifest's SupportsInteractivePTY
// back to false routes Spawn into the headless serve handshake, which
// the fake binary cannot answer, so Spawn fails instead of returning an
// interactive handle.
func TestAgentRunRegistry_MuseInteractivePTY_StartInputResizeTeardown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	workdir := t.TempDir()
	fakeBin := writeFakeMuseBinary(t)
	t.Setenv("MUSE_BIN", fakeBin)
	t.Setenv("MUSE_AUTH_PATH", writeMuseAuthFixture(t))

	reg := buildRegistryFromCtors(quietLogger(), agentRunProviderCtors(), "donmai")
	t.Cleanup(func() { _ = reg.Shutdown(context.Background()) })

	provider, err := reg.Resolve(agent.ProviderMuse)
	if err != nil {
		t.Fatalf("production registry cannot resolve the muse provider: %v", err)
	}
	harnessProvider, ok := provider.(agent.HarnessProvider)
	if !ok {
		t.Fatal("resolved muse provider does not expose a harness manifest")
	}
	if !harnessProvider.Manifest().Caps.SupportsInteractivePTY {
		t.Fatal("muse manifest declares interactivePty=false, want true (interactive terminal spawn mode)")
	}

	handle, err := provider.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn through the production registry: %v", err)
	}

	interactive, ok := handle.(agent.InteractiveCapable)
	if !ok {
		t.Fatal("muse handle does not implement agent.InteractiveCapable")
	}
	session := interactive.InteractiveSession()
	if session == nil {
		t.Fatal("InteractiveSession() returned nil")
	}

	// Start: the InitEvent arrives once the PTY child is up.
	deadline := time.After(20 * time.Second)
	select {
	case event, ok := <-handle.Events():
		if !ok {
			t.Fatal("events channel closed before InitEvent")
		}
		if _, isInit := event.(agent.InitEvent); !isInit {
			t.Fatalf("first event = %T, want agent.InitEvent", event)
		}
	case <-deadline:
		t.Fatal("timed out waiting for InitEvent")
	}

	// Input: written bytes reach the child verbatim. The quit token goes
	// LAST: the child exits on QUIT, and a resize after exit refuses.
	if _, err := session.WriteInput([]byte("hello-input\r")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitForChildBytes(t, workdir, "hello-input")

	// Resize: the host applies the geometry verbatim; the snapshot
	// reflects it.
	if err := session.Resize(100, 40, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	screen, _, err := session.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if screen.Cols != 100 || screen.Rows != 40 {
		t.Errorf("snapshot geometry = %dx%d, want 100x40", screen.Cols, screen.Rows)
	}

	// Quit only after input + resize are proven: the child exits, which
	// drives the terminal ResultEvent below.
	if _, err := session.WriteInput([]byte("QUIT")); err != nil {
		t.Fatalf("WriteInput quit token: %v", err)
	}
	waitForChildBytes(t, workdir, "QUIT")

	// Teardown: the terminal ResultEvent arrives and the stream closes.
	// The child exits on its own after QUIT; Stop is idempotent cover.
	gotResult := false
	limit := time.After(20 * time.Second)
loop:
	for !gotResult {
		select {
		case event, ok := <-handle.Events():
			if !ok {
				break loop
			}
			if result, isResult := event.(agent.ResultEvent); isResult {
				gotResult = true
				if !result.Success {
					t.Errorf("terminal ResultEvent success=false, errors=%v", result.Errors)
				}
			}
		case <-limit:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
	if !gotResult {
		t.Fatal("never observed a terminal ResultEvent")
	}
	if err := handle.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	drained := false
	for !drained {
		select {
		case _, ok := <-handle.Events():
			if !ok {
				drained = true
			}
		case <-time.After(10 * time.Second):
			t.Fatal("events channel never closed after teardown")
		}
	}
}

// writeFakeMuseBinary materializes a fake muse host: it answers the
// `--version` construction probe with the pinned shape and the `exec`
// login probe with the model-validation failure that proves a usable
// login (the probe model id must name the harness's own unresolvable
// probe model, mirroring the provider's own fixtures); any other
// invocation runs a TUI stub that records every stdin byte and exits
// once it sees the quit token.
func writeFakeMuseBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/bash\n" +
		"if [[ \"$1\" == \"--version\" ]]; then echo \"Muse Code 1.4.6 (1.4.6-R6000.1)\"; exit 0; fi\n" +
		"if [[ \"$1\" == \"exec\" ]]; then echo \"model donmai-login-probe-nonexistent-model does not exist or you lack access\" >&2; exit 1; fi\n" +
		"touch \"$PWD/input.txt\"\n" +
		"while IFS= read -r -n1 ch; do\n" +
		"  printf '%s' \"$ch\" >> \"$PWD/input.txt\"\n" +
		"  if grep -q QUIT \"$PWD/input.txt\" 2>/dev/null; then exit 0; fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	return bin
}

// writeMuseAuthFixture materializes a shape-valid login file for the
// host login check's file leg (MUSE_AUTH_PATH override).
func writeMuseAuthFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body := `{"schema_version":1,"providers":{"meta":{"mechanism":"oauth","storage":"keychain"}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitForChildBytes polls the TUI stub's record file until it contains
// the written token.
func waitForChildBytes(t *testing.T, workdir, want string) {
	t.Helper()
	limit := time.Now().Add(15 * time.Second)
	for {
		body, err := os.ReadFile(filepath.Join(workdir, "input.txt"))
		if err == nil && strings.Contains(string(body), want) {
			return
		}
		if time.Now().After(limit) {
			t.Fatalf("child never received %q (input.txt=%q, err=%v)", want, string(body), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
