package pi

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// realCatalogListing is real `pi --list-models zai` output (captured against
// the pinned-adjacent @earendil-works/pi-coding-agent binary with a ZAI_API_KEY
// configured — see builtin_providers.go's provenance comment), used as a
// scripted catalogProbeFunc payload so catalogHasModel's parsing is proven
// against a REAL table shape, not a hand-typed approximation.
const realCatalogListing = `provider  model              context  max-out  thinking  images
zai       glm-4.7            204.8K   131.1K   yes       no
zai       glm-5-turbo        200K     131.1K   yes       no
zai       glm-5.2            1M       131.1K   yes       no
zai       glm-5.2-highspeed  1M       131.1K   yes       no
zai       glm-5.3            1M       131.1K   yes       no
`

// TestCatalogHasModel is the table-driven parser proof: pi's --list-models
// applies fuzzy search, so catalogHasModel must accept only an EXACT
// (provider, model) column match and reject everything else — the header
// row, a fuzzy near-miss, and pi's own "no match" message.
func TestCatalogHasModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		raw      string
		provider string
		model    string
		want     bool
	}{
		{name: "exact match against real output", raw: realCatalogListing, provider: "zai", model: "glm-5.3", want: true},
		{name: "sibling row does not match a different model", raw: realCatalogListing, provider: "zai", model: "glm-5.2", want: true},
		{name: "unknown model absent from real output", raw: realCatalogListing, provider: "zai", model: "glm-9.9-does-not-exist", want: false},
		{name: "wrong provider does not match", raw: realCatalogListing, provider: "xai", model: "glm-5.3", want: false},
		{name: "header row never matches", raw: "provider  model  context  max-out  thinking  images\n", provider: "provider", model: "model", want: false},
		{name: "pi's own no-match message", raw: `No models matching "zai/glm-9.9-does-not-exist"` + "\n", provider: "zai", model: "glm-9.9-does-not-exist", want: false},
		{name: "empty raw output", raw: "", provider: "zai", model: "glm-5.3", want: false},
		{
			name: "fuzzy near-miss row (partial column text) must not count as exact",
			raw:  "provider  model\nzai       glm-5.30-beta\n", provider: "zai", model: "glm-5.3", want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := catalogHasModel(tc.raw, tc.provider, tc.model); got != tc.want {
				t.Errorf("catalogHasModel(%q, %q, %q) = %v, want %v", tc.raw, tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

// TestPreflightCatalogCheck_ConfirmedPresencePasses is the GREEN half: a
// probe that returns a real, matching listing must not deny spawn.
func TestPreflightCatalogCheck_ConfirmedPresencePasses(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi"}
	probe := func(_ context.Context, _, _, _, _, _ string) (string, error) {
		return realCatalogListing, nil
	}
	if err := p.preflightCatalogCheck(context.Background(), probe, "zai", "glm-5.3", "ZAI_API_KEY", "k"); err != nil {
		t.Errorf("preflightCatalogCheck(known model) = %v, want nil", err)
	}
}

// TestPreflightCatalogCheck_ConfirmedAbsenceDenies is requirement 2's core
// proof: a probe that RAN and returned a real listing with no matching row
// must deny spawn with a clear, actionable error wrapping
// agent.ErrSpawnFailed — this is exactly the "spawn succeeds, first turn
// 400s" failure shape caught before any child spawns instead.
func TestPreflightCatalogCheck_ConfirmedAbsenceDenies(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi"}
	probe := func(_ context.Context, _, _, _, _, _ string) (string, error) {
		return realCatalogListing, nil
	}
	err := p.preflightCatalogCheck(context.Background(), probe, "zai", "glm-9.9-does-not-exist", "ZAI_API_KEY", "k")
	if err == nil {
		t.Fatal("preflightCatalogCheck(unknown model) = nil, want a denial")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("preflightCatalogCheck error does not wrap agent.ErrSpawnFailed: %v", err)
	}
	for _, want := range []string{"zai", "glm-9.9-does-not-exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflightCatalogCheck error %q missing %q — must name the exact unresolved pair", err.Error(), want)
		}
	}
}

// TestPreflightCatalogCheck_ProbeErrorIsNotFatal mirrors DEC-2's
// version-probe precedent (probe.go checkVersionPin): a probe that could not
// even RUN (binary unresolvable, offline, timeout) is unverifiable, not a
// confirmed absence — it must not block a spawn that might otherwise have
// succeeded.
func TestPreflightCatalogCheck_ProbeErrorIsNotFatal(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi"}
	probe := func(_ context.Context, _, _, _, _, _ string) (string, error) {
		return "", errors.New("exec: pi: not found")
	}
	if err := p.preflightCatalogCheck(context.Background(), probe, "zai", "glm-5.3", "ZAI_API_KEY", "k"); err != nil {
		t.Errorf("preflightCatalogCheck(probe error) = %v, want nil (unverifiable, not fatal)", err)
	}
}

// TestPreflightCatalogCheck_PassesResolvedCredentialToProbe proves the
// credential-threading fix: pi's own --list-models only lists a provider's
// models once it sees a configured credential for it (verified against the
// pinned-adjacent binary — catalog_preflight.go's doc comment), so the
// preflight must hand the probe the SAME provider-native credential
// applyEndpoint resolved, not leave it to inherit an empty ambient env. RED
// proof: drop the credEnvVar/credEnvValue arguments from
// preflightCatalogCheck's probe call and this test fails, because the fake
// probe below would then receive empty strings for a real Env.
func TestPreflightCatalogCheck_PassesResolvedCredentialToProbe(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi"}
	var gotVar, gotValue string
	probe := func(_ context.Context, _, _, _, credEnvVar, credEnvValue string) (string, error) {
		gotVar, gotValue = credEnvVar, credEnvValue
		return realCatalogListing, nil
	}
	if err := p.preflightCatalogCheck(context.Background(), probe, "zai", "glm-5.3", "ZAI_API_KEY", "resolved-key"); err != nil {
		t.Fatalf("preflightCatalogCheck: %v", err)
	}
	if gotVar != "ZAI_API_KEY" || gotValue != "resolved-key" {
		t.Errorf("probe received (%q, %q), want (%q, %q)", gotVar, gotValue, "ZAI_API_KEY", "resolved-key")
	}
}

// TestResolveCatalogProbe pins the selection rule: an explicit
// Options.CatalogProbe always wins; a Provider that never resolved a real
// binary through New() (realBinary false — skipProcess:true, OR the
// bare-struct-literal test-fixture pattern several other tests in this
// package use, e.g. newFakeInteractivePiProvider) has nothing trustworthy to
// shell out to and skips (nil probe); only a Provider New() actually
// resolved a real pi binary for (realBinary true) defaults to the real exec
// probe.
//
// The bare-struct-literal case is the load-bearing one: gating on
// !opts.skipProcess alone (rather than !realBinary) would shell out to
// whatever binary field a fixture happens to set — see
// Provider.realBinary's doc comment. RED proof: change resolveCatalogProbe
// back to checking p.opts.skipProcess and this test's
// "bare struct literal (fake-binary fixture pattern)" case fails.
func TestResolveCatalogProbe(t *testing.T) {
	t.Parallel()
	fake := func(_ context.Context, _, _, _, _, _ string) (string, error) {
		return "", nil
	}

	skipNoOverride := &Provider{opts: Options{skipProcess: true}}
	if probe := skipNoOverride.resolveCatalogProbe(); probe != nil {
		t.Errorf("skipProcess with no override should skip the preflight (nil probe)")
	}

	skipWithOverride := &Provider{opts: Options{skipProcess: true, CatalogProbe: fake}}
	if probe := skipWithOverride.resolveCatalogProbe(); probe == nil {
		t.Errorf("an explicit CatalogProbe override must win even under skipProcess")
	}

	bareStructLiteral := &Provider{binary: "/tmp/some-fake-pi-fixture-script"}
	if probe := bareStructLiteral.resolveCatalogProbe(); probe != nil {
		t.Errorf("bare struct literal (fake-binary fixture pattern) must skip the preflight — it has opts.skipProcess == false (the zero value) but no real binary")
	}

	realProvider := &Provider{realBinary: true}
	if probe := realProvider.resolveCatalogProbe(); probe == nil {
		t.Errorf("a Provider New() actually resolved a real binary for must default to the real exec probe, not skip")
	}
}

// TestSpawn_NativeRouting_CatalogPreflightDeniesUnknownModel is the Spawn-
// level end-to-end proof of requirement 2: a native builtin-provider pin
// whose (provider, model) pair the injected CatalogProbe reports as ABSENT
// denies Spawn itself — before any child process, before the handshake gate,
// before the prompt ever reaches a wire. RED proof: comment out the
// preflightCatalogCheck call in prepare() (pi.go) and this test fails,
// because Spawn would then proceed to the (never-scripted) stdin/stdout and
// hang/fail for an unrelated reason instead of returning THIS denial.
func TestSpawn_NativeRouting_CatalogPreflightDeniesUnknownModel(t *testing.T) {
	t.Parallel()
	p, err := New(Options{
		skipProcess: true,
		CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return realCatalogListing, nil // ran successfully; the model just isn't in it
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Cwd:    t.TempDir(),
		Model:  "zai/glm-9.9-does-not-exist",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Host: agent.HostDirect,
			BaseURL: "https://api.z.ai/api/coding/paas/v4", Protocol: agent.ProtoOpenAIChat,
			Env: map[string]string{"OPENAI_API_KEY": "k"},
		},
	})
	if err == nil {
		t.Fatal("Spawn succeeded for a model the catalog preflight confirmed absent")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("Spawn error does not wrap agent.ErrSpawnFailed: %v", err)
	}
}

// TestSpawn_NativeRouting_CatalogPreflightAllowsKnownModel is
// PreflightDeniesUnknownModel's GREEN sibling: the SAME native-routing pin
// shape, but the injected CatalogProbe confirms the pair IS present, so
// Spawn proceeds past the preflight into the ordinary handshake+prompt flow
// and succeeds.
func TestSpawn_NativeRouting_CatalogPreflightAllowsKnownModel(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    &syncBuffer{},
		stdoutOverride:   pr,
		handshakeToken:   testHandshakeToken,
		HandshakeTimeout: 2 * time.Second,
		CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return realCatalogListing, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := handshakeEvent("h1") + getStateResponse("ses_native") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	go func() { _, _ = io.WriteString(pw, body) }()

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt: "hi",
		Cwd:    t.TempDir(),
		Model:  "zai/glm-5.3",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Host: agent.HostDirect,
			BaseURL: "https://api.z.ai/api/coding/paas/v4", Protocol: agent.ProtoOpenAIChat,
			Env: map[string]string{"OPENAI_API_KEY": "k"},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	drain(t, h)
}

// newFakeCatalogProbeBinary writes an executable bash script standing in for
// `pi` and returns its path — the same fixture pattern
// newFakeInteractivePiProvider (interactive_test.go) uses, scoped here to a
// bare binary path since defaultCatalogProbe takes one directly (no
// *Provider needed).
func newFakeCatalogProbeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary fixture is unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o755); err != nil { //nolint:gosec // test fixture needs the exec bit
		t.Fatal(err)
	}
	return bin
}

// ambientCredentialAwareFakePi mimics the load-bearing half of pi's OWN
// documented credential resolution: an entry under $PI_CODING_AGENT_DIR
// stands in for auth.json (docs/providers.md: "Auth file credentials take
// priority over environment variables"). It reports the pinned zai/glm-5.3
// pair present IFF it was handed BOTH a real, isolated agent directory (one
// that exists but carries no ambient credential file) AND the ZAI_API_KEY
// env var — echoing the received PI_CODING_AGENT_DIR on its own marker line
// so the test can assert isolation (non-empty, exists, fresh per call)
// independently of the credential check.
const ambientCredentialAwareFakePi = `
echo "AGENTDIR:$PI_CODING_AGENT_DIR"
if [ -n "$PI_CODING_AGENT_DIR" ] && [ -d "$PI_CODING_AGENT_DIR" ] && [ -f "$PI_CODING_AGENT_DIR/auth.json" ]; then
  echo "provider  model"
  echo "zai       glm-5.3"
elif [ -n "$ZAI_API_KEY" ]; then
  echo "provider  model"
  echo "zai       glm-5.3"
else
  echo 'No models matching "zai/glm-5.3"'
fi
`

// TestDefaultCatalogProbe_AmbientCredentialCannotSatisfyPreflight is the
// regression proof for review finding [HIGH] (catalog_preflight.go): before
// the fix, defaultCatalogProbe let the child inherit whatever
// PI_CODING_AGENT_DIR the daemon process happened to have (unset ⇒ pi's own
// default, ~/.pi/agent), so an OPERATOR'S PERSONAL LOGIN for the same
// provider — an ambient auth.json entry entirely unrelated to this cell —
// would satisfy pi's own auth check regardless of the cell's OWN BYOK
// credential. The fixture above models exactly that resolution order (an
// auth-file-shaped entry under the agent dir wins over the env var); this
// test proves the isolated, freshly-created, per-call agent dir this
// package now sets NEVER carries that file, so presence hinges solely on
// the credential this function explicitly passed — never on anything
// ambient. RED proof: revert defaultCatalogProbe to not setting
// PI_CODING_AGENT_DIR at all (or to reusing a fixed/shared directory this
// test can pre-seed a fake auth.json into) and the "empty credential"
// case below starts reporting present.
func TestDefaultCatalogProbe_AmbientCredentialCannotSatisfyPreflight(t *testing.T) {
	t.Parallel()
	bin := newFakeCatalogProbeBinary(t, ambientCredentialAwareFakePi)

	// Empty credential: the isolated agent dir is real but carries no
	// auth.json (fresh MkdirTemp every call), so an ambient credential can
	// never satisfy this — the fake pi must report absent.
	raw, err := defaultCatalogProbe(context.Background(), bin, "zai", "glm-5.3", "ZAI_API_KEY", "")
	if err != nil {
		t.Fatalf("defaultCatalogProbe(no credential): %v", err)
	}
	if catalogHasModel(raw, "zai", "glm-5.3") {
		t.Errorf("an isolated agent dir with no credential reported the model present; ambient/leftover state must not satisfy the preflight: %q", raw)
	}
	agentDir1, ok := catalogProbeAgentDirMarker(raw)
	if !ok || agentDir1 == "" {
		t.Fatalf("fake pi did not receive a non-empty PI_CODING_AGENT_DIR: %q", raw)
	}

	// Real credential: same isolation, but now the explicit env var this
	// function set carries the resolved key — reports present.
	raw2, err := defaultCatalogProbe(context.Background(), bin, "zai", "glm-5.3", "ZAI_API_KEY", "resolved-cell-key")
	if err != nil {
		t.Fatalf("defaultCatalogProbe(with credential): %v", err)
	}
	if !catalogHasModel(raw2, "zai", "glm-5.3") {
		t.Errorf("a real credential on the explicit env var must satisfy the preflight: %q", raw2)
	}
	agentDir2, ok := catalogProbeAgentDirMarker(raw2)
	if !ok || agentDir2 == "" {
		t.Fatalf("fake pi did not receive a non-empty PI_CODING_AGENT_DIR: %q", raw2)
	}

	// Fresh per call — proves this is a throwaway directory minted for this
	// exact probe, never a shared/reused location a prior call's (or a real
	// session's) state could bleed into.
	if agentDir1 == agentDir2 {
		t.Errorf("PI_CODING_AGENT_DIR was reused across calls (%q); each probe must get its own throwaway directory", agentDir1)
	}

	// The throwaway directory is cleaned up after the call returns — it
	// must not accumulate on disk across a fleet's worth of preflights.
	if _, statErr := os.Stat(agentDir1); !os.IsNotExist(statErr) {
		t.Errorf("PI_CODING_AGENT_DIR %q still exists after defaultCatalogProbe returned (stat err: %v); it must be removed", agentDir1, statErr)
	}
}

// catalogProbeAgentDirMarker extracts the "AGENTDIR:<value>" line
// ambientCredentialAwareFakePi emits, so tests can assert on the exact
// PI_CODING_AGENT_DIR value the probe received.
func catalogProbeAgentDirMarker(raw string) (string, bool) {
	const prefix = "AGENTDIR:"
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix), true
		}
	}
	return "", false
}

// envDumpingFakePi dumps its ENTIRE received environment (one KEY=VALUE per
// line) so a test can assert on exactly what did and did not reach it.
const envDumpingFakePi = `env`

// TestDefaultCatalogProbe_BuildsMinimalEnv_BlocklistedVarNeverReachesChild is
// the regression proof for review finding [MEDIUM]: defaultCatalogProbe must
// build the child's env from an explicit, minimal base rather than
// `append(os.Environ(), …)`, which would hand the probe the daemon's FULL
// env — including any AgentEnvBlocklist-covered credential
// (runtime/env/composer.go) present in the parent process — bypassing the
// exact boundary composeChildEnv exists to enforce for a real spawn. RED
// proof: revert the env construction to `cmd.Env = append(os.Environ(), …)`
// and the blocklisted canary below starts appearing in the child's env.
func TestDefaultCatalogProbe_BuildsMinimalEnv_BlocklistedVarNeverReachesChild(t *testing.T) {
	// Not parallel: mutates process env (t.Setenv).
	const canary = "sk-host-canary-must-not-reach-probe"
	t.Setenv("ANTHROPIC_API_KEY", canary) // AgentEnvBlocklist-covered (runtime/env/composer.go)

	bin := newFakeCatalogProbeBinary(t, envDumpingFakePi)
	raw, err := defaultCatalogProbe(context.Background(), bin, "zai", "glm-5.3", "ZAI_API_KEY", "resolved-cell-key")
	if err != nil {
		t.Fatalf("defaultCatalogProbe: %v", err)
	}
	if strings.Contains(raw, canary) {
		t.Errorf("blocklisted host credential leaked into the catalog-preflight probe's env: %q", raw)
	}
	// Positive control: the minimal env this function DOES build must still
	// carry the explicit credential and the isolated agent dir — an
	// over-aggressively minimal env would silently break the preflight
	// instead of leaking a secret.
	if !strings.Contains(raw, "ZAI_API_KEY=resolved-cell-key") {
		t.Errorf("probe env missing the explicit credential var: %q", raw)
	}
	if !strings.Contains(raw, piCodingAgentDirEnvVar+"=") {
		t.Errorf("probe env missing %s: %q", piCodingAgentDirEnvVar, raw)
	}
}
