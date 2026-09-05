// Package ptycli_test is the registry-driven cross-harness interactive
// spawn-mode test (W4 deliverable 5). It lives here — outside package
// ptycli, as an external test package — because it needs to import every
// interactive-capable harness (claude, codex, shell, pi, stub) to exercise their own
// production Spawn/SpawnInteractive call sites, while ptycli itself (the
// shared driver those harnesses route through) never imports any of them.
// This creates no import cycle: matrix and the harness packages already
// depend on ptycli, and an external _test package may depend on anything
// its production package does not.
package ptycli_test

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
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// shimScript is the fake-CLI PATH shim: it prints the env/geometry signals
// this test asserts on, then exits. It stands in for claude/codex/the login
// shell in every table entry below.
const shimScript = "#!/bin/bash\n" +
	"echo \"TERM=$TERM\"\n" +
	"echo \"COLORTERM=$COLORTERM\"\n" +
	"echo \"COLUMNS=${COLUMNS:-unset}\"\n" +
	"echo \"LINES=${LINES:-unset}\"\n" +
	"stty size\n"

// writeShim materializes the shim script fixture — the same PATH-shim
// technique provider/harness/agycli's handle_test.go newFakeProvider uses:
// write without the exec bit, then chmod after close (avoids ETXTBSY on
// Linux when a writable fd is still open on an executable inode).
func writeShim(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-interactive-cli")
	if err := os.WriteFile(bin, []byte(shimScript), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	return bin
}

// spawnFn constructs the fake-binary-backed interactive spawn for one
// registry-declared harness, given the shim path and a Spec already carrying
// Spec.Interactive.
type spawnFn func(t *testing.T, bin string, spec agent.Spec) (agent.Handle, error)

type terminalEnvCase struct {
	name          string
	parentAbsent  bool
	parentTERM    string
	parentColor   string
	requestEnv    map[string]string
	wantTERM      string
	wantCOLORTERM string
}

var terminalEnvCases = []terminalEnvCase{
	{
		name:          "parent absent uses interactive defaults",
		parentAbsent:  true,
		wantTERM:      "xterm-256color",
		wantCOLORTERM: "truecolor",
	},
	{
		name:          "parent values do not override interactive defaults",
		parentTERM:    "dumb",
		parentColor:   "",
		wantTERM:      "xterm-256color",
		wantCOLORTERM: "truecolor",
	},
	{
		name:        "explicit request overrides parent and defaults",
		parentTERM:  "dumb",
		parentColor: "",
		requestEnv: map[string]string{
			"TERM":      "vt100",
			"COLORTERM": "24bit",
		},
		wantTERM:      "vt100",
		wantCOLORTERM: "24bit",
	},
}

// spawnTable maps HarnessName -> "how to construct a fake-binary instance of
// it." The set of harnesses this test COVERS is decided by the registry
// filter in the test below (matrix.HarnessHarvestList(), filtered on
// Caps.SupportsInteractivePTY) — this map only supplies the construction
// mechanics for the harnesses the registry names. A harness that flips
// SupportsInteractivePTY=true without a matching entry here fails the
// coverage assertion in the test rather than silently going untested: P8
// adding an interactive Vertex/Bedrock/Azure/OpenRouter/Fireworks/Groq
// harness must add a table entry in the same change.
func spawnTable() map[agent.HarnessName]spawnFn {
	return map[agent.HarnessName]spawnFn{
		agent.HarnessClaudeCode: func(t *testing.T, bin string, spec agent.Spec) (agent.Handle, error) {
			p, err := claude.New(claude.Options{
				Binary:   bin,
				LookPath: func(string) (string, error) { return bin, nil },
			})
			if err != nil {
				t.Fatalf("claude.New(fake): %v", err)
			}
			return p.Spawn(context.Background(), spec)
		},
		agent.HarnessCodex: func(_ *testing.T, bin string, spec agent.Spec) (agent.Handle, error) {
			// SpawnInteractive needs no live Provider/app-server at all —
			// it is the interactive path's own entry point.
			return codex.SpawnInteractive(context.Background(), codex.Options{CodexBin: bin}, spec)
		},
		agent.HarnessShell: func(t *testing.T, bin string, spec agent.Spec) (agent.Handle, error) {
			// shell always spawns $SHELL (no Spec-provided command slot),
			// so pointing it at the fake binary means overriding $SHELL for
			// the duration of this subtest.
			t.Setenv("SHELL", bin)
			p, err := shell.New()
			if err != nil {
				t.Fatalf("shell.New: %v", err)
			}
			return p.Spawn(context.Background(), spec)
		},
		agent.HarnessStub: func(t *testing.T, bin string, spec agent.Spec) (agent.Handle, error) {
			// The stub's interactive child defaults to THIS executable
			// re-invoked on its hidden subcommand; under `go test` that is the
			// test binary, which answers no such subcommand. Point it at the
			// shim instead — the same substitution every other row makes.
			p, err := stub.New(stub.WithStubAgentCommand(bin))
			if err != nil {
				t.Fatalf("stub.New(fake): %v", err)
			}
			return p.Spawn(context.Background(), spec)
		},
		agent.HarnessPi: func(t *testing.T, bin string, spec agent.Spec) (agent.Handle, error) {
			// Point pi at the fake shim via PiBin and stub the version probe
			// (the shim is not a real pi, so `--version` would not satisfy the
			// pin). pi.Spawn routes Spec.Interactive to its own spawnInteractive,
			// which drives this same shared ptycli driver.
			p, err := pi.New(pi.Options{
				PiBin:        bin,
				VersionProbe: func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
			})
			if err != nil {
				t.Fatalf("pi.New(fake): %v", err)
			}
			return p.Spawn(context.Background(), spec)
		},
	}
}

// TestInteractiveSpawn_EnvAndGeometry_RegistryDriven is the deliverable-5
// TERM/COLORTERM/size env-correctness matrix test. It enumerates
// interactive-capable harnesses from the manifest REGISTRY
// (matrix.HarnessHarvestList()) filtered on Caps.SupportsInteractivePTY —
// never a hardcoded {claude, codex, shell} literal (binding addendum 3 of
// the W4 plan) — and, for each, spawns a fake CLI shim under the harness's
// REAL interactive Spawn call site, asserting the PTY child observed
// TERM=xterm-256color, COLORTERM=truecolor, and the requested Cols×Rows
// geometry (via `stty size`), that the returned handle satisfies
// agent.InteractiveCapable, and that Snapshot round-trips the same geometry.
//
// t.Setenv in the shell branch of spawnTable mutates the process-global
// $SHELL, so subtests here run SEQUENTIALLY (no t.Parallel) rather than risk
// racing that mutation against a concurrent subtest.
func TestInteractiveSpawn_EnvAndGeometry_RegistryDriven(t *testing.T) {
	var interactive []agent.HarnessName
	for _, h := range matrix.HarnessHarvestList() {
		if h.Manifest().Caps.SupportsInteractivePTY {
			interactive = append(interactive, h.Name)
		}
	}
	if len(interactive) < 3 {
		t.Fatalf("expected at least 3 interactive-capable harnesses declared in the registry, got %d: %v", len(interactive), interactive)
	}

	table := spawnTable()
	for _, name := range interactive {
		t.Run(string(name), func(t *testing.T) {
			fn, ok := table[name]
			if !ok {
				t.Fatalf("harness %q declares SupportsInteractivePTY=true but this test has no fake-binary constructor for it — add one to spawnTable()", name)
			}

			for _, envCase := range terminalEnvCases {
				t.Run(envCase.name, func(t *testing.T) {
					setParentTerminalEnv(t, envCase)
					bin := writeShim(t)
					spec := agent.Spec{
						Cwd:         t.TempDir(),
						Env:         envCase.requestEnv,
						Interactive: &agent.InteractiveSpec{Cols: 100, Rows: 40},
					}

					h, err := fn(t, bin, spec)
					if err != nil {
						t.Fatalf("interactive Spawn: %v", err)
					}
					defer func() { _ = h.Stop(context.Background()) }()

					ic, ok := h.(agent.InteractiveCapable)
					if !ok {
						t.Fatalf("handle for %q does not implement agent.InteractiveCapable", name)
					}
					sess := ic.InteractiveSession()
					if sess == nil {
						t.Fatalf("InteractiveSession() returned nil for %q", name)
					}

					out := collectOutput(t, sess)
					if want := "TERM=" + envCase.wantTERM; !strings.Contains(out, want) {
						t.Errorf("%q: output missing %s; got:\n%s", name, want, out)
					}
					if want := "COLORTERM=" + envCase.wantCOLORTERM; !strings.Contains(out, want) {
						t.Errorf("%q: output missing %s; got:\n%s", name, want, out)
					}
					if !strings.Contains(out, "40 100") {
						t.Errorf("%q: `stty size` output missing \"40 100\" (rows cols); got:\n%s", name, out)
					}

					// Snapshot round-trip: callable at any point, never erroring,
					// and reflects the same geometry Spec.Interactive requested.
					scr, _, err := sess.Snapshot()
					if err != nil {
						t.Errorf("%q: Snapshot: %v", name, err)
					}
					if scr.Cols != 100 || scr.Rows != 40 {
						t.Errorf("%q: snapshot geometry = %dx%d, want 100x40", name, scr.Cols, scr.Rows)
					}
				})
			}
		})
	}
}

func setParentTerminalEnv(t *testing.T, envCase terminalEnvCase) {
	t.Helper()
	if envCase.parentAbsent {
		unsetEnv(t, "TERM")
		unsetEnv(t, "COLORTERM")
		return
	}
	t.Setenv("TERM", envCase.parentTERM)
	t.Setenv("COLORTERM", envCase.parentColor)
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	old, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			if err := os.Setenv(key, old); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
			return
		}
		if err := os.Unsetenv(key); err != nil {
			t.Errorf("clear %s: %v", key, err)
		}
	})
}

// collectOutput subscribes from seq 0 and accumulates every Output frame's
// raw bytes until the subscription closes (delivered after the Exit frame)
// or the deadline fires.
func collectOutput(t *testing.T, sess agent.InteractiveSession) string {
	t.Helper()
	sub, err := sess.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	var buf strings.Builder
	deadline := time.After(20 * time.Second)
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				return buf.String()
			}
			if f.Type == attachwire.TypeOutput {
				buf.Write(attachwire.DecodeOutput(f.Payload).Data)
			}
		case <-deadline:
			t.Fatal("timed out collecting interactive output")
			return buf.String()
		}
	}
}
