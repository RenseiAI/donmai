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
