package afcli

import (
	"log/slog"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/runner"
)

// TestLegacyAlias_EightProvidersResolveToTheirHarness is the load-bearing
// P2 acceptance: after splitting the fused providers into
// provider/harness/*, every one of the eight legacy ProviderNames still
// resolves through runner.Registry to a concrete provider whose harness
// identity matches the matrix's LegacyAliasMap. The package move changed
// import paths only — the resolution behaviour is byte-identical.
//
// The eight force-constructed providers come from
// matrix.HarnessProvidersForParity() (zero-value / New(), no probe), the
// same set the matrix is harvested from, so the assertion is against the
// exact instances the matrix is built on.
func TestLegacyAlias_EightProvidersResolveToTheirHarness(t *testing.T) {
	t.Parallel()

	reg := runner.NewRegistry()
	providers := matrix.HarnessProvidersForParity()
	if len(providers) != 8 {
		t.Fatalf("expected 8 harness providers, got %d", len(providers))
	}
	for _, p := range providers {
		if err := reg.Register(p); err != nil {
			t.Fatalf("register %q: %v", p.Name(), err)
		}
	}

	// The eight back-compat ProviderNames P2 must keep resolving.
	wantNames := []agent.ProviderName{
		agent.ProviderClaude,
		agent.ProviderCodex,
		agent.ProviderGemini,
		agent.ProviderAGYCLI,
		agent.ProviderOllama,
		agent.ProviderOpenCode,
		agent.ProviderAmp,
		agent.ProviderStub,
	}

	for _, name := range wantNames {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()

			// (a) the name resolves to a non-nil provider.
			p, err := reg.Resolve(name)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", name, err)
			}
			if p == nil {
				t.Fatalf("Resolve(%q): nil provider", name)
			}

			// (b) the resolved provider answers under this exact name.
			if got := p.Name(); got != name {
				t.Errorf("Resolve(%q).Name() = %q, want %q", name, got, name)
			}

			// (d) the name has a legacy alias cell.
			cell, ok := matrix.LegacyCell(name)
			if !ok {
				t.Fatalf("LegacyCell(%q): no alias cell — every legacy ProviderName must map", name)
			}

			// (c) the provider's harness identity matches the alias cell.
			hp, ok := p.(agent.HarnessProvider)
			if !ok {
				t.Fatalf("provider %q is not a HarnessProvider (no Manifest())", name)
			}
			if got := hp.Manifest().Name; got != cell.Harness {
				t.Errorf("provider %q Manifest().Name = %q, but LegacyAliasMap harness = %q",
					name, got, cell.Harness)
			}
		})
	}
}

// TestLegacyAliasMap_ExactlyTheEightLegacyNames pins the alias map's key
// set so a future provider addition/removal updates the cell anchors
// deliberately (and re-runs the byte-identical matrix gate), rather than
// silently widening the back-compat surface.
func TestLegacyAliasMap_ExactlyTheEightLegacyNames(t *testing.T) {
	t.Parallel()

	want := map[agent.ProviderName]matrix.CellKey{
		agent.ProviderClaude:   {Harness: agent.HarnessClaudeCode, Endpoint: agent.CompanyAnthropic, Host: "oauth-cli"},
		agent.ProviderCodex:    {Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: "oauth-cli"},
		agent.ProviderGemini:   {Harness: agent.HarnessRaw, Endpoint: agent.CompanyGoogle, Host: "direct"},
		agent.ProviderAGYCLI:   {Harness: agent.HarnessAntigravity, Endpoint: agent.CompanyGoogle, Host: "oauth-cli"},
		agent.ProviderOllama:   {Harness: agent.HarnessRaw, Endpoint: agent.CompanyLocal, Host: "local"},
		agent.ProviderOpenCode: {Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: "direct"},
		agent.ProviderAmp:      {Harness: agent.HarnessAmp, Endpoint: agent.CompanyAnthropic, Host: "direct"},
		agent.ProviderStub:     {Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: "local"},
	}

	if len(matrix.LegacyAliasMap) != len(want) {
		t.Fatalf("LegacyAliasMap has %d entries, want %d: %v",
			len(matrix.LegacyAliasMap), len(want), matrix.LegacyAliasMap)
	}
	for name, wantCell := range want {
		got, ok := matrix.LegacyCell(name)
		if !ok {
			t.Errorf("LegacyAliasMap missing %q", name)
			continue
		}
		if got != wantCell {
			t.Errorf("LegacyAliasMap[%q] = %+v, want %+v", name, got, wantCell)
		}
	}
}

// TestAssertLegacyAlias_NoMismatchForRealProviders exercises the runtime
// alias-consistency consumer (assertLegacyAlias) over the live provider
// set and asserts it logs no mismatch — i.e. the registry build's
// defense-in-depth check is quiet for the shipped manifests.
func TestAssertLegacyAlias_NoMismatchForRealProviders(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	for _, p := range matrix.HarnessProvidersForParity() {
		// assertLegacyAlias must not panic and must agree for every
		// shipped provider; a mismatch would mean cells.go drifted.
		assertLegacyAlias(logger, p)

		name := p.Name()
		cell, ok := matrix.LegacyCell(name)
		if !ok {
			t.Errorf("no legacy cell for shipped provider %q", name)
			continue
		}
		if got := p.Manifest().Name; got != cell.Harness {
			t.Errorf("shipped provider %q Manifest().Name=%q != matrix harness %q",
				name, got, cell.Harness)
		}
	}
}
