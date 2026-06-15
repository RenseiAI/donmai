package matrix

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"

	endpAnthropic "github.com/RenseiAI/donmai/provider/endpoint/anthropic"
	endpGoogle "github.com/RenseiAI/donmai/provider/endpoint/google"
	endpLocal "github.com/RenseiAI/donmai/provider/endpoint/local"
	endpOpenAI "github.com/RenseiAI/donmai/provider/endpoint/openai"
	endpStub "github.com/RenseiAI/donmai/provider/endpoint/stub"
)

// endpointProvidersForParity returns the 5 model-endpoint providers as
// constructed instances for the bridge-adapter rule. Each New() is state-free.
func endpointProvidersForParity() []agent.ModelEndpointProvider {
	return []agent.ModelEndpointProvider{
		endpAnthropic.New(),
		endpOpenAI.New(),
		endpGoogle.New(),
		endpLocal.New(),
		endpStub.New(),
	}
}

// This is the B1 base-contract parity gate (donmai-architecture/
// ADR-2026-06-14-provider-base-contract-go-native.md + 002 §"The base
// interface"). It asserts that EVERY shipped provider manifest — across BOTH
// freezing axes (harness + model-endpoint) — embeds the SDK base contract:
// each manifest satisfies agent.BaseManifest and its projected ProviderBase
// header is well-formed (correct apiVersion, a known 9-family discriminant, a
// valid scope, a known stability tier). This is what makes the contract a
// freeze: a new axis manifest that forgets the base projection fails here.
//
// Run target (GOWORK=off so the org workspace does not resolve a non-worktree
// tree):
//
//	GOWORK=off go test -race ./matrix/...

// TestBaseParity_EveryHarnessManifestEmbedsBase asserts each of the shipped
// harness manifests embeds the base contract and projects a well-formed header.
func TestBaseParity_EveryHarnessManifestEmbedsBase(t *testing.T) {
	harvest := HarnessHarvestList()
	if len(harvest) == 0 {
		t.Fatalf("no harness manifests harvested")
	}
	for _, h := range harvest {
		mf := h.Manifest()
		// BaseManifest membership is a compile-time guarantee for the concrete
		// type; assert it dynamically too so a future manifest that drops the
		// projection fails loudly rather than silently.
		var bm agent.BaseManifest = mf
		assertWellFormedBase(t, "harness:"+string(mf.Name), bm)

		if got := agent.ManifestFamily(bm); got != agent.FamilyHarness {
			t.Errorf("harness %q: ManifestFamily=%q, want %q (the harness family discriminant)",
				mf.Name, got, agent.FamilyHarness)
		}
		// The base ID must be the within-family identifier (the HarnessName).
		if got := bm.Base().ID; got != string(mf.Name) {
			t.Errorf("harness %q: base ID=%q, want the within-family HarnessName %q", mf.Name, got, mf.Name)
		}
	}
}

// TestBaseParity_EveryEndpointManifestEmbedsBase asserts each of the shipped
// model-endpoint manifests embeds the base contract and projects a well-formed
// header.
func TestBaseParity_EveryEndpointManifestEmbedsBase(t *testing.T) {
	harvest := EndpointHarvestList()
	if len(harvest) == 0 {
		t.Fatalf("no endpoint manifests harvested")
	}
	for _, e := range harvest {
		mf := e.Manifest()
		var bm agent.BaseManifest = mf
		assertWellFormedBase(t, "endpoint:"+string(mf.Company), bm)

		if got := agent.ManifestFamily(bm); got != agent.FamilyModelEndpoint {
			t.Errorf("endpoint %q: ManifestFamily=%q, want %q (the model-endpoint family discriminant)",
				mf.Company, got, agent.FamilyModelEndpoint)
		}
		if got := bm.Base().ID; got != string(mf.Company) {
			t.Errorf("endpoint %q: base ID=%q, want the within-family Company %q", mf.Company, got, mf.Company)
		}
	}
}

// TestBaseParity_AxisProvidersAdaptToBaseProvider asserts the bridge adapters
// turn each shipped axis provider into a BaseProvider exposing the
// family-agnostic header, and that the base lifecycle is idempotent (002:
// activate/deactivate idempotent) + Health is the stub-by-default ready verdict.
// This proves both freezing axes formally extend the base lifecycle, not just
// the base manifest.
//
// NOTE on harness lifecycle: most harness providers are harvested as
// zero-value instances for state-free Manifest() reads (see HarnessHarvestList)
// and were never constructed via New(); driving Deactivate→Shutdown on those
// would tear down un-initialized internal state. So the FULL idempotent
// lifecycle assertion runs against the stub harness (constructed via New()) and
// every endpoint (pure, no process); for the other harness adapters we assert
// the family-agnostic header projects correctly through the bridge.
func TestBaseParity_AxisProvidersAdaptToBaseProvider(t *testing.T) {
	for _, hp := range HarnessProvidersForParity() {
		bp := agent.BaseProviderFromHarness(hp)
		if got := agent.ManifestFamily(bp); got != agent.FamilyHarness {
			t.Errorf("harness adapter %q: family=%q, want %q", hp.Manifest().Name, got, agent.FamilyHarness)
		}
		if got := bp.Base().APIVersion; got != agent.ProviderAPIVersion {
			t.Errorf("harness adapter %q: apiVersion=%q, want %q", hp.Manifest().Name, got, agent.ProviderAPIVersion)
		}
		// The stub harness is the only one safely constructed via New(), so it
		// is the one we drive the full idempotent lifecycle through.
		if hp.Manifest().Name == agent.HarnessStub {
			assertIdempotentLifecycle(t, "harness:stub", bp)
		}
	}

	for _, ep := range endpointProvidersForParity() {
		bp := agent.BaseProviderFromEndpoint(ep)
		assertIdempotentLifecycle(t, "endpoint:"+string(ep.Manifest().Company), bp)
		if got := agent.ManifestFamily(bp); got != agent.FamilyModelEndpoint {
			t.Errorf("endpoint adapter %q: family=%q, want %q", ep.Manifest().Company, got, agent.FamilyModelEndpoint)
		}
	}
}

// assertWellFormedBase checks the projected ProviderBase header satisfies the
// base contract's invariants (002 §Manifest / §"Scope resolution" / §A).
func assertWellFormedBase(t *testing.T, label string, bm agent.BaseManifest) {
	t.Helper()
	base := bm.Base()

	if base.APIVersion != agent.ProviderAPIVersion {
		t.Errorf("%s: apiVersion=%q, want %q", label, base.APIVersion, agent.ProviderAPIVersion)
	}
	if !agent.IsKnownProviderFamily(base.Family) {
		t.Errorf("%s: family %q is not one of the 9 known families", label, base.Family)
	}
	if base.ID == "" {
		t.Errorf("%s: base ID is empty (must be globally unique within the family)", label)
	}
	if err := agent.ValidateScope(agent.ManifestScope(bm)); err != nil {
		t.Errorf("%s: scope invalid: %v", label, err)
	}
	switch agent.ManifestStability(bm) {
	case agent.StabilityStable, agent.StabilityBeta, agent.StabilityUnstable, agent.StabilityRegistrationOnly:
	default:
		t.Errorf("%s: stability %q is not a known tier", label, agent.ManifestStability(bm))
	}
}

// assertIdempotentLifecycle checks the base lifecycle is safe to call twice
// (002: activate/deactivate are idempotent; deactivate must not error on a
// second call) and Health is the always-ready verdict.
func assertIdempotentLifecycle(t *testing.T, label string, bp agent.BaseProvider) {
	t.Helper()
	ctx := t.Context()

	if err := bp.Activate(ctx); err != nil {
		t.Errorf("%s: first Activate: %v", label, err)
	}
	if err := bp.Activate(ctx); err != nil {
		t.Errorf("%s: second Activate (must be idempotent): %v", label, err)
	}
	if err := bp.Deactivate(ctx); err != nil {
		t.Errorf("%s: first Deactivate: %v", label, err)
	}
	if err := bp.Deactivate(ctx); err != nil {
		t.Errorf("%s: second Deactivate (must not error on second call): %v", label, err)
	}
	h, err := bp.Health(ctx)
	if err != nil {
		t.Errorf("%s: Health: %v", label, err)
	}
	if h.Status != agent.HealthReady {
		t.Errorf("%s: Health status=%q, want %q (stub-by-default)", label, h.Status, agent.HealthReady)
	}
}
