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

// TestUnitPricePinEnv_UnboundPricesExportClearing pins the other half: with
// no prices bound the runner exports explicit clearing entries (each price
// key present but empty, including the bound flag) — not an omission — so
// a DONMAI_PI_PRICE_* value inherited from the spawning environment cannot
// survive child-env layering as a stale price. The extension registers its
// zero table on this lane and the mapper suppresses the reported cost
// (absent, not $0). An explicit all-zero binding still counts as bound.
func TestUnitPricePinEnv_UnboundPricesExportClearing(t *testing.T) {
	t.Parallel()
	clearing := []string{
		piPricesBoundEnvVar + "=",
		piPriceInputEnvVar + "=",
		piPriceOutputEnvVar + "=",
		piPriceCacheReadEnvVar + "=",
		piPriceCacheWriteEnvVar + "=",
	}
	for _, tc := range []struct {
		name   string
		prices *agent.UnitPrices
		bound  bool
	}{
		{name: "nil binding clears", prices: nil},
		{name: "negative rate clears", prices: &agent.UnitPrices{Input: -1}},
		{name: "NaN rate clears", prices: &agent.UnitPrices{Input: math.NaN()}},
		{name: "infinite rate clears", prices: &agent.UnitPrices{Output: math.Inf(1)}},
		{name: "explicit zero is bound", prices: &agent.UnitPrices{}, bound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := providerPinEnv(pricedSpec(tc.prices))
			if !tc.bound {
				for _, want := range clearing {
					found := false
					for _, e := range env {
						if e == want {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("pin env missing clearing entry %q: %v", want, env)
					}
				}
				return
			}
			if !hasEnvVal(env, piPricesBoundEnvVar, "1") {
				t.Errorf("explicit-zero binding must export the bound flag: %v", env)
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

// lastPriceEnvValue returns the value of the LAST KEY= entry in a composed child
// env, matching exec.Cmd's last-entry-wins semantics: the pin this package
// appends last must win over any inherited copy of the same key.
func lastPriceEnvValue(env []string, key string) string {
	value := ""
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			value = e[len(key)+1:]
			found = true
		}
	}
	if !found {
		return "\x00absent"
	}
	return value
}

// TestComposeChildEnv_UnboundPricesClearInheritedStale drives the
// production composeChildEnv entry point: an inherited price binding from
// the spawning environment must not survive on an unpriced injected lane.
// The unbound pin's clearing entries ride last and win under
// last-entry-wins, so the extension sees an empty bound flag and registers
// no stale price. RED proof: return nil from unitPricePinEnv on the unbound
// path and the stale inherited values below reach the child.
func TestComposeChildEnv_UnboundPricesClearInheritedStale(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv(piPricesBoundEnvVar, "1")
	t.Setenv(piPriceInputEnvVar, "99")
	t.Setenv(piPriceOutputEnvVar, "99")
	t.Setenv(piPriceCacheReadEnvVar, "99")
	t.Setenv(piPriceCacheWriteEnvVar, "99")

	layout := newSessionLayout(t.TempDir())
	env := composeChildEnv(pricedSpec(nil), layout, "sess-token")
	for _, key := range []string{piPricesBoundEnvVar, piPriceInputEnvVar, piPriceOutputEnvVar, piPriceCacheReadEnvVar, piPriceCacheWriteEnvVar} {
		if got := lastPriceEnvValue(env, key); got != "" {
			t.Errorf("unbound child env %s = %q, want cleared (empty, last wins)", key, got)
		}
	}
}

// TestComposeChildEnv_BoundPricesOverrideInheritedStale drives the
// production composeChildEnv entry point for a priced binding: the bound
// rates ride last and win over any inherited copy, so the extension
// registers the real prices, not the stale host values.
func TestComposeChildEnv_BoundPricesOverrideInheritedStale(t *testing.T) {
	// Not parallel: mutates process env.
	t.Setenv(piPriceInputEnvVar, "99")
	t.Setenv(piPriceOutputEnvVar, "99")

	layout := newSessionLayout(t.TempDir())
	env := composeChildEnv(pricedSpec(&agent.UnitPrices{Input: 3, Output: 15}), layout, "sess-token")
	if got := lastPriceEnvValue(env, piPriceInputEnvVar); got != "3" {
		t.Errorf("bound child env %s = %q, want 3 (pin wins last)", piPriceInputEnvVar, got)
	}
	if got := lastPriceEnvValue(env, piPriceOutputEnvVar); got != "15" {
		t.Errorf("bound child env %s = %q, want 15 (pin wins last)", piPriceOutputEnvVar, got)
	}
	if got := lastPriceEnvValue(env, piPricesBoundEnvVar); got != "1" {
		t.Errorf("bound child env %s = %q, want 1", piPricesBoundEnvVar, got)
	}
}

// TestInteractiveChildEnv_UnboundPricesClearInheritedStale pins the same
// clearing on the interactive lane: interactiveChildEnv layers the pin as
// PTY overrides, so the stale inherited price must lose there too. RED
// proof: drop the price pin from interactiveChildEnv and the stale values
// below ride into the PTY child.
func TestInteractiveChildEnv_UnboundPricesClearInheritedStale(t *testing.T) {
	t.Parallel()
	// Seed the snapshot layer with stale prices: it must not rescue them.
	spec := pricedSpec(nil)
	spec.Env = map[string]string{
		piPricesBoundEnvVar:    "1",
		piPriceInputEnvVar:     "99",
		piPriceOutputEnvVar:    "99",
		piPriceCacheReadEnvVar: "99",
	}
	layout := newSessionLayout(t.TempDir())
	env := interactiveChildEnv(spec, layout)
	for _, key := range []string{piPricesBoundEnvVar, piPriceInputEnvVar, piPriceOutputEnvVar, piPriceCacheReadEnvVar, piPriceCacheWriteEnvVar} {
		if got, present := env[key]; !present || got != "" {
			t.Errorf("unbound interactive env %s = %q (present=%v), want cleared (empty)", key, got, present)
		}
	}
}

// TestInteractiveChildEnv_BoundPricesOverrideStale pins that a priced
// binding wins on the interactive lane too.
func TestInteractiveChildEnv_BoundPricesOverrideStale(t *testing.T) {
	t.Parallel()
	spec := pricedSpec(&agent.UnitPrices{Input: 3, Output: 15})
	spec.Env = map[string]string{piPriceInputEnvVar: "99"}
	env := interactiveChildEnv(spec, newSessionLayout(t.TempDir()))
	if env[piPriceInputEnvVar] != "3" || env[piPriceOutputEnvVar] != "15" || env[piPricesBoundEnvVar] != "1" {
		t.Errorf("bound interactive env kept stale prices: %v", env)
	}
}

// pricedTurnBody scripts one turn_end carrying fixture usage plus the
// terminal agent_settled, so a Spawn/Resume test can assert the emitted
// per-turn and terminal costs end to end through the production handle.
func pricedTurnBody(total float64) string {
	return getStateResponse("ses_prices") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "turn_end", "message": map[string]any{
			"provider": "fixture-surface", "model": "fixture-model",
			"usage": map[string]any{"input": float64(1000), "output": float64(500), "cost": map[string]any{"total": total}},
		}}) +
		event(map[string]any{"type": "agent_settled"})
}

// collectCosts drains one scripted session and returns the per-turn
// observed cost plus the terminal result event.
func collectCosts(t *testing.T, h agent.Handle) (*float64, agent.ResultEvent) {
	t.Helper()
	var perTurn *float64
	var terminal agent.ResultEvent
	for _, e := range drain(t, h) {
		switch ev := e.(type) {
		case agent.LlmCallEvent:
			perTurn = ev.ObservedCostUsd
		case agent.ResultEvent:
			terminal = ev
		}
	}
	return perTurn, terminal
}

// boundFixtureTotal is the cost pi reports for the fixture usage below
// when the bound prices register: (1000*3 + 500*15 + 200*0.3 +
// 100*3.75) / 1e6. cacheRead/cacheWrite token counts ride no fixture
// field, so they contribute only via the registered table on a real
// binary; the scripted cost here is carried verbatim in the turn event.
const boundFixtureTotal = 0.0105

// TestSpawn_BoundPricesYieldNonzeroCost drives the production Spawn entry
// point with a priced binding: the handle must not suppress the reported
// cost, so a fixture usage carrying the bound-computed total emits it on
// the per-turn event and the terminal result. RED proof: force
// suppressInjectedCost true (drop the price check) and the per-turn cost
// below reads nil.
func TestSpawn_BoundPricesYieldNonzeroCost(t *testing.T) {
	t.Parallel()
	spec := pricedSpec(&agent.UnitPrices{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75})
	_, h, err := spawnScripted(t, spec, handshakeEvent("h1"), pricedTurnBody(boundFixtureTotal))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	perTurn, terminal := collectCosts(t, h)
	if perTurn == nil || *perTurn != boundFixtureTotal {
		t.Errorf("bound per-turn observed cost = %+v, want %v", perTurn, boundFixtureTotal)
	}
	if terminal.ObservedCostUsd == nil || *terminal.ObservedCostUsd != boundFixtureTotal {
		t.Errorf("bound terminal observed cost = %+v, want %v", terminal.ObservedCostUsd, boundFixtureTotal)
	}
	if terminal.Cost == nil || terminal.Cost.TotalCostUsd != boundFixtureTotal {
		t.Errorf("bound terminal total = %+v, want %v", terminal.Cost, boundFixtureTotal)
	}
}

// TestSpawn_UnboundPricesYieldNoCost drives the production Spawn entry
// point with no prices bound: the reported zero cost must read as absent
// (nil per-turn observed cost, nil terminal observed cost, zero terminal
// total with token counts kept), not $0. RED proof: initialize the handle
// without the suppressCost flag and the zero total below is reported
// verbatim.
func TestSpawn_UnboundPricesYieldNoCost(t *testing.T) {
	t.Parallel()
	_, h, err := spawnScripted(t, pricedSpec(nil), handshakeEvent("h1"), pricedTurnBody(0))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	perTurn, terminal := collectCosts(t, h)
	if perTurn != nil {
		t.Errorf("unbound per-turn observed cost = %v, want nil (absent, not $0)", *perTurn)
	}
	if terminal.ObservedCostUsd != nil {
		t.Errorf("unbound terminal observed cost = %v, want nil", *terminal.ObservedCostUsd)
	}
	if terminal.Cost == nil {
		t.Fatalf("unbound terminal cost is nil, want token counts kept")
	}
	if terminal.Cost.TotalCostUsd != 0 {
		t.Errorf("unbound terminal total = %v, want omitted (0)", terminal.Cost.TotalCostUsd)
	}
	if terminal.Cost.InputTokens != 1000 || terminal.Cost.OutputTokens != 500 {
		t.Errorf("unbound terminal tokens = %+v, want 1000 in / 500 out", terminal.Cost)
	}
}

// TestResume_UnboundPricesYieldNoCost drives the production Resume entry
// point on the same unpriced lane: suppression is decided from the resumed
// spec, so a resumed session reads cost-absent too.
func TestResume_UnboundPricesYieldNoCost(t *testing.T) {
	t.Parallel()
	_, h, err := resumeScripted(t, "ses_prices", pricedSpec(nil), handshakeEvent("h1"), pricedTurnBody(0))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	perTurn, terminal := collectCosts(t, h)
	if perTurn != nil {
		t.Errorf("resumed unbound per-turn observed cost = %v, want nil", *perTurn)
	}
	if terminal.ObservedCostUsd != nil {
		t.Errorf("resumed unbound terminal observed cost = %v, want nil", *terminal.ObservedCostUsd)
	}
}
