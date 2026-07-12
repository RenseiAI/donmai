package shell

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestName(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Name(); got != agent.ProviderShell {
		t.Errorf("Name() = %q, want %q", got, agent.ProviderShell)
	}
}

func TestCapabilities_AllOffExceptLabel(t *testing.T) {
	t.Parallel()
	p, _ := New()
	caps := p.Capabilities()
	want := agent.Capabilities{HumanLabel: "Shell"}
	if caps != want {
		t.Errorf("Capabilities() = %+v, want %+v", caps, want)
	}
}

func TestManifest_ProjectsCapabilities(t *testing.T) {
	t.Parallel()
	p := &Provider{}
	mf := p.Manifest()
	caps := p.Capabilities()

	if mf.Name != agent.HarnessShell {
		t.Errorf("Manifest().Name = %q, want %q", mf.Name, agent.HarnessShell)
	}
	if !mf.Caps.SupportsInteractivePTY {
		t.Error("Manifest().Caps.SupportsInteractivePTY = false, want true")
	}
	if mf.Caps.Transport != agent.TransportPTY {
		t.Errorf("Manifest().Caps.Transport = %q, want %q", mf.Caps.Transport, agent.TransportPTY)
	}
	if len(mf.Caps.Drives) != 0 || len(mf.Caps.DrivesHosts) != 0 {
		t.Errorf("shell must drive no model endpoint; got Drives=%v DrivesHosts=%v", mf.Caps.Drives, mf.Caps.DrivesHosts)
	}
	if mf.Caps.SupportsMessageInjection != caps.SupportsMessageInjection ||
		mf.Caps.SupportsSessionResume != caps.SupportsSessionResume ||
		mf.Caps.SupportsToolPlugins != caps.SupportsToolPlugins {
		t.Errorf("Manifest().Caps agent-loop bools must project Capabilities(): mf=%+v caps=%+v", mf.Caps, caps)
	}
}

func TestSpawn_NonInteractive_ReturnsErrUnsupported(t *testing.T) {
	t.Parallel()
	p, _ := New()
	_, err := p.Spawn(context.Background(), agent.Spec{Prompt: "hello"})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Spawn(no Interactive) error = %v, want wrapping agent.ErrUnsupported", err)
	}
}

func TestResume_ReturnsErrUnsupported(t *testing.T) {
	t.Parallel()
	p, _ := New()
	_, err := p.Resume(context.Background(), "some-id", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Resume error = %v, want wrapping agent.ErrUnsupported", err)
	}
}

func TestShutdown_Noop(t *testing.T) {
	t.Parallel()
	p, _ := New()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestShellBinary_DefaultsWhenUnset(t *testing.T) {
	// Mutates the process-global $SHELL; must not run in parallel with
	// other tests in this package that read it.
	t.Setenv("SHELL", "")
	if got := shellBinary(); got != DefaultShell {
		t.Errorf("shellBinary() = %q, want %q", got, DefaultShell)
	}
}

func TestShellBinary_HonorsEnv(t *testing.T) {
	t.Setenv("SHELL", "/custom/shell")
	if got := shellBinary(); got != "/custom/shell" {
		t.Errorf("shellBinary() = %q, want %q", got, "/custom/shell")
	}
}

// TestSpawn_Interactive_RunsUnderPTY proves Spawn actually wires through to
// ptycli.Spawn end-to-end (not just the error-path branch above), using
// $SHELL pointed at a trivial fake script — the same PATH-shim technique
// provider/harness/agycli's handle_test.go uses.
func TestSpawn_Interactive_RunsUnderPTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("SHELL", "/bin/sh")

	p, _ := New()
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}

	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("events channel did not close in time")
			return
		}
	}
}
