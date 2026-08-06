package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

func TestSpawn_Interactive_DeliversAdaptedPTYSeedExactly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	requireSh := func() {
		if _, err := exec.LookPath("sh"); err != nil {
			t.Skip("sh not available")
		}
	}
	requireSh()

	const seed = "  opaque shell seed  "
	tmp := t.TempDir()
	capture := filepath.Join(tmp, "seed.bin")
	script := filepath.Join(tmp, "capture-shell")
	scriptBody := "#!/bin/sh\ndd bs=1 count=\"$SEED_BYTES\" of=\"$SEED_CAPTURE\" 2>/dev/null\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o600); err != nil {
		t.Fatalf("write fake shell: %v", err)
	}
	if err := os.Chmod(script, 0o700); err != nil { //nolint:gosec // test fixture script needs exec bit
		t.Fatalf("chmod fake shell: %v", err)
	}
	t.Setenv("SHELL", script)

	plan := agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		UserPrompt:       agent.PromptContent{ID: "interactive-user-task", Text: seed, Required: true},
	}
	var receipts []agent.PromptDeliveryReceipt
	p, _ := New()
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         tmp,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		PromptPlan:  &plan,
		Env: map[string]string{
			"SEED_CAPTURE": capture,
			"SEED_BYTES":   fmt.Sprintf("%d", len(seed)+1),
		},
		OnPromptAdapted: func(receipt agent.PromptDeliveryReceipt) error {
			receipts = append(receipts, receipt)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	deadline := time.Now().Add(5 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got, err = os.ReadFile(capture)
		if err == nil && len(got) == len(seed)+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want := seed + "\n"; string(got) != want {
		t.Fatalf("captured PTY seed = %q, want exact adapted bytes %q (read error %v)", got, want, err)
	}
	if len(receipts) != 1 || receipts[0].Decision != "ready" {
		t.Fatalf("receipts = %+v, want one ready decision", receipts)
	}
	found := false
	for _, entry := range receipts[0].Entries {
		if entry.ID == "interactive-user-task" {
			found = true
			if entry.Outcome != agent.PromptOutcomeDelivered || entry.Delivery != agent.PromptDeliveryShellPTYSeed {
				t.Fatalf("user-task receipt = %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("ready receipt omitted interactive user task")
	}
}

func TestSpawn_Interactive_SeedFailureReturnsNoSessionAndFinalDenial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	t.Setenv("SHELL", "/bin/sh")
	tests := []struct {
		name string
		err  error
	}{
		{name: "partial write", err: errors.New("partial PTY seed")},
		{name: "write error", err: errors.New("PTY closed")},
		{name: "cancelled", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := agent.PromptPlan{
				ContractVersion:  agent.PromptContractVersion,
				BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
				UserPrompt:       agent.PromptContent{ID: "interactive-user-task", Text: "seed", Required: true},
			}
			var receipts []agent.PromptDeliveryReceipt
			p := &Provider{deliverSeed: func(context.Context, agent.Handle, agent.InteractiveSession, string) error {
				return tt.err
			}}
			h, err := p.Spawn(context.Background(), agent.Spec{
				Cwd:         t.TempDir(),
				Interactive: &agent.InteractiveSpec{},
				PromptPlan:  &plan,
				OnPromptAdapted: func(receipt agent.PromptDeliveryReceipt) error {
					receipts = append(receipts, receipt)
					return nil
				},
			})
			if h != nil {
				t.Fatal("Spawn returned a usable session after failed seed")
			}
			if !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), tt.err.Error()) {
				t.Fatalf("Spawn error = %v, want ErrSpawnFailed containing %q", err, tt.err)
			}
			if len(receipts) < 2 {
				t.Fatalf("receipts = %+v, want pre-spawn decision followed by application denial", receipts)
			}
			final := receipts[len(receipts)-1]
			if final.Decision != "denied" {
				t.Fatalf("final receipt decision = %q, want denied", final.Decision)
			}
			for _, entry := range final.Entries {
				if entry.ID == "interactive-user-task" && (entry.Outcome != agent.PromptOutcomeDenied || entry.DenialCode != agent.PromptDenialApplicationFailed) {
					t.Fatalf("final user-task receipt = %+v, want application_failed denial", entry)
				}
			}
		})
	}
}
