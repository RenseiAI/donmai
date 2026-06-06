package agent

// This file declares Family A — the harness (loop-driver) family — for the
// two-axis provider model (Phase 1). It is purely additive: HarnessProvider
// is a SUPERSET interface that embeds the existing Provider and adds one
// method, Manifest(). Every existing provider keeps satisfying Provider; it
// becomes a HarnessProvider only once a Manifest() method is added (an
// additive method, not a replacement — Capabilities() stays).
//
// See 02-two-axis-architecture.md §2.2.

// HarnessName — the loop-driver identity (distinct from the within-family
// ProviderName wire enum, which P1 keeps for back-compat aliasing).
type HarnessName string

// HarnessName constants name each loop-driver identity.
const (
	HarnessClaudeCode  HarnessName = "claude-code"
	HarnessCodex       HarnessName = "codex"
	HarnessOpenCode    HarnessName = "opencode"
	HarnessAntigravity HarnessName = "antigravity"
	HarnessAmp         HarnessName = "amp"
	HarnessRaw         HarnessName = "raw" // in-box net/http loop (gemini-direct + ollama)
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
// it becomes a HarnessProvider only once a Manifest() method is added.
//
// Because Session = Handle (alias) and Spec.Endpoint is additive, the existing
// Provider.Spawn(ctx, Spec) (Handle, error) signature is already the §2.2
// signature — embedding Provider is the zero-churn realization of §2.2.
type HarnessProvider interface {
	Provider
	Manifest() HarnessManifest
}
