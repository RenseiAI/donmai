package runner

import (
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
