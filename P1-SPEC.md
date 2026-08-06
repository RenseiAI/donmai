# P1 Spec — Two-family contract + ModelEndpoint family + `go generate` SoT + CI parity gate

- **Date:** 2026-06-06
- **Phase:** Phase 1 of the two-axis provider program (`runs/completed/2026-06-06-provider-plugin-architecture` §7.1)
- **Repo:** `donmai` (OSS); consumed downstream by `rensei-tui` (regenerated registry, P1b) and `platform` (consumes `matrix.json`)
- **Status:** Implementation spec (precise, additive). NO behavior change.
- **Authoritative inputs:** `02-two-axis-architecture.md` §2/§3, `03-capability-matrix-spec.md`, `ADR-2026-06-06-two-axis-provider-model.md` D1–D3.
- **Hard constraint:** every existing caller, provider, and test compiles and passes **unchanged**. The runtime Spawn path is byte-for-byte identical when `Spec.Endpoint` is the zero value (which it always is until Phase 3 wires resolution). This phase only *adds* types, manifests, generated artifacts, and a CI gate.

> **Superseded identity note (2026-08-06).** This document remains the
> historical Phase-1 implementation spec, but its shared `raw` harness identity
> and capability-union requirements are no longer current. The live contract
> uses two canonical identities: `gemini-direct` for the Gemini SSE/direct loop
> and `ollama` for the local Ollama NDJSON loop. Matrix generation rejects
> duplicate harness ids instead of unioning capabilities. `raw` and `native`
> survive only as provider-paired, receipted runner compatibility aliases; an
> ambiguous or invalid pairing denies. The live manifests, matrix generator,
> and runner admission tests are authoritative for this post-P1 split.

---

## 0. The additive strategy in one paragraph (how v1 stays compiling)

Today the runtime contract is `agent.Provider` (`Name() / Capabilities() / Spawn() / Resume() / Shutdown()`), the live session is `agent.Handle`, providers declare `Capabilities()`, and `runner.Registry` maps `agent.ProviderName → agent.Provider`. **None of those change.** P1 introduces the two new families *alongside* the existing ones:

- `HarnessProvider` is defined as a **superset interface that embeds `Provider`** and adds one method, `Manifest() HarnessManifest`. `Session` is a **type alias** for `Handle` (`type Session = Handle`). Every existing provider keeps satisfying `Provider`; it becomes a `HarnessProvider` *only* once we add a `Manifest()` method to it (an additive method, not a replacement — `Capabilities()` stays).
- `ModelEndpointProvider` is a **brand-new interface** with no existing implementor; the four company endpoint packages are new files only.
- `Spec` gains **one optional field**, `Endpoint EndpointBinding` (zero value = today's behavior). `Spec.Model` stays the source of truth; `EndpointBinding.Model` is *not* yet read by any provider. Nothing in the Spawn path reads `Spec.Endpoint` in P1.
- The SoT (`matrix/`), the per-provider `manifest.go` files, the generated `harnesses.json` / `endpoints.json` / `matrix.json`, the generated registry+alias map, and the CI parity test are all **new files**. The generated registry is *introduced but not yet consumed* — `buildAgentRunRegistry` and the rensei-tui fork keep working verbatim (P1b swaps them to the generated registry; out of scope here).

Net: P1 is purely additive. `go build ./...` and `go test -race ./...` pass with zero edits to provider Spawn logic. The only edits to *existing* files are: (a) `agent/types.go` gains the new value types + the one `Spec.Endpoint` field; (b) each `provider/*/<name>.go` gains a `Manifest()` method (additive, alongside `Capabilities()`); (c) one new `//go:generate` directive lives in `matrix/`.

---

## 1. Exact new Go types + files in `agent/`

All new types live in OSS `donmai/agent`. Split across new files so the diff to existing files is minimal.

### 1.1 New file `agent/wire.go` — the binding currency (shared value types)

Verbatim from `02` §2.1. JSON tags camelCase (wire-consistent with existing types).

```go
package agent

// WireProtocol — on-the-wire request/response shape a model endpoint speaks.
// A harness declares which it can DRIVE; an endpoint declares which it SPEAKS.
type WireProtocol string

const (
	ProtoAnthropicMessages WireProtocol = "anthropic-messages"
	ProtoOpenAIChat        WireProtocol = "openai-chat"        // /v1/chat/completions (+ compat)
	ProtoOpenAIResponses   WireProtocol = "openai-responses"   // Codex app-server / Responses API
	ProtoGeminiGenerate    WireProtocol = "gemini-generate"    // :generateContent, SSE
	ProtoOllama            WireProtocol = "ollama"             // /api/chat NDJSON (bare surface)
	ProtoAntigravityOAuth  WireProtocol = "antigravity-oauth"  // agy CLI host-login channel (pty)
)

// ServingHost — WHERE the model is served, orthogonal to the wire protocol.
type ServingHost string

const (
	HostDirect   ServingHost = "direct"
	HostBedrock  ServingHost = "bedrock"
	HostVertex   ServingHost = "vertex"
	HostAzure    ServingHost = "azure"
	HostLocal    ServingHost = "local"     // ollama / on-box
	HostOAuthCLI ServingHost = "oauth-cli" // host login (agy / claude-sub / codex-sub) — BringsOwnAuth
)

// AuthMode — the canonical 5-mode set (no api-key mode; byok=user key, metered=Rensei key).
type AuthMode string

const (
	AuthBYOK        AuthMode = "byok"
	AuthMetered     AuthMode = "metered"
	AuthShared      AuthMode = "shared"
	AuthHostSession AuthMode = "host-session"
	AuthLocal       AuthMode = "local"
)

type CostModel string

const (
	CostHostSubscription CostModel = "host-subscription"
	CostMeteredPerToken  CostModel = "metered"
	CostLocalFree        CostModel = "local"
)

// Family discriminates the two axes + unchanged peer families.
// FamilyHarness is byte-identical to 002's string "agent-runtime".
type Family string

const (
	FamilyHarness       Family = "agent-runtime"
	FamilyModelEndpoint Family = "model-endpoint"
	FamilySandbox       Family = "sandbox"
	FamilyIssueTracker  Family = "issue-tracker"
	FamilyVersionCtrl   Family = "version-control"
)

// TransportKind — HOW a (harness × host) pair runs the loop.
type TransportKind string

const (
	TransportCLIInjection  TransportKind = "cli-injection"
	TransportSubprocessRPC TransportKind = "subprocess-jsonrpc"
	TransportPTY           TransportKind = "pty"
	TransportDirectAPI     TransportKind = "direct-api"
)
```

> **Decision (signing):** P1 does **not** add the `*Signature` field referenced in `02` §2.2/§2.3. Signing is Phase 7 (015 registry). Including a `Signature *Signature` field now would require a `Signature` type with no producer/consumer — dead surface. Omit it; the manifest structs are forward-compatible (add the optional pointer field later, additive).

### 1.2 New file `agent/harness.go` — Family A (`HarnessProvider`)

```go
package agent

// HarnessName — the loop-driver identity (distinct from the within-family
// ProviderName wire enum, which P1 keeps for back-compat aliasing).
type HarnessName string

const (
	HarnessClaudeCode  HarnessName = "claude-code"
	HarnessCodex       HarnessName = "codex"
	HarnessOpenCode    HarnessName = "opencode"
	HarnessAntigravity HarnessName = "antigravity"
	HarnessAmp         HarnessName = "amp"
	HarnessRaw         HarnessName = "raw"  // in-box net/http loop (gemini-direct + ollama)
	HarnessStub        HarnessName = "stub"
)

// HarnessCaps = today's flat agent-loop capabilities + the DRIVE surface.
// The agent-loop bools are a SUBSET projection of the existing Capabilities
// struct; the generator/manifest authors map Capabilities → HarnessCaps so
// the matrix is grounded in the live declaration (no second source of truth).
type HarnessCaps struct {
	// agent-loop (carried forward from Capabilities)
	SupportsMessageInjection bool   `json:"inject"`
	SupportsSessionResume    bool   `json:"resume"`
	SupportsToolPlugins      bool   `json:"tools"`
	AcceptsMcpServerSpec     bool   `json:"mcp"`
	AcceptsAllowedToolsList  bool   `json:"allowedToolsList"`
	EmitsSubagentEvents      bool   `json:"subagents"`
	SupportsReasoningEffort  bool   `json:"reasoningEffort"`
	SupportsOneShot          bool   `json:"oneShot"`        // can satisfy a future SpawnComplete projection
	NativeJSONMode           bool   `json:"nativeJsonMode"` // strict structured-out vs spawn-collect-validate
	ToolPermissionFormat     string `json:"toolPermissionFormat"`
	StreamingTransport       string `json:"streamingTransport"` // "sse"|"ndjson"|"websocket"|"none"

	// the DRIVE surface (the axis link)
	Drives      []WireProtocol `json:"drives"`
	DrivesHosts []ServingHost  `json:"drivesHosts"`
	Transport   TransportKind  `json:"transport"`
}

// HarnessManifest — pre-load, readable WITHOUT constructing the provider
// (closes 002 §manifest-separation). ContractABI = "harness/v2".
type HarnessManifest struct {
	Name        HarnessName `json:"name"`
	HumanLabel  string      `json:"humanLabel"`
	Family      Family      `json:"family"`      // FamilyHarness
	ContractABI string      `json:"contractAbi"` // "harness/v2"
	Caps        HarnessCaps `json:"capabilities"`
}

// Session is the live session interface. P1 ALIASES it to the existing Handle
// so every existing implementor of Handle is already a Session — no rename,
// no churn. (Renaming Handle → Session is a later cosmetic phase, if ever.)
type Session = Handle

// HarnessProvider — Family A. SUPERSET of the existing Provider interface
// (embeds it) + Manifest(). Every existing provider still satisfies Provider;
// it becomes a HarnessProvider only once a Manifest() method is added (§2).
type HarnessProvider interface {
	Provider
	Manifest() HarnessManifest
}
```

> **Why embed `Provider` rather than redefine the methods:** the doc (`02` §2.2) shows `HarnessProvider` with its own `Spawn`/`Resume`/`Shutdown` returning `Session`. Because `Session = Handle` (alias) and `Spec.Endpoint` is additive, the existing `Provider.Spawn(ctx, Spec) (Handle, error)` signature is *already* the §2.2 signature. **Embedding `Provider` is therefore the zero-churn realization of §2.2** — no provider re-implements anything, and a `HarnessProvider` value is assignable to `Provider` for the legacy registry. This is the load-bearing additive choice.

### 1.3 New file `agent/endpoint.go` — Family B (`ModelEndpointProvider`)

Verbatim from `02` §2.3 (minus the `Signature` field per §1.1 decision).

```go
package agent

type Company string

const (
	CompanyAnthropic Company = "anthropic"
	CompanyOpenAI    Company = "openai"
	CompanyGoogle    Company = "google"
	CompanyLocal     Company = "local"
	CompanyStub      Company = "stub" // test-only, both axes
)

// HostDesc — one (serving-host × auth × cost × protocol) cell within a company.
type HostDesc struct {
	Host          ServingHost  `json:"host"`
	Protocol      WireProtocol `json:"protocol"`
	AuthModes     []AuthMode   `json:"authModes"`
	BringsOwnAuth bool         `json:"bringsOwnAuth"`
	NeedsAPIKey   bool         `json:"needsApiKey"`
	CostModel     CostModel    `json:"costModel"`
	BaseURLTmpl   string       `json:"baseUrlTmpl"`
	EnvKeys       []string     `json:"envKeys"` // env-var NAMES (never values; OSS-safe)
}

type ModelDesc struct {
	ID               string        `json:"id"`
	HumanLabel       string        `json:"humanLabel"`
	ContextWindow    int           `json:"contextWindow"`
	SupportsTools    bool          `json:"supportsTools"`
	SupportsJSONMode bool          `json:"supportsJsonMode"`
	Hosts            []ServingHost `json:"hosts"`
}

// ModelEndpointManifest — company-named declaration. ContractABI = "model-endpoint/v1".
type ModelEndpointManifest struct {
	Company     Company        `json:"company"`
	HumanLabel  string         `json:"humanLabel"`
	Family      Family         `json:"family"` // FamilyModelEndpoint
	ContractABI string         `json:"contractAbi"`
	Speaks      []WireProtocol `json:"speaks"`
	Hosts       []HostDesc     `json:"hosts"`
	Models      []ModelDesc    `json:"models"`
}

// EndpointRequest — every field already RESOLVED FROM CONFIG.
type EndpointRequest struct {
	Model       string            `json:"model"`
	Host        ServingHost       `json:"host"`
	Auth        AuthMode          `json:"auth"`
	Region      string            `json:"region,omitempty"`
	EnvProvided map[string]string `json:"-"`
}

// EndpointBinding — the resolved cell. The currency crossing into Spec.Endpoint.
type EndpointBinding struct {
	Company       Company           `json:"company"`
	Model         string            `json:"model"`
	BaseURL       string            `json:"baseUrl"`
	Protocol      WireProtocol      `json:"protocol"`
	Host          ServingHost       `json:"host"`
	Auth          AuthMode          `json:"auth"`
	CostModel     CostModel         `json:"costModel"`
	BringsOwnAuth bool              `json:"bringsOwnAuth"`
	Env           map[string]string `json:"-"`
}

// IsZero reports whether the binding is unset (today's behavior). Used by the
// (future, Phase-3) Spawn path to decide between Spec.Model and Endpoint.Model.
func (b EndpointBinding) IsZero() bool { return b.Company == "" && b.Model == "" && b.Protocol == "" }

// ModelEndpointProvider — Family B. Declare + resolve.
type ModelEndpointProvider interface {
	Manifest() ModelEndpointManifest
	Resolve(ctx context.Context, req EndpointRequest) (EndpointBinding, error)
}
```

> **Scope note — `OneShotProvider` / `SpawnComplete` / `StreamTurn` are NOT in P1.** `02` §2.4 and §5.4 belong to Phase 4 (one-shot lane). P1's `HarnessCaps.SupportsOneShot`/`NativeJSONMode` are *declarations only* (matrix columns); no executor reads them yet. Defining `OneShotProvider` now would add an interface with no caller. Defer.

### 1.4 The single structural edit to `agent/types.go` (existing file)

Add **one field** to `Spec` (after `Model`, additive — JSON tag `endpoint,omitempty` so existing serialized `QueuedWork.resolvedProfile` payloads round-trip unchanged and never emit the field until set):

```go
	// Endpoint is the RESOLVED model-endpoint binding. Zero value (IsZero)
	// == today's behavior: providers read Spec.Model / Spec.Env as before.
	// Wired into the Spawn path in Phase 3; no provider reads it in P1.
	// Spec.Model remains the source of truth and is NOT yet derived from this.
	Endpoint EndpointBinding `json:"endpoint,omitempty"`
```

That is the **only** edit to `Spec`. `Spec.Model` is **kept as-is** (not made a read-through alias yet — the doc's "read-through alias" is a Phase-3+ migration step; doing it in P1 would change provider behavior). The doc's "Spec.Model is now DERIVED" line is explicitly deferred — P1 keeps Model authoritative to guarantee zero behavior change.

### 1.5 v1 → v2 reconciliation realized in P1 (per `02` §2.7)

| v1 primitive | P1 realization |
|---|---|
| `Manifest` (pre-load, signed) | Split into `HarnessManifest` + `ModelEndpointManifest`. Both pre-load; **signing deferred** (no `Signature` field in P1). |
| `TransportDesc` (transport×auth×cap row) | Decomposed: loop-caps + transport → `HarnessCaps`; auth/cost/host → `HostDesc`. Realized as *struct shapes + manifest data*; no resolver consumes it yet. |
| `OneShotProvider.Complete` / `SpawnComplete` | **Deferred to Phase 4.** Not defined in P1. |
| `provider-matrix.json` codegen + CI parity | Realized as `capability-matrix.json` (+ split `harnesses.json`/`endpoints.json`/`matrix.json`) + the parity test (§5). This IS the P1 deliverable. |
| `OnPreSpawn` credential hop (P0) | Phase 0, separate. P1 declares `HostDesc.EnvKeys` (data only); no actuation. |

---

## 2. Per-provider `Manifest()` mapping (the 8 providers → cells)

Each provider package gains a `Manifest() agent.HarnessManifest` method (additive, alongside the existing `Capabilities()`), making it satisfy `agent.HarnessProvider`. The `HarnessCaps` agent-loop bools **MUST be derived from / agree with** the live `Capabilities()` (the parity test §5 asserts agreement) — the manifest adds only the DRIVE surface + transport + oneShot/nativeJsonMode declarations.

Add one file per provider: `provider/<name>/manifest.go` (or append the method to `<name>.go`; new file preferred for diff clarity). The literals below are grounded in the real spawn mechanics confirmed in the code (claude=CLI exec `--output-format stream-json`; codex=`app-server` subprocess JSON-RPC; gemini=net/http to `generativelanguage.googleapis.com`; agycli=pty over `agy`; ollama=net/http `/api/chat`; opencode=CLI `opencode run --format json`; amp=CLI `--stream-json` reusing claude's JSONL mapper).

> **Harness identity vs legacy ProviderName.** The current packages return `ProviderName` values (`claude`, `codex`, `gemini`, `agy-cli`, `ollama`, `opencode`, `amp`, `stub`). The `Manifest().Name` uses the **HarnessName** vocabulary (`claude-code`, …, `raw`). `gemini` and `ollama` both map to harness `raw`; the *endpoint* company distinguishes them. The legacy-alias map (§4) keeps the `ProviderName` wire values resolving.

### 2.1 claude → harness `claude-code` × endpoint **Anthropic**

```go
func (*Provider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name: agent.HarnessClaudeCode, HumanLabel: "Claude Code",
		Family: agent.FamilyHarness, ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsMessageInjection: true, SupportsSessionResume: false,
			SupportsToolPlugins: true, AcceptsMcpServerSpec: true,
			AcceptsAllowedToolsList: true, EmitsSubagentEvents: true,
			SupportsReasoningEffort: true, SupportsOneShot: true, NativeJSONMode: false,
			ToolPermissionFormat: "claude", StreamingTransport: "ndjson", // claude CLI JSONL = ndjson framing
			Drives:      []agent.WireProtocol{agent.ProtoAnthropicMessages},
			DrivesHosts: []agent.ServingHost{agent.HostOAuthCLI, agent.HostDirect, agent.HostBedrock, agent.HostVertex},
			Transport:   agent.TransportCLIInjection,
		},
	}
}
```
Cells (matrix, §4): `claude-code × anthropic` on `oauth-cli` (host-session, bringsOwnAuth, host-sub; **legacyProviderId `claude`**), `direct` (byok/metered), `bedrock` (byok/metered), `vertex` (byok/metered). All `protocol: anthropic-messages`, `transport: cli-injection`.

### 2.2 codex → harness `codex` × endpoint **OpenAI**

```go
Name: agent.HarnessCodex, HumanLabel: "Codex", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:false, SupportsSessionResume:true, SupportsToolPlugins:true,
  AcceptsMcpServerSpec:true, AcceptsAllowedToolsList:false, EmitsSubagentEvents:false,
  SupportsReasoningEffort:true, SupportsOneShot:true, NativeJSONMode:false,
  ToolPermissionFormat:"codex", StreamingTransport:"none", // app-server JSON-RPC, not SSE/ndjson over the wire surface
  Drives:[]{ProtoOpenAIResponses, ProtoOpenAIChat},
  DrivesHosts:[]{HostOAuthCLI, HostDirect, HostAzure},
  Transport: TransportSubprocessRPC}
```
Cells: `codex × openai` on `oauth-cli` (host-session, app-server, host-sub; **legacyProviderId `codex`**), `direct` (byok/metered), `azure` (byok/metered). Protocol `openai-responses` on the app-server cells; `openai-chat` available per Drives.

### 2.3 gemini → harness `raw` × endpoint **Google** (direct-api)

```go
Name: agent.HarnessRaw, HumanLabel: "Gemini (direct)", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:true, SupportsSessionResume:false, SupportsToolPlugins:true,
  AcceptsMcpServerSpec:false, AcceptsAllowedToolsList:true, EmitsSubagentEvents:false,
  SupportsReasoningEffort:true, SupportsOneShot:true, NativeJSONMode:true, // responseSchema
  ToolPermissionFormat:"gemini", StreamingTransport:"sse",
  Drives:[]{ProtoGeminiGenerate},
  DrivesHosts:[]{HostDirect, HostVertex},
  Transport: TransportDirectAPI}
```
Cells: `raw × google` on `direct` (byok/metered, NeedsAPIKey, NativeJSONMode; **legacyProviderId `gemini`**), `vertex` (byok/metered). Protocol `gemini-generate`, transport `direct-api`.

> **Note — two `raw` harness manifests.** Both gemini and ollama map to `HarnessRaw`. P1 has each package return its own `HarnessManifest{Name: HarnessRaw, ...}` with package-specific `Drives`/`Transport`. The matrix generator treats `raw` as one harness id but its `drivesProtocols`/`endpointShapes` is the **union** of the two packages' drive surfaces (`gemini-generate`+`ollama`, hosts `direct`+`vertex`+`local`). The parity test asserts each cell's `protocol ∈ raw.drives` against that union. (Per `02` OQ #9 — keep two thin packages; the matrix doesn't care.)

### 2.4 agycli → harness `antigravity` × endpoint **Google** (oauth-cli, pty)

```go
Name: agent.HarnessAntigravity, HumanLabel: "Antigravity", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:false, SupportsSessionResume:false, SupportsToolPlugins:false,
  AcceptsMcpServerSpec:false, AcceptsAllowedToolsList:false, EmitsSubagentEvents:false,
  SupportsReasoningEffort:false, SupportsOneShot:true, NativeJSONMode:false,
  ToolPermissionFormat:"", StreamingTransport:"none", // pty plaintext
  Drives:[]{ProtoAntigravityOAuth},
  DrivesHosts:[]{HostOAuthCLI},
  Transport: TransportPTY}
```
Cell: `antigravity × google` on `oauth-cli` (host-session/local, bringsOwnAuth, host-sub, MCP false; **legacyProviderId `agy-cli`**). Protocol `antigravity-oauth`, transport `pty`. **Same company (Google) as gemini, different host cell** — the §2.6 collapse, realized as matrix data.

### 2.5 ollama → harness `raw` × endpoint **Local**

```go
Name: agent.HarnessRaw, HumanLabel: "Ollama (local)", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:false, SupportsSessionResume:false, SupportsToolPlugins:false,
  AcceptsMcpServerSpec:false, AcceptsAllowedToolsList:false, EmitsSubagentEvents:false,
  SupportsReasoningEffort:false, SupportsOneShot:true, NativeJSONMode:true, // format:"json" (whole-response)
  ToolPermissionFormat:"claude", StreamingTransport:"ndjson",
  Drives:[]{ProtoOllama},
  DrivesHosts:[]{HostLocal},
  Transport: TransportDirectAPI}
```
Cell: `raw × local` on `local` (NeedsAPIKey false, local, bringsOwnAuth true; **legacyProviderId `ollama`**). Protocol `ollama`, transport `direct-api`.

### 2.6 opencode → harness `opencode` × any **OpenAI-compat** cell

```go
Name: agent.HarnessOpenCode, HumanLabel: "OpenCode", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:false, SupportsSessionResume:false, SupportsToolPlugins:false,
  AcceptsMcpServerSpec:false, AcceptsAllowedToolsList:false, EmitsSubagentEvents:false,
  SupportsReasoningEffort:true, SupportsOneShot:true, NativeJSONMode:true, // /v1 honors response_format
  ToolPermissionFormat:"claude", StreamingTransport:"ndjson",
  Drives:[]{ProtoOpenAIChat},   // ONLY openai-chat — NOT anthropic-messages (cross-protocol cell not-yet-valid)
  DrivesHosts:[]{HostOAuthCLI, HostLocal, HostDirect},
  Transport: TransportCLIInjection}
```
Cells: `opencode × openai` on `direct` (byok/metered), `opencode × local` (Ollama `/v1`, local), `opencode × google` on `local` (Google-served-by-Ollama `/v1`, the **north-star cell**, `legacyProviderId null`, `smoked:false`). All protocol `openai-chat`. **No `opencode × anthropic` cell** — Anthropic speaks `anthropic-messages`, not in opencode's Drives (`02` §3.2 `⛔`).

### 2.7 amp → harness `amp` × endpoint **Anthropic** (direct, metered)

```go
Name: agent.HarnessAmp, HumanLabel: "Amp", ContractABI: "harness/v2",
Caps: {SupportsMessageInjection:false, SupportsSessionResume:false, SupportsToolPlugins:true,
  AcceptsMcpServerSpec:true, AcceptsAllowedToolsList:false, EmitsSubagentEvents:false,
  SupportsReasoningEffort:false, SupportsOneShot:true, NativeJSONMode:false,
  ToolPermissionFormat:"claude", StreamingTransport:"ndjson", // reuses claude JSONL mapper
  Drives:[]{ProtoAnthropicMessages},
  DrivesHosts:[]{HostDirect},
  Transport: TransportCLIInjection}
```
Cell: `amp × anthropic` on `direct` — **metered, `bringsOwnAuth:false`, `needsApiKey:true`, env `AMP_API_KEY`, costModel metered** (`legacyProviderId `amp``). NOT a host-subscription/≈$0 cell (`02` remediation #2; §4.4 structural cost-honesty: a key-needing cell cannot be `bringsOwnAuth`).

### 2.8 stub → harness `stub` × endpoint **stub**

```go
Name: agent.HarnessStub, HumanLabel: "Test Stub", ContractABI: "harness/v2",
Caps: {/* mirror defaultCapabilities(): all true */ SupportsOneShot:true, NativeJSONMode:true,
  ToolPermissionFormat:"claude", StreamingTransport:"none",
  Drives:[]{/* stub protocol — add ProtoStub WireProtocol = "stub" */},
  DrivesHosts:[]{/* HostLocal */},
  Transport: TransportDirectAPI}
```
Cell: `stub × stub` (test-only; `legacyProviderId `stub``). Add `ProtoStub WireProtocol = "stub"` and `CompanyStub` so the stub cell satisfies the intersection rule without special-casing the parity test.

### 2.9 Summary table (8 providers → cells)

| pkg (legacy ProviderName) | harness | company endpoint | host cell(s) | protocol | transport | cost | legacyProviderId |
|---|---|---|---|---|---|---|---|
| claude | `claude-code` | Anthropic | oauth-cli / direct / bedrock / vertex | anthropic-messages | cli-injection | host-sub (oauth) · metered (keyed) | **claude** (oauth-cli cell) |
| codex | `codex` | OpenAI | oauth-cli / direct / azure | openai-responses | subprocess-jsonrpc | host-sub · metered | **codex** (oauth-cli) |
| gemini | `raw` | Google | direct / vertex | gemini-generate | direct-api | metered | **gemini** (direct) |
| agy-cli | `antigravity` | Google | oauth-cli | antigravity-oauth | pty | host-sub | **agy-cli** |
| ollama | `raw` | Local | local | ollama | direct-api | local | **ollama** |
| opencode | `opencode` | OpenAI / Local / Google | direct / local / local(`/v1`) | openai-chat | cli-injection | metered · local | null (north-star: opencode×google) |
| amp | `amp` | Anthropic | direct | anthropic-messages | cli-injection | **metered** (AMP_API_KEY) | **amp** |
| stub | `stub` | stub | local | stub | direct-api | local | **stub** |

---

## 3. New ModelEndpoint provider packages (named by company)

Four new packages under `provider/endpoint/<company>/` (plus a `stub` endpoint). Each ships `manifest.go` (the `ModelEndpointManifest` literal) and `<company>.go` (a `Resolve()` that templates `BaseURLTmpl` + region and copies `EnvProvided[envKeys]` into `EndpointBinding.Env`). **`Resolve()` is pure (no network)** in P1 — it does not dial; it only constructs the binding. No package imports a vendor SDK.

> **Env-key reality (grounded in the code):** gemini probes `GEMINI_API_KEY` then `GOOGLE_API_KEY`; ollama needs no key; amp needs `AMP_API_KEY`; codex/claude on oauth-cli ride host login (no env key). These map straight onto `HostDesc.EnvKeys`.

### 3.1 `provider/endpoint/anthropic` — `CompanyAnthropic`

`Speaks: [anthropic-messages]`. HostDescs:
| host | protocol | authModes | bringsOwnAuth | needsApiKey | costModel | baseUrlTmpl | envKeys |
|---|---|---|---|---|---|---|---|
| oauth-cli | anthropic-messages | host-session | true | false | host-subscription | (host login) | [] |
| direct | anthropic-messages | byok, metered | false | true | metered | `https://api.anthropic.com` | `ANTHROPIC_API_KEY` |
| bedrock | anthropic-messages | byok, metered | false | true | metered | `https://bedrock-runtime.{region}.amazonaws.com` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` |
| vertex | anthropic-messages | byok, metered | false | true | metered | `https://{region}-aiplatform.googleapis.com` | `GOOGLE_APPLICATION_CREDENTIALS`, `ANTHROPIC_VERTEX_PROJECT_ID` |

Models: `claude-sonnet-4-5`, `claude-haiku` (hosts direct/bedrock/vertex). `SupportsJSONMode:false` (Messages has no `json_schema` — `structuredVia: forced-tool` on keyed cells per `02` OQ #6).

### 3.2 `provider/endpoint/openai` — `CompanyOpenAI`

`Speaks: [openai-responses, openai-chat]`. HostDescs:
| host | protocol | authModes | bringsOwnAuth | needsApiKey | costModel | baseUrlTmpl | envKeys |
|---|---|---|---|---|---|---|---|
| oauth-cli | openai-responses | host-session | true | false | host-subscription | (codex app-server host login) | [] |
| direct | openai-chat | byok, metered | false | true | metered | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| azure | openai-chat | byok, metered | false | true | metered | `https://{resource}.openai.azure.com` | `AZURE_OPENAI_API_KEY`, `AZURE_OPENAI_ENDPOINT` |

`SupportsJSONMode:true` (`json_schema` strict on keyed cells; `nativeJsonMode:true`).

### 3.3 `provider/endpoint/google` — `CompanyGoogle` (TWO protocols, the collapse)

`Speaks: [gemini-generate, openai-chat]`. HostDescs (this is where gemini + agy-cli collapse to ONE company):
| host | protocol | authModes | bringsOwnAuth | needsApiKey | costModel | baseUrlTmpl | envKeys |
|---|---|---|---|---|---|---|---|
| direct | gemini-generate | byok, metered | false | true | metered | `https://generativelanguage.googleapis.com` | `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| vertex | gemini-generate | byok, metered | false | true | metered | `https://{region}-aiplatform.googleapis.com` | `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_VERTEX_PROJECT_ID` |
| oauth-cli | antigravity-oauth | host-session, local | true | false | host-subscription | (agy host login) | [] |
| local | openai-chat | local | true | false | local | `http://127.0.0.1:11434/v1` | [] |

Models: `gemini-2.5-pro` (direct/vertex), `SupportsJSONMode:true` on the keyed cells. **`nativeJsonMode` is per-host:** true on direct/vertex/local, false on oauth-cli.

### 3.4 `provider/endpoint/local` — `CompanyLocal` (Ollama bare surface)

`Speaks: [ollama]`. HostDesc:
| host | protocol | authModes | bringsOwnAuth | needsApiKey | costModel | baseUrlTmpl | envKeys |
|---|---|---|---|---|---|---|---|
| local | ollama | local | true | false | local | `http://127.0.0.1:11434` | [] |

Models: discovered dynamically (empty `Models` allowed). `SupportsJSONMode: partial` → declare `true` (the `format:"json"` whole-response primitive).

### 3.5 `provider/endpoint/stub` — `CompanyStub` (test-only)

`Speaks: [stub]`, one `local` HostDesc, one model. Lets the matrix carry a `stub × stub` cell that satisfies the parity intersection rule with no special-casing.

> **Boundary safety:** EnvKeys are env-var **names** only (public facts). No concrete key values, no metered-key identity, no Linear IDs. `02` §7 / `03` §7 — the JSON passes `scripts/leak-guard.sh` by construction. **Bedrock/vertex metered Anthropic cells are platform-only docs surfaces** but the *manifest data* (auth-mode legality, env-var names) is OSS-canonical — fine to ship in OSS donmai per `03` §7.

---

## 4. SoT + codegen — the `matrix/` package

New package `matrix/`. Pure Go, no network, deterministic output (sorted keys, stable JSON marshal).

### 4.1 Layout

```
matrix/
  cells.go        // hand-authored: the valid-cell list (HarnessEndpointCell literals) + denylist
  doc.go          // package doc + the //go:generate directive
  gen/
    main.go       // the generator (package main; build-tagged or a subdir main)
  testdata/
    capability-matrix.json   // committed SoT artifact (generated; CI-parity-gated)
    harnesses.json           // committed (generated)
    endpoints.json           // committed (generated)
    matrix.json              // committed (generated)
  registry_gen.go            // GENERATED: legacy-alias map + (P1b) registry ctors
  parity_test.go             // the CI parity gate (§5)
```

> **Placement of generated artifacts:** the doc (`03`) implies repo-root JSON. Recommend the JSON live at a stable committed path (`matrix/` root, not `testdata/` — `testdata/` is ignored by `go build` and we want platform/rensei-tui to read it). Final path: `matrix/capability-matrix.json` + the three split files at `matrix/`. The parity test regenerates into a temp buffer and `bytes.Equal`-compares against the committed files.

### 4.2 Hand-authored input (`matrix/cells.go`)

Two hand-authored data structures (the **only** hand-authored capability data per `03` §1):

1. **`harnessManifests` / `endpointManifests`** — these are *not* re-typed; the generator imports the provider packages and calls each provider's `Manifest()` / each endpoint's `Manifest()` to harvest the authoritative literals. (This keeps the manifest as the single SoT and makes the §5 "manifest agrees with Capabilities()" assertion meaningful.) `cells.go` holds the *list of provider/endpoint constructors to harvest*.
2. **`validCells []HarnessEndpointCell`** — the hand-authored valid-cell list (one literal per row of §2.9 + the north-star cell), each carrying `harness`, `endpoint`, `protocol`, `host`, `transport`, `authModes`, `bringsOwnAuth`, `needsApiKey`, `costModel`, `oneShot`, `nativeJsonMode`, `structuredVia`, optional per-cell `caps` narrowing, `legacyProviderId`, `stability`, `smoked`.
3. **`denylist []CellKey`** — known-bad `(harness, endpoint, host)` triples (per `02` OQ #3: protocol-intersection + small denylist, not a full allowlist).

### 4.3 The generator (`matrix/gen/main.go`)

Pure Go `package main`, run by `go generate`. Structure:

1. Harvest harness manifests (call `Manifest()` on a constructed-or-zero instance of each of the 8 harness providers) and endpoint manifests (the 5 endpoints). **Construction caveat:** several provider `New()` calls probe the environment and may fail on a CI box with no CLI installed. The generator must obtain the manifest **without** a successful probe. **Recommend:** make `Manifest()` a **method on the zero-value `*Provider`** (it already is, per §2 — `func (*Provider) Manifest()` takes no constructed state) so `(&claude.Provider{}).Manifest()` works with no probe. The generator constructs zero-value provider structs purely to call `Manifest()`. (This is why §2 declares `Manifest()` on `(*Provider)` not on a probe-gated value.)
2. Build `harnesses[]` from harness manifests; merge the two `raw` manifests into one harness row with union `drives`/`endpointShapes` (§2.3 note).
3. Build `modelEndpoints[]` from endpoint manifests.
4. Build `harnessEndpointCells[]` from `validCells`, **validating** each against the harvested manifests (protocol intersection, authModes subset, caps narrowing) — fail `go generate` loudly on an invalid hand-authored cell.
5. Build the **legacy-alias map** `map[ProviderName]CellKey` (`claude→{claude-code,anthropic,oauth-cli}`, `codex→{codex,openai,oauth-cli}`, `gemini→{raw,google,direct}`, `agy-cli→{antigravity,google,oauth-cli}`, `ollama→{raw,local,local}`, `amp→{amp,anthropic,direct}`, `stub→{stub,stub,local}`; opencode's three cells have no single legacy id — keep `opencode→{opencode,openai,direct}` as the legacy default so the existing `ProviderName "opencode"` keeps resolving).
6. Marshal deterministically (sorted; `encoding/json` with sorted map keys or struct slices sorted by id). Write `capability-matrix.json` + the three split files + `registry_gen.go` (the alias map as Go source for the runner, plus — P1b — the ctor list; in P1 emit the alias map only).
7. `peer family` sections (`sandbox`, `issueTracker`, `vcs`, …): P1 emits **empty arrays with the section keys present** (so the schema is complete and docs never silently omit a family) — populating them is later work. The schema carries `schemaVersion`, `contractAbi`, `generatedFrom`. **Omit `generatedAt`** (a timestamp breaks the byte-identical parity assertion); use a fixed value or drop the field. (Doc shows `generatedAt`; for the byte-identical gate to work it MUST be stable — recommend dropping it or sourcing from a committed `VERSION`.)

### 4.4 The `//go:generate` directive

In `matrix/doc.go`:
```go
//go:generate go run ./gen
```
`make generate` (new Makefile target) wraps `go generate ./matrix/...`. Run with `GOWORK=off` parity in mind (the generator imports only `donmai/agent` + `donmai/provider/*` — all in-module, no workspace dependency).

---

## 5. The CI parity test (`matrix/parity_test.go`, runnable `GOWORK=off`)

A single pure test (no network, no daemon). Asserts the six gate rules from `03` §parity-gate + the manifest-agreement rule:

1. **Byte-identical to fresh generate.** Run the generator into in-memory buffers; `bytes.Equal` each against the committed `capability-matrix.json` / `harnesses.json` / `endpoints.json` / `matrix.json`. Fail with a diff hint ("`run make generate`").
2. **Every `legacyProviderId` present.** Each non-null `cell.legacyProviderId` and each legacy-alias-map key is a known `agent.ProviderName` const AND resolves to a cell in `matrix.json`. (Cross-repo: §6 covers the `PROVIDER_REGISTRY` / rensei-tui registry half — P1 asserts the OSS half; the platform/rensei-tui assertion lands when they consume the JSON in P1b.)
3. **Protocol intersection.** For every cell: `cell.protocol ∈ harness.drives` AND `cell.protocol == endpoint.host(cell.host).protocol` (i.e. `cell.protocol ∈ harness.Drives ∩ {host's protocol}`). For `raw`, `harness.drives` is the union of the two `raw` packages.
4. **authModes subset + declared.** `cell.authModes ⊆ endpoint.host(cell.host).authModes`; each authMode ∈ the canonical 5-mode enum. (The "declared in some access_policy baseline" half is platform-side; P1 asserts the enum-membership half.)
5. **AGENT_ENV_BLOCKLIST de-dup.** Read `internal/credentials/blocklist.AgentEnvBlocklist`; assert no `HostDesc.EnvKeys` entry across all endpoints collides with a blocklist entry (an endpoint must never declare an env key the snapshot would strip), and assert the blocklist has no duplicate entries.
6. **Caps narrowing-only.** For every cell with a `caps` override: each set bool in `cell.caps` is `false` only where it narrows (`cell.caps ⊆ harness.caps` — a cell may remove, never add, a capability).
7. **Manifest agrees with `Capabilities()` (P1-specific, the additive-safety guard).** For each of the 8 harness providers, construct the zero-value provider and assert `Manifest().Caps.{Supports*,Accepts*,Emits*,ToolPermissionFormat}` equals the corresponding fields of `Capabilities()` — proving the manifest is a faithful additive projection, not a divergent second source of truth.

Run target: `GOWORK=off go test -race ./matrix/...`. The test is the **load-bearing merge gate**; treat generated files as committed artifacts (`03` §4).

---

## 6. No-behavior-change proof obligations

The implementer MUST demonstrate all of the following before declaring P1 done:

- **`go build ./...` passes** in donmai (and `GOWORK=off go build ./...` for the OSS-standalone check). No existing import breaks; the new packages compile.
- **`make test` (`go test -race ./...`) stays green** with **zero edits to existing test files**. Specifically the known-good suites must pass unchanged: `runner/registry_test.go`, every `provider/*/*_test.go`, `runner/loop` tests, `afcli/agent_run` tests, `internal/credentials/*_test.go`.
- **`buildAgentRunRegistry` is untouched** (both donmai's `afcli/agent_run.go` and the rensei-tui fork `cmd/rensei/registry.go`). The generated registry (`matrix/registry_gen.go`) is introduced but **not yet consumed** — swapping the registries to generated is P1b. The legacy `ProviderName`-keyed `runner.Registry` still resolves `claude/codex/gemini/agy-cli/ollama/opencode/amp/stub` exactly as today.
- **Spawn path identical when `Spec.Endpoint.IsZero()`.** No provider's `Spawn`/`Resume` reads `Spec.Endpoint` in P1. `Spec.Model` / `Spec.Env` remain authoritative. Demonstrate by grep: `grep -rn 'spec.Endpoint\|Spec.Endpoint\|\.Endpoint' provider/ runner/` returns only the new manifest/endpoint files, never a Spawn callsite.
- **Wire round-trip unchanged.** `Spec` JSON with no `endpoint` key deserializes identically (the `omitempty` tag guarantees no new field is emitted for existing producers). Add a table test asserting `json.Marshal(Spec{...})` of a pre-P1-shaped Spec produces no `"endpoint"` key.
- **Aliases byte-match.** The legacy-alias map covers every current `ProviderName` const that names a real provider (`claude, codex, gemini, agy-cli, ollama, opencode, amp, stub`); reserved-but-unimplemented names (`spring-ai, a2a, jules`) need NO cell (they have no provider) — assert they are intentionally absent, not silently dropped.

---

## 7. File-by-file change list + build/test gate plan

### 7.1 New files

| File | Contents |
|---|---|
| `agent/wire.go` | WireProtocol, ServingHost, AuthMode, CostModel, Family, TransportKind + consts (incl. `ProtoStub`) |
| `agent/harness.go` | HarnessName consts, HarnessCaps, HarnessManifest, `Session = Handle` alias, HarnessProvider interface |
| `agent/endpoint.go` | Company consts (incl. `CompanyStub`), HostDesc, ModelDesc, ModelEndpointManifest, EndpointRequest, EndpointBinding (+`IsZero`), ModelEndpointProvider interface |
| `provider/claude/manifest.go` | `func (*Provider) Manifest() agent.HarnessManifest` (§2.1) |
| `provider/codex/manifest.go` | §2.2 |
| `provider/gemini/manifest.go` | §2.3 |
| `provider/agycli/manifest.go` | §2.4 |
| `provider/ollama/manifest.go` | §2.5 |
| `provider/opencode/manifest.go` | §2.6 |
| `provider/amp/manifest.go` | §2.7 |
| `provider/stub/manifest.go` | §2.8 (method on `*provider`) |
| `provider/endpoint/anthropic/{anthropic.go,manifest.go}` | §3.1 manifest + pure `Resolve` |
| `provider/endpoint/openai/{openai.go,manifest.go}` | §3.2 |
| `provider/endpoint/google/{google.go,manifest.go}` | §3.3 |
| `provider/endpoint/local/{local.go,manifest.go}` | §3.4 |
| `provider/endpoint/stub/{stub.go,manifest.go}` | §3.5 |
| `matrix/cells.go` | valid-cell list, denylist, harvest list (§4.2) |
| `matrix/doc.go` | package doc + `//go:generate go run ./gen` |
| `matrix/gen/main.go` | the generator (§4.3) |
| `matrix/capability-matrix.json` + `harnesses.json` + `endpoints.json` + `matrix.json` | committed generated artifacts |
| `matrix/registry_gen.go` | GENERATED legacy-alias map |
| `matrix/parity_test.go` | the CI parity gate (§5) |

### 7.2 Additive edits to existing files

| File | Edit |
|---|---|
| `agent/types.go` | add ONE field to `Spec`: `Endpoint EndpointBinding `json:"endpoint,omitempty"`` (§1.4) |
| `Makefile` | add `generate:` target (`go generate ./matrix/...`); optionally a `verify-generated:` that runs the parity test |
| `agent/doc.go` (if present) | optional: a sentence noting the two new families are additive |

**No edits** to: `agent/provider.go`, `agent/handle.go`, `runner/registry.go`, `afcli/agent_run.go`, `rensei-tui/cmd/rensei/registry.go`, any provider Spawn/Resume/Capabilities, any existing test.

### 7.3 Build/test gate plan (deterministic execution order)

1. Add `agent/wire.go`, `agent/harness.go`, `agent/endpoint.go` + the `Spec.Endpoint` field. `go build ./agent/`.
2. Add the 8 `provider/*/manifest.go`. `go build ./provider/...`. Confirm each provider now satisfies `agent.HarnessProvider` (a compile-time `var _ agent.HarnessProvider = (*Provider)(nil)` assertion in each manifest.go is recommended).
3. Add the 5 `provider/endpoint/*` packages. `go build ./provider/endpoint/...`. Add `var _ agent.ModelEndpointProvider = (*Endpoint)(nil)` assertions.
4. Add `matrix/cells.go`, `matrix/gen/main.go`. Run `go run ./matrix/gen` → produces the JSON + `registry_gen.go`. Commit the artifacts.
5. Add `matrix/parity_test.go`. `GOWORK=off go test -race ./matrix/...` green.
6. Full gate: `make fmt && make lint && GOWORK=off go build ./... && make test` (`go test -race ./...`) — all green, no existing test edited.
7. Type gate (per CLAUDE.md): `go build ./...` in the workspace AND `GOWORK=off go build ./...` standalone. Validate rensei-tui still builds against the new donmai (`GOWORK=off` in rensei-tui) — it should, since nothing it imports changed signature.

---

## 8. Risks / ambiguities the implementer must watch

- **`Spec.Endpoint` threading is a trap.** The doc says "Spec.Model is now DERIVED (Endpoint.Model)". **Do NOT do that in P1** — it would change behavior the moment any producer sets Endpoint. P1 keeps `Spec.Model` authoritative and `Spec.Endpoint` write-only-but-unread. The derive step is Phase 3.
- **`Manifest()` must work on a zero/unprobed value.** The generator harvests manifests without a successful `New()` probe (CI boxes have no `claude`/`codex`/`agy` installed). Declare `Manifest()` on `(*Provider)` with no dependence on probe state (as the existing `Name()`/`Capabilities()` already do — confirmed: all are pointer-receiver, state-free). The parity test relies on this.
- **`generatedAt` breaks byte-identical parity.** Drop the timestamp field (or source it from a committed VERSION). A non-deterministic field makes rule #1 unsatisfiable.
- **`raw` is two packages, one harness id.** gemini and ollama both emit `HarnessRaw`. The generator must MERGE them into one `harnesses[]` row (union drives/hosts) and the parity intersection check must validate cells against that union, not a single package's drives. Easy to get a false-red here.
- **agycli/gemini → Google collapse is STAGED.** P1 only *declares* the collapse as matrix data (both map to `CompanyGoogle`, different host cells). `applyRuntimeProviderRewrite` is **not** deleted in P1 (that is Phase 6, platform-side, lock-step). The OSS runner still routes by legacy `ProviderName` via the alias map. Do not touch routing.
- **Alias coverage completeness.** Every real provider's `ProviderName` MUST map to a cell, or the legacy runner silently loses a provider in P1b. opencode has 3 cells but one legacy id — pin a default (`{opencode,openai,direct}`). Reserved names (`spring-ai/a2a/jules`) must be asserted absent, not forgotten.
- **EnvKeys vs blocklist collision (rule #5).** None of the declared endpoint env keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `AMP_API_KEY`, AWS/Vertex creds) may appear in `AgentEnvBlocklist` (which lists daemon-auth surfaces like `DONMAI_DAEMON_JWT`, `WORKER_API_KEY`). Verify before committing — a collision means the snapshot would strip a key the endpoint needs.
- **Cross-protocol cells must not appear.** opencode's `Drives` is `[openai-chat]` ONLY — no `anthropic-messages`. Authoring an `opencode × anthropic` cell trips rule #3. The north-star `opencode × google` rides Google's `local` (`/v1` openai-chat) host, NOT the gemini-generate host.
- **amp cost honesty.** amp's `direct` cell is `metered` + `needsApiKey:true` + `bringsOwnAuth:false`. Authoring it as host-subscription/≈$0 violates the structural cost-honesty invariant (a key-needing cell cannot be bringsOwnAuth) and is the explicit `02` remediation #2 regression to avoid.
- **Generated-artifact path.** Don't put the committed JSON under `testdata/` (excluded from build/embed). Platform and rensei-tui will read these files; keep them at `matrix/`.
- **`GOWORK=off` decoupling.** The generator and parity test import only in-module packages so they run identically with and without the go.work workspace. Validate both — a workspace-only import would red the OSS CI lane.
