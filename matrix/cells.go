// Package matrix is the single source of truth for the two-axis capability
// matrix: which (harness × model-endpoint × serving-host) cells are valid,
// which legacy ProviderName each maps to, and the generated artifacts that
// platform / rensei-tui consume. The matrix is grounded in the live
// provider/endpoint Manifest() declarations (harvested by the generator), not
// a re-typed copy — keeping the manifests the single SoT.
//
// This file holds the ONLY hand-authored capability data (per 03 §1):
//   - the harvest lists (which providers/endpoints to call Manifest() on),
//   - the valid-cell list (validCells),
//   - the denylist of known-bad (harness, endpoint, host) triples.
//
// The generator (matrix/gen) validates every hand-authored cell against the
// harvested manifests (protocol intersection, authMode subset, caps
// narrowing) and fails loudly on an invalid cell, then marshals the
// deterministic JSON artifacts + registry_gen.go.
package matrix

import (
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/opencode"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// SchemaVersion is the committed schema version of the generated artifacts.
//
// p1.0 -> p1.1 (07-design-opencode-spawn.md §8): additive — a new
// top-level `binaryPins` section. Existing consumers (platform's
// resolve-model.ts/providers.ts) ignore unknown keys, so this bump ships
// with no consumer break; it exists purely so a future schema reader can
// tell "binaryPins may be absent" (p1.0) from "binaryPins is present when
// non-empty" (p1.1) apart.
const SchemaVersion = "p1.1"

// ContractABI pins the matrix-document contract version (distinct from the
// per-manifest harness/v2 and model-endpoint/v1 ABIs).
const ContractABI = "capability-matrix/v1"

// GeneratedFrom names the SoT the artifacts are generated from. Stable string
// (NOT a timestamp — a timestamp would break the byte-identical parity gate).
const GeneratedFrom = "donmai/matrix (go generate ./matrix/...)"

// CellKey is the (harness, endpoint, host) triple that uniquely names a cell.
type CellKey struct {
	Harness  agent.HarnessName `json:"harness"`
	Endpoint agent.Company     `json:"endpoint"`
	Host     agent.ServingHost `json:"host"`
}

// CapsOverride is an optional per-cell NARROWING of the harness caps. Only the
// agent-loop bools may be narrowed (set false where the harness sets true); a
// cell may remove, never add, a capability. nil pointer == no override (the
// cell inherits the harness caps verbatim). The parity test asserts every
// override is narrowing-only.
type CapsOverride struct {
	SupportsMessageInjection *bool `json:"inject,omitempty"`
	SupportsSessionResume    *bool `json:"resume,omitempty"`
	SupportsToolPlugins      *bool `json:"tools,omitempty"`
	AcceptsMcpServerSpec     *bool `json:"mcp,omitempty"`
	AcceptsAllowedToolsList  *bool `json:"allowedToolsList,omitempty"`
	EmitsSubagentEvents      *bool `json:"subagents,omitempty"`
	SupportsReasoningEffort  *bool `json:"reasoningEffort,omitempty"`
}

// HarnessEndpointCell is one hand-authored valid cell — a (harness × endpoint
// × host) combination that the runtime may legally bind. The generator
// validates each against the harvested manifests before emitting it.
type HarnessEndpointCell struct {
	Harness        agent.HarnessName   `json:"harness"`
	Endpoint       agent.Company       `json:"endpoint"`
	Host           agent.ServingHost   `json:"host"`
	Protocol       agent.WireProtocol  `json:"protocol"`
	Transport      agent.TransportKind `json:"transport"`
	AuthModes      []agent.AuthMode    `json:"authModes"`
	BringsOwnAuth  bool                `json:"bringsOwnAuth"`
	NeedsAPIKey    bool                `json:"needsApiKey"`
	CostModel      agent.CostModel     `json:"costModel"`
	OneShot        bool                `json:"oneShot"`
	NativeJSONMode bool                `json:"nativeJsonMode"`
	StructuredVia  string              `json:"structuredVia"`
	Caps           *CapsOverride       `json:"caps,omitempty"`
	// LegacyProviderID is the back-compat ProviderName this cell anchors, or
	// nil when no single legacy id owns the cell (the north-star opencode×google
	// cell). Pointer so the JSON emits null, not "".
	LegacyProviderID *agent.ProviderName `json:"legacyProviderId"`
	Stability        string              `json:"stability"`
	Smoked           bool                `json:"smoked"`
}

// Key returns the cell's (harness, endpoint, host) identity.
func (c HarnessEndpointCell) Key() CellKey {
	return CellKey{Harness: c.Harness, Endpoint: c.Endpoint, Host: c.Host}
}

// pn is a tiny helper to take the address of a ProviderName literal for the
// nullable LegacyProviderID field.
func pn(p agent.ProviderName) *agent.ProviderName { return &p }

// boolp is a tiny helper to take the address of a bool literal for narrowing
// overrides (kept for hand-authoring per-cell caps narrowing; unused today).
func boolp(b bool) *bool { return &b }

// silence unused-helper lint while no cell currently narrows caps; boolp is
// part of the hand-authoring surface and exercised by the parity test.
var _ = boolp

// The harvest types (HarnessHarvest / EndpointHarvest) name the providers and
// endpoints the generator calls Manifest() on. The concrete harvest lists live
// in harvest.go (which imports every provider package). Held as slices (not
// maps) so iteration — and thus the generated output — is deterministic.

// HarnessHarvest names one harness provider to harvest.
type HarnessHarvest struct {
	// Name is the expected HarnessName (for diagnostics).
	Name agent.HarnessName
	// Manifest returns the harness manifest from a zero/new instance, never
	// touching probe state.
	Manifest func() agent.HarnessManifest
}

// EndpointHarvest names one model-endpoint provider to harvest.
type EndpointHarvest struct {
	Company  agent.Company
	Manifest func() agent.ModelEndpointManifest
}

// validCells is the authoritative hand-authored valid-cell list (§2.9 + the
// north-star cell). Order here is the emit order (the generator sorts
// deterministically before marshalling, so hand order does not affect output).
var validCells = []HarnessEndpointCell{
	// claude-code × Anthropic ------------------------------------------------
	{
		Harness: agent.HarnessClaudeCode, Endpoint: agent.CompanyAnthropic, Host: agent.HostOAuthCLI,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthHostSession}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostHostSubscription, OneShot: true, NativeJSONMode: false,
		StructuredVia: "spawn-collect", LegacyProviderID: pn(agent.ProviderClaude),
		Stability: "stable", Smoked: true,
	},
	{
		Harness: agent.HarnessClaudeCode, Endpoint: agent.CompanyAnthropic, Host: agent.HostDirect,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "forced-tool", LegacyProviderID: nil,
		Stability: "stable", Smoked: false,
	},
	{
		Harness: agent.HarnessClaudeCode, Endpoint: agent.CompanyAnthropic, Host: agent.HostBedrock,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "forced-tool", LegacyProviderID: nil,
		Stability: "beta", Smoked: false,
	},
	{
		Harness: agent.HarnessClaudeCode, Endpoint: agent.CompanyAnthropic, Host: agent.HostVertex,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "forced-tool", LegacyProviderID: nil,
		Stability: "beta", Smoked: false,
	},

	// codex × OpenAI ---------------------------------------------------------
	// All three cells ride the app-server harness (TransportSubprocessRPC),
	// which threads Spec.ResponseSchema as turn/start outputSchema — native
	// strict structured output ("output-schema").
	{
		Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: agent.HostOAuthCLI,
		Protocol: agent.ProtoOpenAIResponses, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthHostSession}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostHostSubscription, OneShot: true, NativeJSONMode: true,
		StructuredVia: "output-schema", LegacyProviderID: pn(agent.ProviderCodex),
		Stability: "stable", Smoked: true,
	},
	{
		Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "output-schema", LegacyProviderID: nil,
		Stability: "beta", Smoked: false,
	},
	{
		Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: agent.HostAzure,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "output-schema", LegacyProviderID: nil,
		Stability: "beta", Smoked: false,
	},

	// gemini-direct × Google -----------------------------------------------
	{
		Harness: agent.HarnessGeminiDirect, Endpoint: agent.CompanyGoogle, Host: agent.HostDirect,
		Protocol: agent.ProtoGeminiGenerate, Transport: agent.TransportDirectAPI,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "response-schema", LegacyProviderID: pn(agent.ProviderGemini),
		Stability: "stable", Smoked: true,
	},
	{
		Harness: agent.HarnessGeminiDirect, Endpoint: agent.CompanyGoogle, Host: agent.HostVertex,
		Protocol: agent.ProtoGeminiGenerate, Transport: agent.TransportDirectAPI,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "response-schema", LegacyProviderID: nil,
		Stability: "beta", Smoked: false,
	},

	// antigravity × Google (agy host login) ---------------------------------
	{
		Harness: agent.HarnessAntigravity, Endpoint: agent.CompanyGoogle, Host: agent.HostOAuthCLI,
		Protocol: agent.ProtoAntigravityOAuth, Transport: agent.TransportPTY,
		AuthModes: []agent.AuthMode{agent.AuthHostSession, agent.AuthLocal}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostHostSubscription, OneShot: true, NativeJSONMode: false,
		StructuredVia: "spawn-collect", LegacyProviderID: pn(agent.ProviderAGYCLI),
		Stability: "stable", Smoked: true,
	},

	// ollama × Local --------------------------------------------------------
	{
		Harness: agent.HarnessOllama, Endpoint: agent.CompanyLocal, Host: agent.HostLocal,
		Protocol: agent.ProtoOllama, Transport: agent.TransportDirectAPI,
		AuthModes: []agent.AuthMode{agent.AuthLocal}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostLocalFree, OneShot: true, NativeJSONMode: true,
		StructuredVia: "format-json", LegacyProviderID: pn(agent.ProviderOllama),
		Stability: "beta", Smoked: false,
	},

	// opencode × OpenAI (direct) — legacy default for ProviderName "opencode"
	{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "json-schema", LegacyProviderID: pn(agent.ProviderOpenCode),
		Stability: "beta", Smoked: true,
	},

	// opencode × OpenAI on the gateway (openai-chat surface) — the first
	// gateway cell (M1; ADR-2026-07-24 / 08 §9). opencode drives openai-chat and
	// the OpenAI gateway HostDesc PRESENTS openai-chat, so the D2 intersection
	// rule passes unchanged. Same-protocol in M1 (the gateway's cross-protocol
	// value proves out in M2). Smoked per DEC-2/DEC-3: donmai-smokes step19
	// gateway lane green 3 consecutive main runs (2026-07-25 → 2026-07-26).
	// Not a legacy anchor — the opencode default stays the direct cell above.
	{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyOpenAI, Host: agent.HostGateway,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: true,
		StructuredVia: "json-schema", LegacyProviderID: nil,
		Stability: "experimental", Smoked: true,
	},

	// opencode × Google on local (/v1 openai-chat) — THE NORTH-STAR CELL ----
	{
		Harness: agent.HarnessOpenCode, Endpoint: agent.CompanyGoogle, Host: agent.HostLocal,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthLocal}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostLocalFree, OneShot: true, NativeJSONMode: true,
		StructuredVia: "json-schema", LegacyProviderID: nil,
		Stability: "experimental", Smoked: false,
	},

	// pi × Anthropic (direct) — deliberately narrow (09 §3): pi's Drive
	// surface is the broadest in the fleet, but only two direct cells are
	// authored, both experimental/untested, until the donmai-smokes step20
	// gate accrues green history (DEC-2/DEC-3). Anchors ProviderName "pi".
	{
		Harness: agent.HarnessPi, Endpoint: agent.CompanyAnthropic, Host: agent.HostDirect,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "spawn-collect", LegacyProviderID: pn(agent.ProviderPi),
		Stability: "experimental", Smoked: false,
	},

	// pi × OpenAI (direct) --------------------------------------------------
	{
		Harness: agent.HarnessPi, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect,
		Protocol: agent.ProtoOpenAIChat, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthBYOK, agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "spawn-collect", LegacyProviderID: nil,
		Stability: "experimental", Smoked: false,
	},

	// amp × Anthropic (direct, metered — cost-honest, key-needing) ----------
	{
		Harness: agent.HarnessAmp, Endpoint: agent.CompanyAnthropic, Host: agent.HostDirect,
		Protocol: agent.ProtoAnthropicMessages, Transport: agent.TransportCLIInjection,
		AuthModes: []agent.AuthMode{agent.AuthMetered}, BringsOwnAuth: false, NeedsAPIKey: true,
		CostModel: agent.CostMeteredPerToken, OneShot: true, NativeJSONMode: false,
		StructuredVia: "spawn-collect", LegacyProviderID: pn(agent.ProviderAmp),
		Stability: "beta", Smoked: false,
	},

	// stub × stub -----------------------------------------------------------
	{
		Harness: agent.HarnessStub, Endpoint: agent.CompanyStub, Host: agent.HostLocal,
		Protocol: agent.ProtoStub, Transport: agent.TransportDirectAPI,
		AuthModes: []agent.AuthMode{agent.AuthLocal}, BringsOwnAuth: true, NeedsAPIKey: false,
		CostModel: agent.CostLocalFree, OneShot: true, NativeJSONMode: true,
		StructuredVia: "stub", LegacyProviderID: pn(agent.ProviderStub),
		Stability: "stable", Smoked: true,
	},
}

// denylist is the small set of known-bad (harness, endpoint, host) triples
// (per 02 OQ #3: protocol-intersection + a small denylist, not a full
// allowlist). Empty in P1 — the intersection rule + the hand-authored valid
// list already cover the legal surface; the denylist is reserved for future
// explicit exclusions and is emitted (as an array) into the matrix for
// completeness.
var denylist = []CellKey{}

// HarnessBinaryPin declares the version-pin metadata for a harness whose
// external binary drifts independently of donmai releases (opencode ships
// ~2 releases/day — 07-design-opencode-spawn.md §8). Net-new, additive
// field (SchemaVersion p1.0 -> p1.1): no version metadata existed anywhere
// in the generated matrix before this.
type HarnessBinaryPin struct {
	// MinVersion is the lowest version the harness's adapter is known to
	// work against; the adapter's probe.go fails construction below it.
	MinVersion string `json:"minVersion"`
	// PinnedVersion is the exact version CI installs and smoke-tests
	// against (donmai-smokes harness/<name>_install.go).
	PinnedVersion string `json:"pinnedVersion"`
	// VerifiedAgainst is the highest version anyone has actually run the
	// adapter against; above it the adapter proceeds but labels the
	// session unverified (DEC-2: label, don't block).
	VerifiedAgainst string `json:"verifiedAgainst"`
}

// BinaryPinRow is one row of the generated binaryPins section — a
// HarnessBinaryPin keyed by the harness it describes. A sorted slice (not
// a map) so the generated JSON is deterministic independent of map
// iteration order, matching every other generated section's convention.
type BinaryPinRow struct {
	Harness agent.HarnessName `json:"harness"`
	HarnessBinaryPin
}

// harnessBinaryPins is the hand-authored version-pin map (07 §8). Values
// are NOT re-typed literals: they read directly from the owning provider
// package's exported constants (e.g. opencode.MinVersion) so probe-time
// enforcement (provider/harness/opencode/probe.go) and this generated
// documentation can never drift apart from each other.
var harnessBinaryPins = map[agent.HarnessName]HarnessBinaryPin{
	agent.HarnessOpenCode: {
		MinVersion:      opencode.MinVersion,
		PinnedVersion:   opencode.PinnedVersion,
		VerifiedAgainst: opencode.VerifiedAgainst,
	},
	// pi ships multiple releases/day; the pin is enforced probe-time in
	// provider/harness/pi/probe.go. VerifiedAgainst == MinVersion encodes that
	// no pi version has been verified locally yet (pi was not installed on the
	// authoring host) — every probed binary is labeled unverified until the
	// donmai-smokes step20 lane exercises the pinned binary (09 §8).
	agent.HarnessPi: {
		MinVersion:      pi.MinVersion,
		PinnedVersion:   pi.PinnedVersion,
		VerifiedAgainst: pi.VerifiedAgainst,
	},
}

// ValidCells returns the hand-authored valid-cell list. Exported so the
// parity test (same package) and the generator can both read it.
func ValidCells() []HarnessEndpointCell { return validCells }

// Denylist returns the hand-authored denylist.
func Denylist() []CellKey { return denylist }
