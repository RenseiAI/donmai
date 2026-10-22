package pi

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

func confinedSecurityStamp() *agent.ExecutionSecurity {
	levels := agent.IndexZeroExecutionSecurityLevels()
	levels.FileWrite = agent.FileWriteWorkarea
	levels.Isolation = agent.IsolationOSSandbox
	return &agent.ExecutionSecurity{Version: agent.ExecutionSecurityWireVersion, Levels: levels}
}

// TestExecutionSecurityModesArePinnedToAttestedSession pins the mode
// binding: an attestation that proves only the headless spawn path must
// admit workarea-write and OS-sandbox isolation for headless sessions and
// refuse them as unrenderable for interactive ones. Both modes pass the
// self-test on a real confined host, so the live suite cannot tell the
// binding from its absence; the seam pins a single-mode attestation
// instead. The assertions drive Manifest (the production rendering entry)
// through the shared Spawn admission path.
func TestExecutionSecurityModesArePinnedToAttestedSession(t *testing.T) {
	old := piSecurityAttestation
	piSecurityAttestation = func(context.Context, *Provider, string) (confinement.Attestation, bool) {
		return confinement.Attestation{
			SelfTestDigest: "sha256:pinned-mode-test",
			SessionModes:   []agent.PromptSessionMode{agent.PromptModeAutonomous},
		}, true
	}
	t.Cleanup(func() { piSecurityAttestation = old })

	p := &Provider{binary: "/must-not-execute", opts: Options{RequireConfinement: true}}
	manifest := p.Manifest()
	if got := len(manifest.ExecutionSecurity.Levels); got != 2 {
		t.Fatalf("single-mode attestation declared %d levels, want 2", got)
	}
	for _, level := range manifest.ExecutionSecurity.Levels {
		if !reflect.DeepEqual(level.Modes, []agent.PromptSessionMode{agent.PromptModeAutonomous}) {
			t.Fatalf("%s/%s modes = %v, want exactly the attested headless mode", level.Dimension, level.Level, level.Modes)
		}
		if level.EvidenceDigest == "" {
			t.Fatalf("%s/%s carries no self-test digest", level.Dimension, level.Level)
		}
	}

	headless := agent.Spec{Cwd: t.TempDir(), Prompt: "probe", ExecutionSecurity: confinedSecurityStamp()}
	if _, err := agent.PrepareHarness(headless, manifest); err != nil {
		t.Fatalf("headless session with attested mode was refused: %v", err)
	}

	interactive := agent.Spec{Cwd: t.TempDir(), Prompt: "probe", ExecutionSecurity: confinedSecurityStamp(), Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}}
	_, err := agent.PrepareHarness(interactive, manifest)
	var refusal *agent.ExecutionSecurityError
	if !errors.As(err, &refusal) || refusal.Code != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("interactive session without attested mode = %v, want unrenderable refusal", err)
	}
	want := []agent.ExecutionSecurityDimension{agent.ExecutionSecurityFileWrite, agent.ExecutionSecurityIsolation}
	if !reflect.DeepEqual(refusal.Dimensions, want) {
		t.Fatalf("interactive refusal dimensions = %v, want %v", refusal.Dimensions, want)
	}
}

func TestExecutionSecurityUnattestedPiRefusesBeforeSpawn(t *testing.T) {
	old := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = old })
	for _, host := range []struct {
		name string
		opts Options
	}{
		{"unconfined", Options{}},
		{"backend absent", Options{RequireConfinement: true}},
		{"processless", Options{RequireConfinement: true, skipProcess: true}},
	} {
		for _, mode := range []string{"spawn", "resume", "interactive"} {
			t.Run(host.name+"/"+mode, func(t *testing.T) {
				p := &Provider{binary: "/must-not-execute", opts: host.opts}
				spec := agent.Spec{Cwd: t.TempDir(), Prompt: "probe", ExecutionSecurity: confinedSecurityStamp()}
				if mode == "interactive" {
					spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
				}
				var err error
				if mode == "resume" {
					_, err = p.Resume(context.Background(), "session", spec)
				} else {
					_, err = p.Spawn(context.Background(), spec)
				}
				var refusal *agent.ExecutionSecurityError
				want := []agent.ExecutionSecurityDimension{agent.ExecutionSecurityFileWrite, agent.ExecutionSecurityIsolation}
				if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &refusal) || refusal.Code != agent.ExecutionSecurityUnrenderable || !reflect.DeepEqual(refusal.Dimensions, want) {
					t.Fatalf("entry point refusal = %v, want unrenderable write and isolation", err)
				}
				if _, err := os.Stat(sessionStateRoot(spec)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused session materialized state: %v", err)
				}
			})
		}
	}
}
