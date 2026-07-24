package agent

// This file declares the shared "binding currency" value types for the
// two-axis provider model (Phase 1). Verbatim from
// 02-two-axis-architecture.md §2.1. JSON tags are camelCase, wire-consistent
// with the existing types in this package.
//
// These types are PURELY ADDITIVE: nothing in the Spawn path reads them in
// Phase 1. They exist so the per-provider Manifest() literals (the harness
// DRIVE surface) and the per-company endpoint manifests (the SPEAK surface)
// share one vocabulary, and so the matrix/ generator can harvest a single
// source of truth.

// WireProtocol — on-the-wire request/response shape a model endpoint speaks.
// A harness declares which it can DRIVE; an endpoint declares which it SPEAKS.
type WireProtocol string

// WireProtocol constants name each on-the-wire request/response shape.
const (
	ProtoAnthropicMessages WireProtocol = "anthropic-messages"
	ProtoOpenAIChat        WireProtocol = "openai-chat"       // /v1/chat/completions (+ compat)
	ProtoOpenAIResponses   WireProtocol = "openai-responses"  // Codex app-server / Responses API
	ProtoGeminiGenerate    WireProtocol = "gemini-generate"   // :generateContent, SSE
	ProtoOllama            WireProtocol = "ollama"            // /api/chat NDJSON (bare surface)
	ProtoAntigravityOAuth  WireProtocol = "antigravity-oauth" // agy CLI host-login channel (pty)
	ProtoStub              WireProtocol = "stub"              // test-only sentinel protocol
)

// ServingHost — WHERE the model is served, orthogonal to the wire protocol.
type ServingHost string

// ServingHost constants name each place a model can be served.
const (
	HostDirect   ServingHost = "direct"
	HostBedrock  ServingHost = "bedrock"
	HostVertex   ServingHost = "vertex"
	HostAzure    ServingHost = "azure"
	HostLocal    ServingHost = "local"     // ollama / on-box
	HostOAuthCLI ServingHost = "oauth-cli" // host login (agy / claude-sub / codex-sub) — BringsOwnAuth
	// HostGateway is the translating-gateway serving host: a company's model
	// served from the local loopback gateway, which presents an existing wire
	// protocol and translates to the chosen upstream (ADR-2026-07-24). A cell
	// on this host is cross-protocol-legal iff the harness drives the protocol
	// the gateway PRESENTS for the company (the HostDesc's Protocol) — the D2
	// intersection rule is unchanged; the gateway HostDesc simply declares a
	// protocol the direct host would not.
	HostGateway ServingHost = "gateway"
)

// AuthMode — the canonical 5-mode set (no api-key mode; byok=user key,
// metered=Rensei key).
type AuthMode string

// AuthMode constants name the canonical 5 authentication modes.
const (
	AuthBYOK        AuthMode = "byok"
	AuthMetered     AuthMode = "metered"
	AuthShared      AuthMode = "shared"
	AuthHostSession AuthMode = "host-session"
	AuthLocal       AuthMode = "local"
)

// CostModel — the per-cell economic model.
type CostModel string

// CostModel constants name each per-cell economic model.
const (
	CostHostSubscription CostModel = "host-subscription"
	CostMeteredPerToken  CostModel = "metered"
	CostLocalFree        CostModel = "local"
)

// Family discriminates the two axes + unchanged peer families.
// FamilyHarness is byte-identical to 002's string "agent-runtime".
type Family string

// Family constants discriminate the two axes + the unchanged peer families.
const (
	FamilyHarness       Family = "agent-runtime"
	FamilyModelEndpoint Family = "model-endpoint"
	FamilySandbox       Family = "sandbox"
	FamilyIssueTracker  Family = "issue-tracker"
	FamilyVersionCtrl   Family = "version-control"
)

// TransportKind — HOW a (harness × host) pair runs the loop.
type TransportKind string

// TransportKind constants name how a (harness × host) pair runs the loop.
const (
	TransportCLIInjection  TransportKind = "cli-injection"
	TransportSubprocessRPC TransportKind = "subprocess-jsonrpc"
	TransportPTY           TransportKind = "pty"
	TransportDirectAPI     TransportKind = "direct-api"
)
