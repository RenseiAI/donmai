package agent

import "context"

// This file lands the Go-native realization of the SDK BASE PROVIDER CONTRACT
// from donmai-architecture/002-provider-base-contract.md §"The base interface".
// It is the contract the 3rd-party provider SDK builds on, so it FREEZES the
// shape every plugin family extends: discovery (manifest), capability
// declaration, scope resolution, signing/trust, and the activate/deactivate/
// health lifecycle.
//
// The TS contract in 002 uses `Provider<F>`, `ProviderManifest<F>`,
// `ProviderCapabilities<F>`, `ProviderScope`, `ProviderSignature`, plus
// activate/deactivate/health. Go has no interface inheritance and no
// declaration-site bounded-by-value generics, so the realization is:
//
//   - ProviderFamily         — the 9-value family discriminant (002 §enum).
//   - ProviderManifest[F]     — generic, parameterized by the family-typed
//                               capabilities struct F. Embeds ProviderBase
//                               (the family-agnostic discovery + trust header).
//   - ProviderScope/Selector  — scope resolution (002 §"Scope resolution").
//   - ProviderSignature       — signing/trust (002 §"Signing and trust").
//   - ProviderHealth          — the lifecycle health verdict.
//   - BaseProvider            — the family-agnostic lifecycle interface
//                               (Family/Scope/Signature/Activate/Deactivate/
//                               Health) that every family interface extends.
//
// EVERYTHING HERE IS ADDITIVE. The legacy agent.Provider (the AgentRuntime
// loop-driver port) and the two-axis HarnessProvider / ModelEndpointProvider
// keep satisfying their existing interfaces unchanged; this file gives them a
// common base to formally embed without forcing any read-site or wire change.
// Per 002 Decision 3 ("Stay on v1") the contract is enriched in place — there
// is no apiVersion v2 bump.

// ProviderAPIVersion is the base-contract apiVersion every ProviderManifest
// carries (002 §Manifest, Decision 3 — the contract stays on v1 and absorbs
// additions in place until there are production users).
const ProviderAPIVersion = "rensei.dev/v1"

// ProviderFamily is the 9-value family discriminant from 002 §"The base
// interface". It is the STRING form a manifest declares; it is byte-compatible
// with the existing agent.Family value type (FamilyHarness == "agent-runtime",
// FamilyModelEndpoint == "model-endpoint", …) so the two never diverge.
//
// The two-axis code (wire.go) ships agent.Family with five constants. This
// type is the FULL enum 002 names; the helper IsKnownProviderFamily reports
// membership so a manifest cannot declare an off-roster family.
type ProviderFamily = Family

// The full 9-family roster from 002 §"The base interface". FamilyHarness,
// FamilyModelEndpoint, FamilySandbox, FamilyIssueTracker, and FamilyVersionCtrl
// are already declared in wire.go (the two-axis vocabulary); the four below
// complete the 002 enum so the base contract names every family a future
// provider SDK can target. They are reserved identifiers — no OSS implementor
// ships against them in this wave — but declaring them now keeps the family
// discriminant stable as peer families graduate to manifest-based discovery.
const (
	FamilyWorkarea      Family = "workarea"
	FamilyDeployment    Family = "deployment"
	FamilyAgentRegistry Family = "agent-registry"
	FamilyKit           Family = "kit"
)

// KnownProviderFamilies is the ordered 9-family roster (002 §"The base
// interface"). Ordering matches the doc's enum for deterministic iteration.
func KnownProviderFamilies() []ProviderFamily {
	return []ProviderFamily{
		FamilySandbox,
		FamilyWorkarea,
		FamilyHarness, // "agent-runtime" — 8th family, renamed surface "Harness"
		FamilyVersionCtrl,
		FamilyIssueTracker,
		FamilyDeployment,
		FamilyAgentRegistry,
		FamilyKit,
		FamilyModelEndpoint, // 9th family per ADR-2026-06-06
	}
}

// IsKnownProviderFamily reports whether f is one of the 9 families 002 names.
func IsKnownProviderFamily(f ProviderFamily) bool {
	for _, k := range KnownProviderFamilies() {
		if f == k {
			return true
		}
	}
	return false
}

// ProviderHealth is the optional health verdict (002 §"The base interface").
// A host may poll it to drop an unhealthy provider from rotation without
// restarting it.
type ProviderHealth struct {
	// Status is one of "ready" | "degraded" | "unhealthy".
	Status string `json:"status"`
	// Reason explains a non-ready status. Empty when Status == "ready".
	Reason string `json:"reason,omitempty"`
	// RecoverableAt is an optional hint for a "degraded" provider naming when
	// it may return to "ready" (RFC3339 string; empty when unknown).
	RecoverableAt string `json:"recoverableAt,omitempty"`
}

// ProviderHealth status constants.
const (
	HealthReady     = "ready"
	HealthDegraded  = "degraded"
	HealthUnhealthy = "unhealthy"
)

// HealthReadyVerdict is the canonical zero-cost "ready" verdict for providers
// with no meaningful liveness signal beyond Spawn (002 §v2-enrichment-2:
// "Stub-by-default for providers that have no meaningful liveness signal").
func HealthReadyVerdict() ProviderHealth { return ProviderHealth{Status: HealthReady} }

// ProviderSignature is the signing/trust descriptor from 002 §"Signing and
// trust". The OSS layer ships optional signing (permissive default); the SaaS
// control plane adds allowlist + attested modes. The whole manifest is hashed
// (canonical JSON) and that hash is what ManifestHash carries — sign the
// manifest, not the implementation.
//
// Phase note: per ADR-2026-06-06 the Signature field is deferred to a later
// phase for the two-axis manifests; this type lands the SHAPE now so the SDK
// base contract is frozen, and a manifest may carry a nil signature today
// (permissive default).
type ProviderSignature struct {
	// Signer is the signer identity (URL or DID).
	Signer string `json:"signer"`
	// PublicKey is the verifying key (PEM or multibase).
	PublicKey string `json:"publicKey"`
	// Algorithm is one of "sigstore" | "cosign" | "minisign" | "ed25519".
	Algorithm string `json:"algorithm"`
	// SignatureValue is base64-encoded.
	SignatureValue string `json:"signatureValue"`
	// ManifestHash is the canonical-JSON sha256 of the manifest this signature
	// attests (the "sign the manifest, not the implementation" rule).
	ManifestHash string `json:"manifestHash"`
	// AttestedAt is an RFC3339 timestamp.
	AttestedAt string `json:"attestedAt"`
	// Attestations carries signer-defined extensions (SLSA provenance,
	// in-toto, scan results, …). Optional.
	Attestations map[string]any `json:"attestations,omitempty"`
}

// ProviderSignature algorithm constants (002 §"Signing and trust").
const (
	SigSigstore = "sigstore"
	SigCosign   = "cosign"
	SigMinisign = "minisign"
	SigEd25519  = "ed25519"
)

// ProviderScope is the scope a provider is activated within (002 §"Scope
// resolution"). Four levels, most-specific wins.
type ProviderScope struct {
	// Level is one of "project" | "org" | "tenant" | "global".
	Level string `json:"level"`
	// Selector refines the scope. REQUIRED at non-global levels (002 rule 4:
	// an empty selector at a non-global level is invalid).
	Selector *ScopeSelector `json:"selector,omitempty"`
}

// ProviderScope level constants (002 §"Scope resolution"). Ordered most- to
// least-specific so a resolver can rank candidates by ScopeSpecificity.
const (
	ScopeProject = "project"
	ScopeOrg     = "org"
	ScopeTenant  = "tenant"
	ScopeGlobal  = "global"
)

// GlobalScope is the canonical global scope (no selector). It is the safe
// default for a statically-bundled OSS provider that applies everywhere.
func GlobalScope() ProviderScope { return ProviderScope{Level: ScopeGlobal} }

// ScopeSelector refines a non-global ProviderScope (002 §"Scope resolution").
// Matching is conjunctive across matchers, disjunctive within (002 rule 3).
type ScopeSelector struct {
	// Identity matchers.
	Project []string `json:"project,omitempty"`
	Org     []string `json:"org,omitempty"`
	Tenant  []string `json:"tenant,omitempty"`

	// Path matchers — for monorepo / per-project provider variation.
	Paths        []string `json:"paths,omitempty"`
	ExcludePaths []string `json:"excludePaths,omitempty"`

	// Conditional matchers — the provider only applies when these host-side
	// capabilities are present / when these other providers are active.
	RequiresCapability []string `json:"requiresCapability,omitempty"`
	RequiresProvider   []string `json:"requiresProvider,omitempty"`
}

// IsEmpty reports whether the selector declares no matchers. A non-global scope
// with an empty selector is invalid (002 rule 4); ValidateScope enforces it.
func (s *ScopeSelector) IsEmpty() bool {
	if s == nil {
		return true
	}
	return len(s.Project) == 0 && len(s.Org) == 0 && len(s.Tenant) == 0 &&
		len(s.Paths) == 0 && len(s.ExcludePaths) == 0 &&
		len(s.RequiresCapability) == 0 && len(s.RequiresProvider) == 0
}

// ScopeSpecificity ranks a scope for the "most-specific wins" rule (002 §"Scope
// resolution", rule 1). Higher == more specific: project=3, org=2, tenant=1,
// global=0. An unknown level ranks -1.
func ScopeSpecificity(level string) int {
	switch level {
	case ScopeProject:
		return 3
	case ScopeOrg:
		return 2
	case ScopeTenant:
		return 1
	case ScopeGlobal:
		return 0
	default:
		return -1
	}
}

// ValidateScope enforces 002's scope rules a host can check before activation:
// the level must be one of the four known levels, and a non-global level MUST
// carry a non-empty selector (rule 4). Returns ErrInvalidScope on violation.
func ValidateScope(scope ProviderScope) error {
	if ScopeSpecificity(scope.Level) < 0 {
		return ErrInvalidScope
	}
	if scope.Level != ScopeGlobal && scope.Selector.IsEmpty() {
		return ErrInvalidScope
	}
	return nil
}

// ProviderBase is the family-agnostic discovery + trust header every
// ProviderManifest embeds (002 §Manifest). It carries the fields that are the
// SAME across all nine families; the family-typed capabilities live on the
// embedding manifest's own struct (HarnessCaps, the endpoint Host/Model lists,
// …). Embedding (not a separate struct field) keeps the JSON flat so a single
// canonical-JSON hash signs the whole manifest as a unit.
type ProviderBase struct {
	// APIVersion is the base-contract version. Always ProviderAPIVersion
	// ("rensei.dev/v1") per Decision 3.
	APIVersion string `json:"apiVersion"`
	// Family is the 9-value family discriminant (002 §enum).
	Family ProviderFamily `json:"family"`
	// ID is globally unique WITHIN the family.
	ID string `json:"id"`
	// Version is the provider semver.
	Version string `json:"version"`
	// Name is the human-readable provider name.
	Name string `json:"name"`
	// Description is optional human prose.
	Description string `json:"description,omitempty"`

	// Origin metadata — used for discovery and trust (002 §Manifest).
	Author         string `json:"author,omitempty"`
	AuthorIdentity string `json:"authorIdentity,omitempty"`
	Homepage       string `json:"homepage,omitempty"`
	License        string `json:"license,omitempty"`
	Repository     string `json:"repository,omitempty"`

	// Scope is the scope this provider activates within. The zero value
	// (Level=="") is read as GlobalScope by ManifestScope so a bundled
	// provider need not spell it out.
	Scope ProviderScope `json:"scope,omitempty"`

	// Signature attests the manifest hash. nil == unsigned (the OSS permissive
	// default; deferred for the two-axis manifests per ADR-2026-06-06).
	Signature *ProviderSignature `json:"signature,omitempty"`

	// Stability is the provider stability tier (002 §A: "stable" | "beta" |
	// "unstable" | "registration-only"). Empty defaults to "stable".
	Stability string `json:"stability,omitempty"`

	// MetricsPrefix / LogScope are tooling/observability hints (002 §Manifest).
	MetricsPrefix string `json:"metricsPrefix,omitempty"`
	LogScope      string `json:"logScope,omitempty"`
}

// ProviderStability tier constants (002 §A "Stability tier declaration").
const (
	StabilityStable           = "stable"
	StabilityBeta             = "beta"
	StabilityUnstable         = "unstable"
	StabilityRegistrationOnly = "registration-only"
)

// Base returns the receiver so a *ProviderBase value satisfies the
// BaseManifest interface (below) by embedding alone. A manifest that embeds
// ProviderBase therefore gains BaseManifest for free — that embedding is the
// Go realization of "every family manifest extends ProviderManifest<F>".
func (b ProviderBase) Base() ProviderBase { return b }

// BaseManifest is the interface every family manifest satisfies once it embeds
// ProviderBase. It lets a host read the family-agnostic discovery + trust
// header (and therefore administer, scope, and verify a provider) WITHOUT
// knowing the family-typed capabilities. The parity test asserts every shipped
// manifest satisfies it.
type BaseManifest interface {
	Base() ProviderBase
}

// ManifestFamily reads the family discriminant from any manifest that embeds
// ProviderBase.
func ManifestFamily(m BaseManifest) ProviderFamily { return m.Base().Family }

// ManifestScope reads the effective scope, defaulting a zero-value scope to
// GlobalScope (the safe default for a statically-bundled OSS provider).
func ManifestScope(m BaseManifest) ProviderScope {
	s := m.Base().Scope
	if s.Level == "" {
		return GlobalScope()
	}
	return s
}

// ManifestStability reads the effective stability tier, defaulting an unset
// tier to "stable".
func ManifestStability(m BaseManifest) string {
	if s := m.Base().Stability; s != "" {
		return s
	}
	return StabilityStable
}

// ProviderManifest is the GENERIC base manifest from 002 §Manifest. F is the
// family-typed capabilities struct (HarnessCaps for the harness family, etc.).
// It embeds ProviderBase for the family-agnostic header and carries the
// declared, pre-load capabilities so the scheduler can reason about candidates
// without loading the implementation (002: "Capabilities are declared in the
// manifest, not at runtime").
//
// The two existing axis manifests (HarnessManifest, ModelEndpointManifest)
// remain their own concrete structs for wire back-compat; they gain a Base()
// method (a thin additive projection) so they satisfy BaseManifest without a
// field-layout change. ProviderManifest[F] is the canonical shape a NEW
// SDK-authored provider family targets directly.
type ProviderManifest[F any] struct {
	ProviderBase
	// CapabilitiesDeclared is the family-typed capability struct, declared
	// up-front (002 §Manifest "capabilitiesDeclared").
	CapabilitiesDeclared F `json:"capabilitiesDeclared"`
}

// BaseProvider is the family-agnostic lifecycle interface from 002 §"The base
// interface" — the Go realization of `Provider<F>`'s base methods. Every family
// interface (HarnessProvider, ModelEndpointProvider, and future SDK families)
// extends it. It deliberately carries ONLY what 002 says is on the base
// (declare-self via the manifest header, advertise scope, prove identity,
// attach to lifecycle); family-specific verbs (Spawn/Resolve/provision/…) live
// on the family interface, never here.
//
// activate/deactivate are idempotent (002): activate validates the environment
// and opens long-lived clients (throwing aborts activation; no family verb runs
// before it returns); deactivate releases them and must not throw on a second
// call. Health is optional — a provider with no meaningful liveness signal can
// return HealthReadyVerdict().
type BaseProvider interface {
	// BaseManifest exposes the family-agnostic discovery + trust header.
	BaseManifest

	// Activate is called once by the host at activation. Idempotent.
	Activate(ctx context.Context) error
	// Deactivate is called by the host at deactivation. Idempotent; must not
	// error on a second call.
	Deactivate(ctx context.Context) error
	// Health is the optional health check. Providers with no liveness signal
	// return HealthReadyVerdict().
	Health(ctx context.Context) (ProviderHealth, error)
}

// NoopLifecycle is an embeddable zero-value helper that gives a new
// SDK-authored provider the base lifecycle for FREE — idempotent no-op
// Activate/Deactivate and an always-ready Health (002 §v2-enrichment-2:
// "Stub-by-default for providers that have no meaningful liveness signal beyond
// Spawn"). A provider embeds it, supplies a Base() method (typically by also
// embedding the manifest's projection or returning it), and immediately
// satisfies BaseProvider:
//
//	type myProvider struct {
//	    agent.NoopLifecycle
//	    mf agent.ProviderManifest[MyCaps]
//	}
//	func (p *myProvider) Base() agent.ProviderBase { return p.mf.Base() }
//
// Providers that DO have a liveness signal override Health; providers that open
// long-lived clients override Activate/Deactivate. Embedding is opt-in: the
// existing two-axis providers keep their own lifecycle (the harness family uses
// the legacy Provider.Shutdown, which maps to Deactivate — see HarnessProvider
// below) and do NOT embed this, so the change is additive.
type NoopLifecycle struct{}

// Activate is the idempotent no-op activation.
func (NoopLifecycle) Activate(context.Context) error { return nil }

// Deactivate is the idempotent no-op deactivation.
func (NoopLifecycle) Deactivate(context.Context) error { return nil }

// Health reports always-ready (the stub-by-default verdict).
func (NoopLifecycle) Health(context.Context) (ProviderHealth, error) {
	return HealthReadyVerdict(), nil
}

// Base exposes the embedded ProviderBase header so ProviderManifest[F] itself
// satisfies BaseManifest directly (handy for a provider that embeds a
// ProviderManifest[F]).
func (m ProviderManifest[F]) Base() ProviderBase { return m.ProviderBase }
