package agent

import "context"

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
	// HarnessShell is the bare interactive-only PTY spawn mode
	// (provider/harness/shell): it drives no model endpoint at all, so it
	// anchors no matrix cell (matrix/cells.go) — it exists purely as an
	// interactive spawn-mode row alongside claude/codex. W4.
	HarnessShell HarnessName = "shell"
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

	// SupportsInteractivePTY declares an ADDITIONAL spawn mode (W4,
	// interactive-attach-v1): this harness can be spawned under a PTY
	// (agent.Spec.Interactive != nil; see agent/interactive.go) running its
	// own interactive UI instead of its headless loop. It is orthogonal to
	// the agent-loop bools above and to Transport below — Transport still
	// names how the harness's DEFAULT headless loop runs (e.g. claude stays
	// cli-injection, codex stays subprocess-jsonrpc); TransportPTY is used
	// only by a harness whose ONLY transport is PTY (e.g. the shell
	// harness). The set of interactive-capable harnesses is always read
	// from this field on the live manifest registry — never hardcoded as a
	// closed {claude, codex, shell} list, so P8's Vertex/Bedrock/Azure/
	// OpenRouter/Fireworks/Groq harnesses join by flipping this flag alone.
	// See HarnessManifest.SupportsInteractivePTY for the registry-check
	// helper.
	SupportsInteractivePTY bool `json:"interactivePty"`

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

// Base projects the harness manifest onto the family-agnostic ProviderBase
// header so HarnessManifest satisfies BaseManifest — the additive realization
// of "every family manifest extends ProviderManifest<F>" (002 §"The base
// interface"). It does NOT change HarnessManifest's wire layout (the projection
// is computed, not stored): ID is the within-family HarnessName, Family is the
// declared discriminant, Name comes from HumanLabel, Version from the
// ContractABI, APIVersion from the base-contract constant, and Scope defaults
// to global (a bundled OSS harness applies everywhere). Signature is nil today
// — signing is deferred for the two-axis manifests per ADR-2026-06-06.
func (m HarnessManifest) Base() ProviderBase {
	return ProviderBase{
		APIVersion: ProviderAPIVersion,
		Family:     m.Family,
		ID:         string(m.Name),
		Name:       m.HumanLabel,
		Version:    m.ContractABI,
		Scope:      GlobalScope(),
		Stability:  StabilityStable,
	}
}

// Compile-time assertion: HarnessManifest satisfies the base contract.
var _ BaseManifest = HarnessManifest{}

// SupportsInteractivePTY reports whether this harness declares the
// interactive PTY spawn-mode capability (W4; HarnessCaps.SupportsInteractivePTY).
// This is the helper a composing layer (e.g. the runner) calls BEFORE
// setting Spec.Interactive, so the set of interactive-capable harnesses is
// always derived from the live manifest — never a hardcoded
// {claude, codex, shell} literal. A harness without the capability that
// nonetheless receives Spec.Interactive != nil ignores it, per the same
// capability-gated-Spec contract every other Spec field follows.
func (m HarnessManifest) SupportsInteractivePTY() bool {
	return m.Caps.SupportsInteractivePTY
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
//
// HOW THE HARNESS FAMILY EXTENDS THE BASE CONTRACT (002 §"The base interface").
// The harness family extends the SDK base contract at the MANIFEST level:
// HarnessManifest.Base() projects the manifest onto ProviderBase, so a harness
// is administrable, scopable, and verifiable through the family-agnostic header
// like any other family. The base LIFECYCLE maps onto the existing surface:
// Provider construction (provider.New, fail-fast probing) ≡ the base
// activate(); Provider.Shutdown ≡ the base deactivate(); the base health() is
// the additive Health verb 002 §v2-enrichment-2 accepts (stub-by-default for
// harnesses with no liveness signal beyond Spawn). Adding the base methods to
// THIS interface would break every existing harness implementor, so the
// extension is realized additively via the manifest projection + the
// BaseProviderFromHarness bridge below, NOT by widening this interface.
type HarnessProvider interface {
	Provider
	Manifest() HarnessManifest
}

// BaseProviderFromHarness adapts a HarnessProvider into a BaseProvider so the
// host can administer a harness through the family-agnostic lifecycle without
// the harness implementing the base methods directly. It maps Provider.Shutdown
// onto the base Deactivate (the documented harness↔base lifecycle mapping
// above), no-ops Activate (construction already probed fail-fast), and reports
// always-ready Health. This is the bridge that proves the harness family
// formally extends BaseProvider while keeping every existing harness unchanged.
func BaseProviderFromHarness(h HarnessProvider) BaseProvider {
	return harnessBase{h: h}
}

type harnessBase struct {
	NoopLifecycle
	h HarnessProvider
}

func (b harnessBase) Base() ProviderBase { return b.h.Manifest().Base() }

func (b harnessBase) Deactivate(ctx context.Context) error { return b.h.Shutdown(ctx) }
