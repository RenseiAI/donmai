package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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
