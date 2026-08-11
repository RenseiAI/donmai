package pi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// captureArgvEnvScript is the fake-pi body the fixture tests use. It dumps its
// own argv (one element per line so an argument containing spaces still lands on
// one line) and the provider-pin / handshake / home env vars to files in the
// session cwd, then exits immediately — no PTY interaction is needed for the
// argv+env assertions. DONMAI_PI_HANDSHAKE is wrapped in brackets so an ABSENT
// value is distinguishable from any value at all.
const captureArgvEnvScript = `
printf '%s\n' "$@" > "$PWD/argv.txt"
{
  printf 'DONMAI_PI_BASE_URL=%s\n' "$DONMAI_PI_BASE_URL"
  printf 'DONMAI_PI_API=%s\n' "$DONMAI_PI_API"
  printf 'DONMAI_PI_MODEL=%s\n' "$DONMAI_PI_MODEL"
  printf 'DONMAI_PI_KEY=%s\n' "$DONMAI_PI_KEY"
  printf 'DONMAI_PI_HANDSHAKE=[%s]\n' "$DONMAI_PI_HANDSHAKE"
  printf 'PI_HOME=%s\n' "$PI_HOME"
} > "$PWD/env.txt"
`

// newFakeInteractivePiProvider builds a pi Provider whose binary is a fake
// script, so the interactive PTY spawn mode can be exercised without the real
// `pi`. The Provider is constructed by field (same package) to skip the
// version-pin probe New performs — the probe is irrelevant to argv/env wiring.
func newFakeInteractivePiProvider(t *testing.T, script string) *Provider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	return &Provider{binary: bin}
}

// awaitPTYExit drains h.Events() until the channel closes, so the capture files
// the fake script wrote are guaranteed present on disk by the time the test
// reads them.
func awaitPTYExit(t *testing.T, h agent.Handle) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake pi exit")
		}
	}
}

func readCapturedFile(t *testing.T, workdir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func argvLines(captured string) []string {
	trimmed := strings.TrimRight(captured, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func flagValue(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

// TestSpawn_Interactive_PinArgvAndEnvFromBinding is the load-bearing proof of
// D4: a gateway-backed binding (the Host:direct + external BaseURL shape an
// endpoint-binding producer emits for an aggregator reached directly) reaches the interactive
// PTY child as BOTH the `--provider donmai --model <slug>` pin argv AND the
// DONMAI_PI_* pin env + the mirrored DONMAI_PI_KEY — because Spawn ran
// applyEndpoint BEFORE the interactive/headless split, so the interactive lane
// consumes the binding from birth. RED proof: remove the modelPinArgs(spec) call
// from interactiveArgs, or move applyEndpoint back inside launch, and the pin
// argv/env vanish from the child.
func TestSpawn_Interactive_PinArgvAndEnvFromBinding(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, captureArgvEnvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Model:       "should-lose-to-endpoint-model",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Endpoint: &agent.EndpointBinding{
			// The real gateway-pi cell: an aggregator reached DIRECTLY over the
			// openai-chat dialect surface, the model named by the full catalog
			// slug, the key riding the binding env.
			Company:  agent.CompanyAnthropic,
			Model:    "anthropic/claude-3-haiku",
			Protocol: agent.ProtoOpenAIChat,
			Host:     agent.HostDirect,
			BaseURL:  "https://ai-gateway.invalid/v1",
			Env:      map[string]string{"OPENAI_API_KEY": "gw-secret"},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	// Provider pin: --provider donmai --model <slug>, with the binding's model
	// winning over Spec.Model (applyEndpoint honored Endpoint.Model).
	provider, hasProvider := flagValue(argv, "--provider")
	if !hasProvider || provider != pinnedProviderName {
		t.Errorf("--provider = %q (present=%v), want %q; argv: %q", provider, hasProvider, pinnedProviderName, argv)
	}
	model, hasModel := flagValue(argv, "--model")
	if !hasModel || model != "anthropic/claude-3-haiku" {
		t.Errorf("--model = %q (present=%v), want the endpoint slug; argv: %q", model, hasModel, argv)
	}
	// Session-isolation posture: the embedded extension is loaded explicitly and
	// nothing else is discovered, so the "donmai" provider registration cannot be
	// shadowed.
	for _, want := range []string{"-e", "--no-extensions", "--session-dir"} {
		if !slices.Contains(argv, want) {
			t.Errorf("interactive argv missing %q; argv: %q", want, argv)
		}
	}

	env := readCapturedFile(t, workdir, "env.txt")
	for _, want := range []string{
		"DONMAI_PI_BASE_URL=https://ai-gateway.invalid/v1",
		"DONMAI_PI_API=openai-completions",
		"DONMAI_PI_MODEL=anthropic/claude-3-haiku",
		"DONMAI_PI_KEY=gw-secret",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("captured PTY-child env missing %q; got:\n%s", want, env)
		}
	}
	// The interactive spawn NEVER sets the handshake token — the extension's
	// RPC-mode handshake is skipped, so no UI artifact renders in the TUI.
	if !strings.Contains(env, "DONMAI_PI_HANDSHAKE=[]") {
		t.Errorf("interactive child unexpectedly carries a handshake token; got:\n%s", env)
	}
}

// TestSpawn_Interactive_NilBindingParity pins the nil-binding shape: with no
// endpoint resolved, the interactive argv carries a PLAIN --model (no
// --provider pin, so pi resolves its own provider) and the pin base URL is
// empty — byte-consistent with the design's "unbound session" case and never
// conjuring a gateway pin from nowhere.
func TestSpawn_Interactive_NilBindingParity(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, captureArgvEnvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Model:       "local-model",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		// Endpoint intentionally nil: no binding resolved.
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	if slices.Contains(argv, "--provider") {
		t.Errorf("nil-binding interactive argv must not pin a provider; argv: %q", argv)
	}
	model, hasModel := flagValue(argv, "--model")
	if !hasModel || model != "local-model" {
		t.Errorf("--model = %q (present=%v), want plain Spec.Model; argv: %q", model, hasModel, argv)
	}

	env := readCapturedFile(t, workdir, "env.txt")
	// The pin base URL var is present but EMPTY for an unbound session — never a
	// gateway URL conjured from nowhere.
	if !strings.Contains(env, "DONMAI_PI_BASE_URL=\n") {
		t.Errorf("nil-binding interactive child should carry an EMPTY pin base URL; got:\n%s", env)
	}
	if strings.Contains(env, "DONMAI_PI_BASE_URL=https") {
		t.Errorf("nil-binding interactive child carries a non-empty pin base URL; got:\n%s", env)
	}
	if !strings.Contains(env, "DONMAI_PI_HANDSHAKE=[]") {
		t.Errorf("interactive child unexpectedly carries a handshake token; got:\n%s", env)
	}
}

// TestSpawn_Interactive_DonmaiPiKeyMirrorSurvivesPreset proves the interactive
// lane inherits applyEndpoint's "mirror the cell key onto DONMAI_PI_KEY only if
// not already set" rule: a snapshot-provided DONMAI_PI_KEY on Spec.Env survives
// even when the binding env carries a DIFFERENT key. This is the credential
// precedence a cell-aware env fan-out relies on.
func TestSpawn_Interactive_DonmaiPiKeyMirrorSurvivesPreset(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, captureArgvEnvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         map[string]string{PiKeyEnvVar: "snapshot-key-wins"},
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyAnthropic,
			Model:    "anthropic/claude-3-haiku",
			Protocol: agent.ProtoOpenAIChat,
			Host:     agent.HostDirect,
			BaseURL:  "https://ai-gateway.invalid/v1",
			Env:      map[string]string{"OPENAI_API_KEY": "binding-key-loses"},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "env.txt")
	if !strings.Contains(env, "DONMAI_PI_KEY=snapshot-key-wins") {
		t.Errorf("pre-set snapshot DONMAI_PI_KEY did not survive the binding merge; got:\n%s", env)
	}
	if strings.Contains(env, "DONMAI_PI_KEY=binding-key-loses") {
		t.Errorf("binding key overwrote the snapshot key; got:\n%s", env)
	}
}

// TestInteractiveChildEnv_OmitsHandshakeTokenVsHeadless is the fast unit-level
// companion to the fixture tests: it pins the ONE difference between the
// interactive and headless child env at the seam. The headless composeChildEnv
// carries the per-session handshake token; the interactive interactiveChildEnv
// omits it (so the extension's RPC-mode handshake is skipped) while still
// carrying the provider pin. RED proof: add piHandshakeEnvVar to
// interactiveChildEnv and this test fails; the headless handshake gate would
// then wrongly appear reachable from a PTY child that has no RPC consumer.
func TestInteractiveChildEnv_OmitsHandshakeTokenVsHeadless(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	layout := newSessionLayout(cwd)
	spec := agent.Spec{
		Cwd: cwd,
		Env: map[string]string{PiKeyEnvVar: "k"},
		Endpoint: &agent.EndpointBinding{
			Model: "anthropic/claude-3-haiku", Protocol: agent.ProtoOpenAIChat,
			Host: agent.HostDirect, BaseURL: "https://ai-gateway.invalid/v1",
		},
	}

	// Headless carries the token; interactive omits it.
	headless := composeChildEnv(spec, layout, "sess-token")
	if !hasEnvVal(headless, piHandshakeEnvVar, "sess-token") {
		t.Fatalf("headless child env must carry the handshake token")
	}
	inter := interactiveChildEnv(spec, layout)
	if _, present := inter[piHandshakeEnvVar]; present {
		t.Errorf("interactive child env must NOT carry the handshake token; got %q", inter[piHandshakeEnvVar])
	}
	// The interactive env still carries the provider pin + config-home redirect.
	if inter[piBaseURLEnvVar] != "https://ai-gateway.invalid/v1" {
		t.Errorf("interactive child env missing the provider pin base URL: %v", inter)
	}
	if inter[PiKeyEnvVar] != "k" {
		t.Errorf("interactive child env dropped the resolved cell key: %v", inter)
	}
	if inter["PI_HOME"] != layout.root {
		t.Errorf("interactive child env missing the config-home redirect: %v", inter)
	}
}

// TestSpawn_Interactive_AdmissionFlipsDenyToAccept is the RED-first proof for the
// ValidateSpecCapabilities gate (agent/harness.go): before this cell that gate
// fail-closed any Spec.Interactive against pi (SupportsInteractivePTY defaulted
// false) with a typed SpecAdmissionError; now that pi declares the capability it
// ACCEPTS. RED proof: set SupportsInteractivePTY back to false in manifest.go and
// this assertion (and the whole interactive spawn path) fails closed again — the
// exact behavior TestPrepareHarnessRejectsUnsupportedFlatSpecCapabilities pins
// for a non-PTY harness.
func TestSpawn_Interactive_AdmissionFlipsDenyToAccept(t *testing.T) {
	t.Parallel()
	if err := agent.ValidateSpecCapabilities(agent.Spec{Interactive: &agent.InteractiveSpec{}}, (&Provider{}).Manifest()); err != nil {
		t.Fatalf("interactive spec must be admitted now that pi declares SupportsInteractivePTY; got %v", err)
	}
}

// TestInteractiveProfiles_TellCoarseTruthNoInjectedBoundary pins the D6-truthful
// manifest declarations for the interactive profiles: coarse PTY lifecycle +
// terminal-cast replay, and — critically — NO pi_handshake_policy_extension claim
// (the human at the terminal + pi's native approval UI is the authority in PTY
// mode). The headless profile's injected-boundary claim is unchanged, proving
// the interactive profile does not inherit headless evidence.
func TestInteractiveProfiles_TellCoarseTruthNoInjectedBoundary(t *testing.T) {
	t.Parallel()
	m := (&Provider{}).Manifest()

	tl, ok := m.ToolLifecycleProfile(agent.PromptModeHumanControlled)
	if !ok {
		t.Fatal("pi manifest declares no interactive tool-lifecycle profile")
	}
	if tl.LifecycleDelivery != agent.ToolDeliveryCoarsePTYEvents || tl.LifecycleFidelity != agent.EvidenceCoarse {
		t.Errorf("interactive lifecycle = %q/%q, want coarse PTY events", tl.LifecycleDelivery, tl.LifecycleFidelity)
	}
	if tl.ReplayDelivery != agent.ToolDeliveryTerminalCastReplay || tl.ReplayFidelity != agent.EvidenceCoarse {
		t.Errorf("interactive replay = %q/%q, want coarse terminal-cast replay", tl.ReplayDelivery, tl.ReplayFidelity)
	}
	if tl.NativeToolPolicyDelivery != agent.ToolDeliveryUnsupported || tl.PermissionConfigDelivery != agent.ToolDeliveryUnsupported {
		t.Errorf("interactive profile must NOT claim an injected policy boundary; got native=%q permission=%q", tl.NativeToolPolicyDelivery, tl.PermissionConfigDelivery)
	}

	// The headless profile keeps its injected-boundary claim — the interactive
	// profile did not inherit it, and did not weaken it.
	head, ok := m.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("pi manifest lost its headless tool-lifecycle profile")
	}
	if head.NativeToolPolicyDelivery != agent.ToolDeliveryPiInjectedBoundary {
		t.Errorf("headless native policy = %q, want the injected boundary intact", head.NativeToolPolicyDelivery)
	}

	pp, ok := m.PromptProfile(agent.PromptModeHumanControlled)
	if !ok {
		t.Fatal("pi manifest declares no interactive prompt-delivery profile")
	}
	if pp.UserDelivery != agent.PromptDeliveryPiPTYSeed || pp.ContextDelivery != agent.PromptDeliveryPiPTYSeed {
		t.Errorf("interactive prompt user/context delivery = %q/%q, want the PTY seed", pp.UserDelivery, pp.ContextDelivery)
	}
	if pp.SystemDelivery != agent.PromptDeliveryPiSystemAppend {
		t.Errorf("interactive system delivery = %q, want pi's append-system flag", pp.SystemDelivery)
	}
}

// TestSpawn_Interactive_MismatchedEndpointFailsLoudly mirrors the headless
// applyEndpoint contract on the interactive lane: a binding this harness cannot
// route must fail Spawn outright, never silently fall back to a default — proving
// errors surface truthfully on both spawn modes now that applyEndpoint runs
// ahead of the split for both.
func TestSpawn_Interactive_MismatchedEndpointFailsLoudly(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "unused-fails-before-exec"}
	_, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Endpoint:    &agent.EndpointBinding{Company: agent.CompanyAnthropic, Host: agent.HostAzure, Protocol: agent.ProtoAnthropicMessages},
	})
	if err == nil {
		t.Fatal("Spawn: want error for an unroutable endpoint host on the interactive lane, got nil")
	}
}

// TestSpawn_Interactive_StopTerminatesChildOrphanFree spawns a long-running fake
// pi under the PTY and proves Stop tears the child down: the events channel
// closes with a terminal event and no process is left behind. Uses a sleeping
// fake so the terminal event can only come from Stop, not from a fast exit.
func TestSpawn_Interactive_StopTerminatesChildOrphanFree(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, "sleep 60\n")

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After Stop the events channel must close promptly (the child was reaped).
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("events channel did not close after Stop — the PTY child may be orphaned")
		}
	}
}

// TestSpawn_Interactive_RealBinary_NoUIArtifact is the real-`pi` evidence that
// the embedded extension renders NO handshake UI artifact in TUI mode and that
// bare `pi` actually accepts the interactive argv this package builds. It is
// gated on `pi`+`node` being on PATH (donmai's hosted CI installs neither — only
// donmai-smokes' does), so it SKIPS on most machines; treat it as local/manual
// real-binary evidence layered on the fixture tests above.
//
// NAMED RISK (documented in the PR): the bare-`pi` interactive argv surface
// (positional prompt seed + `--append-system-prompt` on the TUI, and that no
// `--mode` is the TUI selector) is unverified against the pinned binary on a
// machine without pi installed. This test is that verification when it runs.
func TestSpawn_Interactive_RealBinary_NoUIArtifact(t *testing.T) {
	realBinaryAvailable(t)
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	cast := filepath.Join(workdir, "session.cast")

	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New(real pi): %v", err)
	}
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24, RecordPath: cast},
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic, Model: "anthropic/claude-3-haiku",
			Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect,
			BaseURL: "https://ai-gateway.invalid/v1",
			Env:     map[string]string{"OPENAI_API_KEY": "gw-secret"},
		},
	})
	if err != nil {
		t.Fatalf("real pi interactive Spawn rejected the argv: %v", err)
	}
	// Let the TUI reach session_start (where a mis-gated extension would render
	// the handshake round-trip), then tear down.
	time.Sleep(2 * time.Second)
	_ = h.Stop(context.Background())
	awaitPTYExit(t, h)

	if b, err := os.ReadFile(cast); err == nil {
		if strings.Contains(string(b), donmaiUIMarker) {
			t.Errorf("the policy extension rendered a UI round-trip (%q) in TUI mode — the RPC-mode gate leaked", donmaiUIMarker)
		}
	}
}
