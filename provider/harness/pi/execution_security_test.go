package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
	"github.com/RenseiAI/donmai/runtime/workarea"
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

// TestPiWorkareaBothModesAttestsExactContract pins AC-1: a both-mode,
// nonempty-digest proof makes pi's manifest emit exactly session-root-v1
// with isolated-read-only-v1 enforcement, which is what
// runner.ProviderView.WorkareaExecutorCapabilities projects verbatim
// (runner/provider_view.go:753-805: only positive manifest protocols
// become exact registration attestations). Mutant RED: omitting either
// field (protocols or enforcement) fails this test.
func TestPiWorkareaBothModesAttestsExactContract(t *testing.T) {
	old := piSecurityAttestation
	piSecurityAttestation = func(context.Context, *Provider, string) (confinement.Attestation, bool) {
		return confinement.Attestation{
			SelfTestDigest: "sha256:both-mode-workarea-test",
			SessionModes:   []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled},
		}, true
	}
	t.Cleanup(func() { piSecurityAttestation = old })

	p := &Provider{binary: "/must-not-execute", opts: Options{RequireConfinement: true}}
	manifest := p.Manifest()
	if !reflect.DeepEqual(manifest.Caps.MultiRepositoryWorkareaProtocols, []string{"session-root-v1"}) {
		t.Fatalf("MultiRepositoryWorkareaProtocols = %q, want exactly [session-root-v1]", manifest.Caps.MultiRepositoryWorkareaProtocols)
	}
	if manifest.Caps.RepositoryAuthorityEnforcement != "isolated-read-only-v1" {
		t.Fatalf("RepositoryAuthorityEnforcement = %q, want isolated-read-only-v1", manifest.Caps.RepositoryAuthorityEnforcement)
	}
	if got := len(manifest.ExecutionSecurity.Levels); got != 2 {
		t.Fatalf("both-mode attestation declared %d execution-security levels, want 2", got)
	}
	// The registration projection carries the same two contract values:
	// rebuild the workarea capabilities exactly as ProviderView does and
	// require them to admit a read-only declaration.
	caps := workarea.ExecutorWorkareaCapabilities{
		MultiRepositoryWorkareaProtocols: []workarea.Protocol{workarea.Protocol(manifest.Caps.MultiRepositoryWorkareaProtocols[0])},
		RepositoryAuthorityEnforcement:   workarea.RepositoryAuthorityEnforcement(manifest.Caps.RepositoryAuthorityEnforcement),
	}
	if len(caps.MultiRepositoryWorkareaProtocols) != 1 || caps.MultiRepositoryWorkareaProtocols[0] != workarea.ProtocolSessionRootV1 {
		t.Fatalf("registration protocols = %v, want exactly [session-root-v1]", caps.MultiRepositoryWorkareaProtocols)
	}
	if caps.RepositoryAuthorityEnforcement != workarea.RepositoryAuthorityIsolatedReadOnlyV1 {
		t.Fatalf("registration enforcement = %q, want isolated-read-only-v1", caps.RepositoryAuthorityEnforcement)
	}
}

// TestPiWorkareaProofsWithoutBothModesEmitNoRegistration pins AC-2: failed,
// empty-digest, degraded/processless (all ok==false or skipped), and
// one-mode proofs emit no pi workarea registration, while a one-mode proof
// still renders its single execution-security mode. Mutant RED: gating
// workarea on the digest alone (ignoring modes) admits the one-mode case.
func TestPiWorkareaProofsWithoutBothModesEmitNoRegistration(t *testing.T) {
	bothModes := []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}
	for _, tc := range []struct {
		name         string
		attestation  confinement.Attestation
		ok           bool
		opts         Options
		wantSecurity bool // execution-security still renders
	}{
		{name: "failed", attestation: confinement.Attestation{}, ok: false, opts: Options{RequireConfinement: true}},
		{name: "empty digest", attestation: confinement.Attestation{SelfTestDigest: "", SessionModes: bothModes}, ok: true, opts: Options{RequireConfinement: true}},
		{name: "degraded", attestation: confinement.Attestation{}, ok: false, opts: Options{RequireConfinement: true}},
		{name: "processless", attestation: confinement.Attestation{SelfTestDigest: "sha256:processless", SessionModes: bothModes}, ok: true, opts: Options{RequireConfinement: true, skipProcess: true}},
		{name: "one mode headless", attestation: confinement.Attestation{SelfTestDigest: "sha256:one-mode", SessionModes: []agent.PromptSessionMode{agent.PromptModeAutonomous}}, ok: true, opts: Options{RequireConfinement: true}, wantSecurity: true},
		{name: "one mode interactive", attestation: confinement.Attestation{SelfTestDigest: "sha256:one-mode-pty", SessionModes: []agent.PromptSessionMode{agent.PromptModeHumanControlled}}, ok: true, opts: Options{RequireConfinement: true}, wantSecurity: true},
		{name: "unrequired", attestation: confinement.Attestation{SelfTestDigest: "sha256:unrequired", SessionModes: bothModes}, ok: true, opts: Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := piSecurityAttestation
			piSecurityAttestation = func(context.Context, *Provider, string) (confinement.Attestation, bool) {
				return tc.attestation, tc.ok
			}
			t.Cleanup(func() { piSecurityAttestation = old })

			p := &Provider{binary: "/must-not-execute", opts: tc.opts}
			manifest := p.Manifest()
			if len(manifest.Caps.MultiRepositoryWorkareaProtocols) != 0 {
				t.Fatalf("%s proof declares protocols %q, want none", tc.name, manifest.Caps.MultiRepositoryWorkareaProtocols)
			}
			if manifest.Caps.RepositoryAuthorityEnforcement != "" {
				t.Fatalf("%s proof declares enforcement %q, want empty", tc.name, manifest.Caps.RepositoryAuthorityEnforcement)
			}
			if tc.wantSecurity {
				if got := len(manifest.ExecutionSecurity.Levels); got != 2 {
					t.Fatalf("%s proof renders %d execution-security levels, want 2 (one-mode rendering remains)", tc.name, got)
				}
				for _, level := range manifest.ExecutionSecurity.Levels {
					if !reflect.DeepEqual(level.Modes, tc.attestation.SessionModes) {
						t.Fatalf("%s proof security modes = %v, want exactly the attested mode", tc.name, level.Modes)
					}
				}
			} else if len(manifest.ExecutionSecurity.Levels) != 0 {
				t.Fatalf("%s proof renders %d execution-security levels, want none", tc.name, len(manifest.ExecutionSecurity.Levels))
			}

			// No registration: ProviderView only projects positive manifest
			// protocols, so empty caps register nothing.
			if len(manifest.Caps.MultiRepositoryWorkareaProtocols) != 0 || manifest.Caps.RepositoryAuthorityEnforcement != "" {
				t.Fatalf("%s proof would register: %q/%q", tc.name, manifest.Caps.MultiRepositoryWorkareaProtocols, manifest.Caps.RepositoryAuthorityEnforcement)
			}
		})
	}
}

// TestPiManifestSharesOneAttestation pins AC-3: one Manifest() evaluation
// calls piSecurityAttestation exactly once. Mutant RED: probing workarea
// and execution security independently calls it twice.
func TestPiManifestSharesOneAttestation(t *testing.T) {
	calls := 0
	old := piSecurityAttestation
	piSecurityAttestation = func(context.Context, *Provider, string) (confinement.Attestation, bool) {
		calls++
		return confinement.Attestation{
			SelfTestDigest: "sha256:single-call-test",
			SessionModes:   []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled},
		}, true
	}
	t.Cleanup(func() { piSecurityAttestation = old })

	p := &Provider{binary: "/must-not-execute", opts: Options{RequireConfinement: true}}
	manifest := p.Manifest()
	if calls != 1 {
		t.Fatalf("Manifest() called piSecurityAttestation %d times, want exactly once", calls)
	}
	if !slices.Contains(manifest.Caps.MultiRepositoryWorkareaProtocols, "session-root-v1") {
		t.Fatalf("single shared attestation did not attest workarea: %#v", manifest.Caps)
	}
	if len(manifest.ExecutionSecurity.Levels) != 2 {
		t.Fatalf("single shared attestation rendered %d security levels, want 2", len(manifest.ExecutionSecurity.Levels))
	}
}

// TestPiWorkareaUnavailableBackendAdvertisesNothing pins AC-5: an
// unavailable backend advertises no workarea capability and its Spawn
// refuses an authority-bearing spec without running the child or planting
// files. Mutant RED: granting from RequireConfinement alone advertises
// session-root-v1 even when the backend is absent.
func TestPiWorkareaUnavailableBackendAdvertisesNothing(t *testing.T) {
	oldBackend := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = oldBackend })
	oldAttestation := piSecurityAttestation
	// Production piSecurityAttestation would fail closed without a backend;
	// keep the seam honest by not stubbing success here: call through.
	piSecurityAttestation = oldAttestation
	t.Cleanup(func() { piSecurityAttestation = oldAttestation })

	p := &Provider{binary: "/must-not-execute", opts: Options{RequireConfinement: true}}
	manifest := p.Manifest()
	if len(manifest.Caps.MultiRepositoryWorkareaProtocols) != 0 || manifest.Caps.RepositoryAuthorityEnforcement != "" {
		t.Fatalf("backend-absent manifest declares workarea %q/%q, want none", manifest.Caps.MultiRepositoryWorkareaProtocols, manifest.Caps.RepositoryAuthorityEnforcement)
	}

	root := t.TempDir()
	mut := filepath.Join(root, "mut")
	ro := filepath.Join(root, "ro")
	for _, dir := range []string{mut, ro} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	spec := agent.Spec{
		Cwd:          mut,
		Prompt:       "probe",
		SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: root, SelectedPath: mut,
			MutablePaths: []string{mut}, ReadOnlyPaths: []string{ro}, Enforcement: "isolated-read-only-v1",
		},
	}
	_, err := p.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("backend-absent Spawn admitted a read-only authority")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("backend-absent Spawn = %v, want %v", err, agent.ErrSpawnFailed)
	}
	var denial *agent.SpecAdmissionError
	if !errors.As(err, &denial) || denial.Field != "repositoryAuthority" {
		t.Fatalf("backend-absent Spawn = %v, want a repositoryAuthority admission denial", err)
	}
	if _, statErr := os.Stat(sessionStateRoot(spec)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused authority Spawn planted state: %v", statErr)
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
