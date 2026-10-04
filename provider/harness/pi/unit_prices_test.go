package pi

import (
	"math"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func pricedSpec(prices *agent.UnitPrices) agent.Spec {
	return agent.Spec{
		Prompt: "hi",
		Model:  "claude-x",
		Endpoint: &agent.EndpointBinding{
			Company:    agent.CompanyAnthropic,
			BaseURL:    "http://127.0.0.1:4000/v1",
			Host:       agent.HostGateway,
			Protocol:   agent.ProtoOpenAIChat,
			Model:      "claude-x",
			UnitPrices: prices,
		},
	}
}

// TestUnitPricePinEnv_BoundPricesExported pins the runner-to-extension price
// contract: a binding that carries per-token prices exports the bound flag
// plus all four rates, so the extension registers them as the model's cost
// table and pi computes the real per-turn cost.
func TestUnitPricePinEnv_BoundPricesExported(t *testing.T) {
	t.Parallel()
	env := providerPinEnv(pricedSpec(&agent.UnitPrices{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}))
	for _, want := range []string{
		piPricesBoundEnvVar + "=1",
		piPriceInputEnvVar + "=3",
		piPriceOutputEnvVar + "=15",
		piPriceCacheReadEnvVar + "=0.3",
		piPriceCacheWriteEnvVar + "=3.75",
	} {
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pin env missing %q: %v", want, env)
		}
	}
}

// TestUnitPricePinEnv_UnboundPricesExportNothing pins the other half: with no
// prices bound the runner exports no price entry at all — not even the bound
// flag — so the extension registers its zero table and the mapper suppresses
// the reported cost (absent, not $0). An explicit all-zero binding still
// counts as bound.
func TestUnitPricePinEnv_UnboundPricesExportNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prices *agent.UnitPrices
		bound  bool
	}{
		{name: "nil binding is unbound", prices: nil},
		{name: "negative rate is unbound", prices: &agent.UnitPrices{Input: -1}},
		{name: "NaN rate is unbound", prices: &agent.UnitPrices{Input: math.NaN()}},
		{name: "infinite rate is unbound", prices: &agent.UnitPrices{Output: math.Inf(1)}},
		{name: "explicit zero is bound", prices: &agent.UnitPrices{}, bound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := providerPinEnv(pricedSpec(tc.prices))
			hasPriceKey := false
			for _, e := range env {
				if strings.HasPrefix(e, piPricesBoundEnvVar+"=") ||
					strings.HasPrefix(e, piPriceInputEnvVar+"=") ||
					strings.HasPrefix(e, piPriceOutputEnvVar+"=") ||
					strings.HasPrefix(e, piPriceCacheReadEnvVar+"=") ||
					strings.HasPrefix(e, piPriceCacheWriteEnvVar+"=") {
					hasPriceKey = true
				}
			}
			if hasPriceKey != tc.bound {
				t.Errorf("price keys present = %v, want bound = %v (env %v)", hasPriceKey, tc.bound, env)
			}
		})
	}
}

// TestExtensionRegistersBoundPrices drives the REAL production extension (no
// pi binary) and reads back the registered model's cost table. Bound prices
// register verbatim; unbound prices register the zero table pi requires (the
// Go mapper suppresses the reported cost on that lane).
func TestExtensionRegistersBoundPrices(t *testing.T) {
	t.Parallel()
	bound := runExtensionFixture(t, []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
		piPricesBoundEnvVar + "=1",
		piPriceInputEnvVar + "=3",
		piPriceOutputEnvVar + "=15",
		piPriceCacheReadEnvVar + "=0.3",
		piPriceCacheWriteEnvVar + "=3.75",
	}, "read", `{"path":"README.md"}`)
	if cost, _ := registeredDonmaiModel(t, bound)["cost"].(map[string]any); cost["input"] != float64(3) || cost["output"] != float64(15) || cost["cacheRead"] != float64(0.3) || cost["cacheWrite"] != float64(3.75) {
		t.Errorf("registered cost = %v, want the bound prices", cost)
	}

	unbound := runExtensionFixture(t, []string{
		piBaseURLEnvVar + "=http://127.0.0.1:9/v1",
		piModelEnvVar + "=served-model",
	}, "read", `{"path":"README.md"}`)
	if cost, _ := registeredDonmaiModel(t, unbound)["cost"].(map[string]any); cost["input"] != float64(0) || cost["output"] != float64(0) || cost["cacheRead"] != float64(0) || cost["cacheWrite"] != float64(0) {
		t.Errorf("unbound registered cost = %v, want the zero table", cost)
	}
}

// TestExtensionRegistersBoundPrices_RedProof documents the revert-RED check:
// with the hard-coded zero cost restored, the bound-price registration above
// goes red (the extension ignores the exported prices).
func TestExtensionRegistersBoundPrices_RedProof(t *testing.T) {
	t.Parallel()
	src := string(extensionSource())
	if !strings.Contains(src, piPricesBoundEnvVar) || !strings.Contains(src, piPriceInputEnvVar) {
		t.Errorf("embedded extension does not read the exported price pin (%s/%s)", piPricesBoundEnvVar, piPriceInputEnvVar)
	}
}

// TestSuppressInjectedCost pins which lanes suppress the reported cost:
// exactly the injected provider with no valid prices bound. Bound prices
// (even explicit zero) and native lanes report unchanged.
func TestSuppressInjectedCost(t *testing.T) {
	t.Parallel()
	native := pricedSpec(nil)
	native.Endpoint = nil
	native.Model = "anthropic/claude-x"
	for _, tc := range []struct {
		name  string
		spec  agent.Spec
		suppr bool
	}{
		{name: "unpriced injected lane suppresses", spec: pricedSpec(nil), suppr: true},
		{name: "invalid prices suppress", spec: pricedSpec(&agent.UnitPrices{Input: -1}), suppr: true},
		{name: "bound prices report", spec: pricedSpec(&agent.UnitPrices{Input: 3, Output: 15})},
		{name: "explicit zero reports", spec: pricedSpec(&agent.UnitPrices{})},
		{name: "native lane reports", spec: native},
		{name: "no endpoint reports", spec: agent.Spec{Model: "claude-x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := suppressInjectedCost(tc.spec); got != tc.suppr {
				t.Errorf("suppressInjectedCost = %v, want %v", got, tc.suppr)
			}
		})
	}
}

// TestMapperSuppressesUnpricedCost drives the production mapEvent entry
// point: on an unpriced injected lane a reported zero cost reads as absent
// (no per-turn observed cost, no terminal totals) while token counts still
// accumulate; a priced lane keeps the real cost.
func TestMapperSuppressesUnpricedCost(t *testing.T) {
	t.Parallel()
	turn := func(st *mapperState, total float64) agent.LlmCallEvent {
		t.Helper()
		events, terminal := mapEvent(rawEvent{
			Type: "turn_end",
			Fields: map[string]any{"message": map[string]any{
				"provider": "fixture-surface", "model": "fixture-model",
				"usage": map[string]any{"input": float64(100), "output": float64(50), "cost": map[string]any{"total": total}},
			}},
		}, st)
		if terminal || len(events) != 1 {
			t.Fatalf("turn mapping: terminal=%v events=%d", terminal, len(events))
		}
		call, ok := events[0].(agent.LlmCallEvent)
		if !ok || !call.TurnCompleted {
			t.Fatalf("native completed turn missing: %#v", events[0])
		}
		return call
	}
	settle := func(st *mapperState) agent.ResultEvent {
		t.Helper()
		events, terminal := mapEvent(rawEvent{Type: "agent_settled", Fields: map[string]any{}}, st)
		if !terminal || len(events) != 1 {
			t.Fatalf("settled mapping: terminal=%v events=%d", terminal, len(events))
		}
		result, ok := events[0].(agent.ResultEvent)
		if !ok {
			t.Fatalf("terminal event %T, want ResultEvent", events[0])
		}
		return result
	}

	suppressed := &mapperState{suppressCost: true}
	if call := turn(suppressed, 0); call.ObservedCostUsd != nil {
		t.Errorf("suppressed per-turn observed cost = %v, want nil", *call.ObservedCostUsd)
	}
	res := settle(suppressed)
	if res.ObservedCostUsd != nil {
		t.Errorf("suppressed terminal observed cost = %v, want nil", *res.ObservedCostUsd)
	}
	if res.Cost == nil {
		t.Fatalf("suppressed terminal cost is nil, want token counts kept")
	}
	if res.Cost.InputTokens != 100 || res.Cost.OutputTokens != 50 {
		t.Errorf("suppressed terminal tokens = %+v, want 100 in / 50 out", res.Cost)
	}
	if res.Cost.TotalCostUsd != 0 {
		t.Errorf("suppressed terminal total = %v, want omitted (0)", res.Cost.TotalCostUsd)
	}
	if res.ObservedTurns == nil || *res.ObservedTurns != 1 {
		t.Errorf("suppressed terminal turns = %v, want 1", res.ObservedTurns)
	}

	priced := &mapperState{}
	if call := turn(priced, 0.042); call.ObservedCostUsd == nil || *call.ObservedCostUsd != 0.042 {
		t.Errorf("priced per-turn observed cost = %+v, want 0.042", call.ObservedCostUsd)
	}
	if res := settle(priced); res.ObservedCostUsd == nil || *res.ObservedCostUsd != 0.042 || res.Cost == nil || res.Cost.TotalCostUsd != 0.042 {
		t.Errorf("priced terminal cost lost: %+v", res)
	}
}
