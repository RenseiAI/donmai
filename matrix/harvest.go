package matrix

import (
	"github.com/RenseiAI/donmai/agent"

	endpAnthropic "github.com/RenseiAI/donmai/provider/endpoint/anthropic"
	endpGoogle "github.com/RenseiAI/donmai/provider/endpoint/google"
	endpLocal "github.com/RenseiAI/donmai/provider/endpoint/local"
	endpOpenAI "github.com/RenseiAI/donmai/provider/endpoint/openai"
	endpStub "github.com/RenseiAI/donmai/provider/endpoint/stub"
	"github.com/RenseiAI/donmai/provider/harness/agycli"
	"github.com/RenseiAI/donmai/provider/harness/amp"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/provider/harness/gemini"
	"github.com/RenseiAI/donmai/provider/harness/ollama"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/shell"
	stubprov "github.com/RenseiAI/donmai/provider/harness/stub"
)

// HarnessHarvestList is the harvest list of the 9 harness providers. Each entry
// returns the harness manifest from a constructed instance WITHOUT relying on
// probe state:
//   - The eight real providers expose a state-free Manifest() on (*Provider),
//     so a zero-value struct is sufficient and no New() probe runs.
//   - The stub provider's Manifest() projects p.caps; it is harvested via
//     New() so it picks up defaultCapabilities() (all-on) per §2.8.
//
// This is also THE registry P8's future harnesses (Vertex/Bedrock/Azure/
// OpenRouter/Fireworks/Groq) join by flipping HarnessCaps.SupportsInteractivePTY
// on their own Manifest() — nothing anywhere derives the interactive-capable
// set from a hardcoded {claude, codex, shell} literal; callers filter this
// list (or matrix.CapabilityMatrix.Harnesses, its rendered projection) on
// that field instead (see provider/harness/ptycli's registry-driven matrix
// test).
//
// A slice (not a map) so iteration — and generated output — is deterministic.
func HarnessHarvestList() []HarnessHarvest {
	return []HarnessHarvest{
		{Name: agent.HarnessClaudeCode, Manifest: func() agent.HarnessManifest { return (&claude.Provider{}).Manifest() }},
		{Name: agent.HarnessCodex, Manifest: func() agent.HarnessManifest { return (&codex.Provider{}).Manifest() }},
		{Name: agent.HarnessRaw, Manifest: func() agent.HarnessManifest { return (&gemini.Provider{}).Manifest() }},
		{Name: agent.HarnessAntigravity, Manifest: func() agent.HarnessManifest { return (&agycli.Provider{}).Manifest() }},
		{Name: agent.HarnessRaw, Manifest: func() agent.HarnessManifest { return (&ollama.Provider{}).Manifest() }},
		{Name: agent.HarnessOpenCode, Manifest: func() agent.HarnessManifest { return (&opencode.Provider{}).Manifest() }},
		{Name: agent.HarnessPi, Manifest: func() agent.HarnessManifest { return (&pi.Provider{}).Manifest() }},
		{Name: agent.HarnessAmp, Manifest: func() agent.HarnessManifest { return (&amp.Provider{}).Manifest() }},
		{Name: agent.HarnessShell, Manifest: func() agent.HarnessManifest { return (&shell.Provider{}).Manifest() }},
		{Name: agent.HarnessStub, Manifest: stubManifest},
	}
}

// stubManifest harvests the stub harness manifest via New() so p.caps carries
// defaultCapabilities() (all-on) per §2.8. stubprov.New() never returns an
// error and always returns a non-nil *provider that satisfies HarnessProvider;
// the type assertion is the only branch worth guarding (a fallback keeps the
// generator robust if the stub's interface surface ever changes).
func stubManifest() agent.HarnessManifest {
	p, _ := stubprov.New()
	if hp, ok := p.(agent.HarnessProvider); ok {
		return hp.Manifest()
	}
	return agent.HarnessManifest{Name: agent.HarnessStub, Family: agent.FamilyHarness, ContractABI: "harness/v2"}
}

// EndpointHarvestList is the harvest list of the 5 model-endpoint providers.
// Each Manifest() is state-free; constructed via New() for clarity.
func EndpointHarvestList() []EndpointHarvest {
	return []EndpointHarvest{
		{Company: agent.CompanyAnthropic, Manifest: func() agent.ModelEndpointManifest { return endpAnthropic.New().Manifest() }},
		{Company: agent.CompanyOpenAI, Manifest: func() agent.ModelEndpointManifest { return endpOpenAI.New().Manifest() }},
		{Company: agent.CompanyGoogle, Manifest: func() agent.ModelEndpointManifest { return endpGoogle.New().Manifest() }},
		{Company: agent.CompanyLocal, Manifest: func() agent.ModelEndpointManifest { return endpLocal.New().Manifest() }},
		{Company: agent.CompanyStub, Manifest: func() agent.ModelEndpointManifest { return endpStub.New().Manifest() }},
	}
}

// HarnessProvidersForParity returns the 9 harness providers as constructed
// instances for the parity test's "manifest agrees with Capabilities()" rule.
// Each is the same zero/New instance used for harvesting, so the comparison is
// against the exact value the matrix is built from.
func HarnessProvidersForParity() []agent.HarnessProvider {
	stub, _ := stubprov.New()
	stubHP, _ := stub.(agent.HarnessProvider)
	return []agent.HarnessProvider{
		&claude.Provider{},
		&codex.Provider{},
		&gemini.Provider{},
		&agycli.Provider{},
		&ollama.Provider{},
		&opencode.Provider{},
		&pi.Provider{},
		&amp.Provider{},
		&shell.Provider{},
		stubHP,
	}
}
