package agent

import "context"

// This file declares Family B — the model-endpoint family — for the two-axis
// provider model (Phase 1). ModelEndpointProvider is a brand-new interface
// with no existing implementor; the company endpoint packages (under
// provider/endpoint/<company>/) are the only implementors, and they are all
// new files. Verbatim from 02-two-axis-architecture.md §2.3 (minus the
// Signature field, which is deferred to Phase 7).

// Company names a model-endpoint vendor (the SPEAK axis identity).
type Company string

// Company constants name each model-endpoint vendor.
const (
	CompanyAnthropic Company = "anthropic"
	CompanyOpenAI    Company = "openai"
	CompanyGoogle    Company = "google"
	CompanyLocal     Company = "local"
	CompanyStub      Company = "stub" // test-only, both axes
)

// AuthMechanism identifies how a runtime authenticates to an exact endpoint
// without carrying commercial-policy meaning. It is deliberately separate
// from the legacy AuthMode axis, which remains the accounting/entitlement
// selector used by existing model-access callers.
type AuthMechanism string

// Canonical endpoint authentication mechanisms.
const (
	AuthAPIKey         AuthMechanism = "api_key"
	AuthOAuth          AuthMechanism = "oauth"
	AuthCLISession     AuthMechanism = "cli_session"
	AuthServiceAccount AuthMechanism = "service_account"
	AuthFederated      AuthMechanism = "federated"
	AuthNone           AuthMechanism = "none"
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

// ModelDesc — one model a company serves, with the hosts it is reachable on.
type ModelDesc struct {
	ID               string        `json:"id"`
	HumanLabel       string        `json:"humanLabel"`
	ContextWindow    int           `json:"contextWindow"`
	SupportsTools    bool          `json:"supportsTools"`
	SupportsJSONMode bool          `json:"supportsJsonMode"`
	Hosts            []ServingHost `json:"hosts"`
}

// ModelEndpointManifest — company-named declaration. ContractABI =
// "model-endpoint/v1".
type ModelEndpointManifest struct {
	Company     Company        `json:"company"`
	HumanLabel  string         `json:"humanLabel"`
	Family      Family         `json:"family"` // FamilyModelEndpoint
	ContractABI string         `json:"contractAbi"`
	Speaks      []WireProtocol `json:"speaks"`
	Hosts       []HostDesc     `json:"hosts"`
	Models      []ModelDesc    `json:"models"`
}

// Base projects the model-endpoint manifest onto the family-agnostic
// ProviderBase header so ModelEndpointManifest satisfies BaseManifest — the
// additive realization of "every family manifest extends ProviderManifest<F>"
// (002 §"The base interface"). It does NOT change the wire layout (the
// projection is computed, not stored): ID is the within-family Company, Family
// is the declared discriminant, Name comes from HumanLabel, Version from the
// ContractABI, APIVersion from the base-contract constant, Scope defaults to
// global. Signature is nil today — signing is deferred for the two-axis
// manifests per ADR-2026-06-06.
func (m ModelEndpointManifest) Base() ProviderBase {
	return ProviderBase{
		APIVersion: ProviderAPIVersion,
		Family:     m.Family,
		ID:         string(m.Company),
		Name:       m.HumanLabel,
		Version:    m.ContractABI,
		Scope:      GlobalScope(),
		Stability:  StabilityStable,
	}
}

// Compile-time assertion: ModelEndpointManifest satisfies the base contract.
var _ BaseManifest = ModelEndpointManifest{}

// EndpointRequest — every field already RESOLVED FROM CONFIG.
type EndpointRequest struct {
	Model       string            `json:"model"`
	Host        ServingHost       `json:"host"`
	Auth        AuthMode          `json:"auth"`
	Region      string            `json:"region,omitempty"`
	EnvProvided map[string]string `json:"-"`
}

// EndpointBinding — the resolved cell. The currency crossing into
// Spec.Endpoint.
type EndpointBinding struct {
	Company  Company      `json:"company"`
	Model    string       `json:"model"`
	BaseURL  string       `json:"baseUrl"`
	Protocol WireProtocol `json:"protocol"`
	Host     ServingHost  `json:"host"`
	// Mechanism is the exact endpoint authentication protocol. Auth remains
	// alongside it for backwards-compatible commercial/access policy.
	Mechanism AuthMechanism `json:"mechanism,omitempty"`
	Auth      AuthMode      `json:"auth"`
	// Region is the serving region the binding was resolved for (the same
	// value templated into BaseURL). Harness read sites that must hand the
	// region to a CLI env knob (bedrock → AWS_REGION, vertex →
	// CLOUD_ML_REGION) or a resource path (vertex
	// projects/{p}/locations/{r}/publishers) read it here rather than
	// re-parsing the templated URL. Empty for region-less hosts
	// (direct/local/oauth-cli).
	Region        string    `json:"region,omitempty"`
	CostModel     CostModel `json:"costModel"`
	BringsOwnAuth bool      `json:"bringsOwnAuth"`
	// Env carries the resolved credential/config VALUES for the cell's
	// declared EnvKeys. json:"-" — values never cross the wire; bindings
	// that travel (QueuedWork payloads) deliver credentials via Spec.Env,
	// so harness read sites consult Endpoint.Env first, then Spec.Env.
	Env map[string]string `json:"-"`
}

// IsZero reports whether the binding is unset (today's behavior). Used by
// Spawn read sites to decide between Spec.Model and Endpoint.Model.
func (b EndpointBinding) IsZero() bool {
	return b.Company == "" && b.Model == "" && b.Protocol == "" && b.Mechanism == ""
}

// ModelEndpointProvider — Family B. Declare + resolve. No existing
// implementor; the company endpoint packages are the only ones.
//
// HOW THE MODEL-ENDPOINT FAMILY EXTENDS THE BASE CONTRACT (002 §"The base
// interface"). Like the harness family, the extension is at the MANIFEST level:
// ModelEndpointManifest.Base() projects onto ProviderBase, so the thin 9th
// family is administrable through the same family-agnostic header as every
// other family (002 §"Two-axis decomposition"). Resolve is the family's sole
// verb (002: "its sole verb is Resolve" — the accepted exception to the
// "families have user-facing verbs" norm). The base lifecycle is supplied
// additively by the BaseProviderFromEndpoint bridge below rather than by
// widening this interface, so the existing company endpoint packages stay
// unchanged.
type ModelEndpointProvider interface {
	Manifest() ModelEndpointManifest
	Resolve(ctx context.Context, req EndpointRequest) (EndpointBinding, error)
}

// BaseProviderFromEndpoint adapts a ModelEndpointProvider into a BaseProvider
// so the host can administer an endpoint through the family-agnostic lifecycle.
// A model endpoint holds no long-lived process (Resolve is a pure
// config→binding transform), so Activate/Deactivate are no-ops and Health is
// always-ready — the stub-by-default verdict 002 §v2-enrichment-2 accepts. This
// bridge proves the model-endpoint family formally extends BaseProvider while
// keeping every existing endpoint package unchanged.
func BaseProviderFromEndpoint(e ModelEndpointProvider) BaseProvider {
	return endpointBase{e: e}
}

type endpointBase struct {
	NoopLifecycle
	e ModelEndpointProvider
}

func (b endpointBase) Base() ProviderBase { return b.e.Manifest().Base() }
