package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestCopyEndpointBinding_PreservesUnitPrices pins that bound per-token
// prices survive the queued-work → Spec copy as a deep copy: dropping the
// UnitPrices branch would reach the harness with nil prices and the session
// would report cost-absent instead of the real cost.
func TestCopyEndpointBinding_PreservesUnitPrices(t *testing.T) {
	t.Parallel()
	in := &agent.EndpointBinding{
		Company:    agent.CompanyOpenAI,
		Model:      "model-a",
		BaseURL:    "https://gateway.example.test/v1",
		UnitPrices: &agent.UnitPrices{Input: 3, Output: 15, CacheRead: 0.75, CacheWrite: 1.25},
	}

	got := copyEndpointBinding(in)
	if got == nil {
		t.Fatal("copyEndpointBinding returned nil for non-nil input")
	}
	if got.UnitPrices == nil {
		t.Fatal("copyEndpointBinding dropped UnitPrices")
	}
	if got.UnitPrices == in.UnitPrices {
		t.Fatal("copyEndpointBinding aliases UnitPrices")
	}
	if *got.UnitPrices != *in.UnitPrices {
		t.Errorf("UnitPrices = %+v, want %+v", got.UnitPrices, in.UnitPrices)
	}

	// Deep-copy proof: mutating the copy must not touch the queued work.
	got.UnitPrices.Input = 999
	if in.UnitPrices.Input != 3 {
		t.Errorf("queued UnitPrices.Input mutated through copy: %v", in.UnitPrices.Input)
	}
}

// TestCopyEndpointBinding_NilUnitPricesStaysNil pins that unpriced bindings
// stay unpriced through the copy — nil must not materialize into a zero
// price table.
func TestCopyEndpointBinding_NilUnitPricesStaysNil(t *testing.T) {
	t.Parallel()
	got := copyEndpointBinding(&agent.EndpointBinding{Model: "model-a"})
	if got == nil {
		t.Fatal("copyEndpointBinding returned nil for non-nil input")
	}
	if got.UnitPrices != nil {
		t.Errorf("UnitPrices = %+v, want nil", got.UnitPrices)
	}
}

// TestReconciledEndpointBinding_PreservesUnitPrices pins that bound prices
// survive preflight reconciliation into the resolved profile.
func TestReconciledEndpointBinding_PreservesUnitPrices(t *testing.T) {
	t.Parallel()
	in := &agent.EndpointBinding{
		Company:    agent.CompanyOpenAI,
		Model:      "model-a",
		BaseURL:    "https://gateway.example.test/v1",
		UnitPrices: &agent.UnitPrices{Input: 3, Output: 15},
	}

	got, err := reconciledEndpointBinding(in)
	if err != nil {
		t.Fatalf("reconciledEndpointBinding: %v", err)
	}
	if got.UnitPrices == nil {
		t.Fatal("reconciledEndpointBinding dropped UnitPrices")
	}
	if got.UnitPrices == in.UnitPrices {
		t.Fatal("reconciledEndpointBinding aliases UnitPrices")
	}
	if *got.UnitPrices != *in.UnitPrices {
		t.Errorf("UnitPrices = %+v, want %+v", got.UnitPrices, in.UnitPrices)
	}

	got.UnitPrices.Output = 999
	if in.UnitPrices.Output != 15 {
		t.Errorf("queued UnitPrices.Output mutated through reconciled copy: %v", in.UnitPrices.Output)
	}
}

// TestReconciledEndpointBinding_NilUnitPricesStaysNil pins that unpriced
// bindings stay unpriced through reconciliation.
func TestReconciledEndpointBinding_NilUnitPricesStaysNil(t *testing.T) {
	t.Parallel()
	got, err := reconciledEndpointBinding(&agent.EndpointBinding{
		Company: agent.CompanyOpenAI,
		Model:   "model-a",
		BaseURL: "https://gateway.example.test/v1",
	})
	if err != nil {
		t.Fatalf("reconciledEndpointBinding: %v", err)
	}
	if got.UnitPrices != nil {
		t.Errorf("UnitPrices = %+v, want nil", got.UnitPrices)
	}
}

// TestTranslateSpec_PreservesUnitPrices drives the production entry point:
// bound prices on the queued work must reach Spec.Endpoint as a deep copy.
func TestTranslateSpec_PreservesUnitPrices(t *testing.T) {
	t.Parallel()
	endpoint := &agent.EndpointBinding{
		Model:      "model-a",
		UnitPrices: &agent.UnitPrices{Input: 3, Output: 15},
	}
	spec := translateSpec(QueuedWork{ResolvedProfile: ResolvedProfile{Endpoint: endpoint}}, agent.Capabilities{}, SpecInputs{})
	if spec.Endpoint == nil || spec.Endpoint.UnitPrices == nil {
		t.Fatal("Spec.Endpoint.UnitPrices missing for priced binding")
	}
	if *spec.Endpoint.UnitPrices != *endpoint.UnitPrices {
		t.Errorf("Spec UnitPrices = %+v, want %+v", spec.Endpoint.UnitPrices, endpoint.UnitPrices)
	}
	spec.Endpoint.UnitPrices.Input = 999
	if endpoint.UnitPrices.Input != 3 {
		t.Errorf("queued UnitPrices.Input mutated through Spec: %v", endpoint.UnitPrices.Input)
	}
}

// TestReconcileResolvedProfile_PreservesUnitPrices drives the production
// entry point: bound prices on the resolved-profile wire JSON must reach
// the reconciled queued work.
func TestReconcileResolvedProfile_PreservesUnitPrices(t *testing.T) {
	t.Parallel()
	rp := marshalFixture(t, map[string]any{
		"provider": "pi",
		"model":    "m",
		"endpoint": map[string]any{
			"company": "openai", "model": "m",
			"unitPrices": map[string]any{"input": 3, "output": 15},
		},
	})
	qw, err := ReconcileResolvedProfile(QueuedWork{}, nil, rp)
	if err != nil {
		t.Fatalf("ReconcileResolvedProfile: %v", err)
	}
	if qw.ResolvedProfile.Endpoint == nil || qw.ResolvedProfile.Endpoint.UnitPrices == nil {
		t.Fatal("reconciled Endpoint.UnitPrices missing for priced binding")
	}
	want := agent.UnitPrices{Input: 3, Output: 15}
	if *qw.ResolvedProfile.Endpoint.UnitPrices != want {
		t.Errorf("UnitPrices = %+v, want %+v", qw.ResolvedProfile.Endpoint.UnitPrices, want)
	}
}

// TestCopyEndpointBinding_PreservesRoutingPreference pins that a bound
// routing preference survives the queued-work → Spec copy as a deep copy:
// dropping it would reach the harness with no preference and the session
// would run unpinned instead of routed.
func TestCopyEndpointBinding_PreservesRoutingPreference(t *testing.T) {
	t.Parallel()
	in := &agent.EndpointBinding{
		Company:           agent.CompanyOpenAI,
		Model:             "model-a",
		RoutingPreference: &agent.RoutingPreference{Order: []string{"anthropic", "openai"}, Only: []string{"anthropic"}},
	}

	got := copyEndpointBinding(in)
	if got == nil {
		t.Fatal("copyEndpointBinding returned nil for non-nil input")
	}
	if got.RoutingPreference == nil {
		t.Fatal("copyEndpointBinding dropped RoutingPreference")
	}
	if got.RoutingPreference == in.RoutingPreference {
		t.Fatal("copyEndpointBinding aliases RoutingPreference")
	}
	if len(got.RoutingPreference.Order) != 2 || len(got.RoutingPreference.Only) != 1 {
		t.Errorf("RoutingPreference = %+v, want order x2 + only x1", got.RoutingPreference)
	}

	// Deep-copy proof down to the slug slices: mutating the copy must not
	// touch the queued work.
	got.RoutingPreference.Order[0] = "mutated"
	got.RoutingPreference.Only = append(got.RoutingPreference.Only, "mutated")
	if in.RoutingPreference.Order[0] != "anthropic" || len(in.RoutingPreference.Only) != 1 {
		t.Errorf("queued RoutingPreference mutated through copy: %+v", in.RoutingPreference)
	}
}

// TestCopyEndpointBinding_NilRoutingPreferenceStaysNil pins that
// unpreferred bindings stay unpreferred through the copy.
func TestCopyEndpointBinding_NilRoutingPreferenceStaysNil(t *testing.T) {
	t.Parallel()
	got := copyEndpointBinding(&agent.EndpointBinding{Model: "model-a"})
	if got == nil {
		t.Fatal("copyEndpointBinding returned nil for non-nil input")
	}
	if got.RoutingPreference != nil {
		t.Errorf("RoutingPreference = %+v, want nil", got.RoutingPreference)
	}
}

// TestReconciledEndpointBinding_PreservesRoutingPreference pins that a
// bound preference survives preflight reconciliation into the resolved
// profile.
func TestReconciledEndpointBinding_PreservesRoutingPreference(t *testing.T) {
	t.Parallel()
	in := &agent.EndpointBinding{
		Company:           agent.CompanyOpenAI,
		Model:             "model-a",
		BaseURL:           "https://gateway.example.test/v1",
		RoutingPreference: &agent.RoutingPreference{Order: []string{"anthropic"}},
	}

	got, err := reconciledEndpointBinding(in)
	if err != nil {
		t.Fatalf("reconciledEndpointBinding: %v", err)
	}
	if got.RoutingPreference == nil {
		t.Fatal("reconciledEndpointBinding dropped RoutingPreference")
	}
	if got.RoutingPreference == in.RoutingPreference {
		t.Fatal("reconciledEndpointBinding aliases RoutingPreference")
	}
	if len(got.RoutingPreference.Order) != 1 || len(got.RoutingPreference.Only) != 0 {
		t.Errorf("RoutingPreference = %+v, want order x1", got.RoutingPreference)
	}

	got.RoutingPreference.Order[0] = "mutated"
	if in.RoutingPreference.Order[0] != "anthropic" {
		t.Errorf("queued RoutingPreference mutated through reconciled copy: %+v", in.RoutingPreference)
	}
}

// TestReconcileResolvedProfile_PreservesRoutingPreference drives the
// production entry point: a preference on the resolved-profile wire JSON
// must reach the reconciled queued work.
func TestReconcileResolvedProfile_PreservesRoutingPreference(t *testing.T) {
	t.Parallel()
	rp := marshalFixture(t, map[string]any{
		"provider": "pi",
		"model":    "m",
		"endpoint": map[string]any{
			"company": "openai", "model": "m",
			"routingPreference": map[string]any{"order": []string{"anthropic", "openai"}, "only": []string{"anthropic"}},
		},
	})
	qw, err := ReconcileResolvedProfile(QueuedWork{}, nil, rp)
	if err != nil {
		t.Fatalf("ReconcileResolvedProfile: %v", err)
	}
	pref := qw.ResolvedProfile.Endpoint.RoutingPreference
	if pref == nil {
		t.Fatal("reconciled Endpoint.RoutingPreference missing for preferred binding")
	}
	if len(pref.Order) != 2 || len(pref.Only) != 1 {
		t.Errorf("RoutingPreference = %+v, want order x2 + only x1", pref)
	}
}

// TestOperationalPayload_BindsRoutingPreference pins that a set preference
// joins the canonical operational document (so preflight-to-spawn drift in
// it denies), while an unset preference leaves the document byte-identical
// to its pre-field shape (the golden architecture digest still holds).
func TestOperationalPayload_BindsRoutingPreference(t *testing.T) {
	t.Parallel()
	base := QueuedWork{
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderPi,
			Model:    "model-a",
			Endpoint: &agent.EndpointBinding{Company: agent.CompanyOpenAI, Model: "model-a"},
		},
	}
	base.SessionID = "session_routing_pref_digest"
	plain, err := CanonicalOperationalPayload(base)
	if err != nil {
		t.Fatalf("CanonicalOperationalPayload: %v", err)
	}
	if strings.Contains(string(plain), "routingPreference") {
		t.Fatalf("unset preference leaked into the canonical document: %s", plain)
	}

	preferred := base
	preferred.ResolvedProfile.Endpoint = &agent.EndpointBinding{
		Company:           agent.CompanyOpenAI,
		Model:             "model-a",
		RoutingPreference: &agent.RoutingPreference{Order: []string{"anthropic"}},
	}
	withPref, err := CanonicalOperationalPayload(preferred)
	if err != nil {
		t.Fatalf("CanonicalOperationalPayload: %v", err)
	}
	if !strings.Contains(string(withPref), `"routingPreference":{"only":null,"order":["anthropic"]}`) {
		t.Fatalf("set preference missing from the canonical document: %s", withPref)
	}
	if string(plain) == string(withPref) {
		t.Error("canonical document identical with and without a set preference: drift would go undetected")
	}
}
