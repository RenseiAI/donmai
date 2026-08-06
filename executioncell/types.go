package executioncell

import "github.com/RenseiAI/donmai/agent"

// ContractVersion is the only version accepted by this package's decoders.
const ContractVersion = "donmai.execution-cell/v1alpha1"

// ContractErrorCode classifies a closed-decoder failure.
type ContractErrorCode string

// Closed contract decoder error codes.
const (
	ErrorUnsupportedContractVersion ContractErrorCode = "unsupported_contract_version"
	ErrorUnknownField               ContractErrorCode = "unknown_field"
	ErrorUnknownDiscriminator       ContractErrorCode = "unknown_discriminator"
	ErrorMissingRequiredField       ContractErrorCode = "missing_required_field"
	ErrorInvalidReference           ContractErrorCode = "invalid_reference"
	ErrorSecretMaterialForbidden    ContractErrorCode = "secret_material_forbidden"
)

// HarnessRef identifies a harness implementation and exact contract version.
type HarnessRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// ModelRef identifies a model independently from its serving endpoint.
type ModelRef struct {
	ID     string `json:"id"`
	Author string `json:"author"`
}

// ServingEndpointRef identifies non-secret routing configuration.
type ServingEndpointRef struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Operator string `json:"operator"`
	Revision string `json:"revision"`
}

// AuthMechanism identifies how a runtime authenticates without carrying
// credentials. The endpoint package owns the canonical type; this alias keeps
// the execution-cell contract source-compatible.
type AuthMechanism = agent.AuthMechanism

// Supported authentication mechanisms.
const (
	AuthAPIKey         = agent.AuthAPIKey
	AuthOAuth          = agent.AuthOAuth
	AuthCLISession     = agent.AuthCLISession
	AuthServiceAccount = agent.AuthServiceAccount
	AuthFederated      = agent.AuthFederated
	AuthNone           = agent.AuthNone
)

// CommercialMode identifies how use of a binding is commercially accounted.
type CommercialMode string

// Supported commercial modes.
const (
	CommercialUsageBilled     CommercialMode = "usage_billed"
	CommercialSubscription    CommercialMode = "subscription"
	CommercialPlatformMetered CommercialMode = "platform_metered"
	CommercialSelfHosted      CommercialMode = "self_hosted"
	CommercialFree            CommercialMode = "free"
)

// AuthBindingScope identifies the resource scope of an auth binding.
type AuthBindingScope string

// Supported authentication binding scopes.
const (
	ScopeProcess  AuthBindingScope = "process"
	ScopeSession  AuthBindingScope = "session"
	ScopeHarness  AuthBindingScope = "harness"
	ScopeEndpoint AuthBindingScope = "endpoint"
	ScopeHost     AuthBindingScope = "host"
	ScopePool     AuthBindingScope = "pool"
	ScopeProject  AuthBindingScope = "project"
	ScopeOrg      AuthBindingScope = "org"
)

// AuthPortability identifies where an auth binding may be reused.
type AuthPortability string

// Supported authentication portability constraints.
const (
	Portable      AuthPortability = "portable"
	EndpointBound AuthPortability = "endpoint_bound"
	HarnessBound  AuthPortability = "harness_bound"
	HostBound     AuthPortability = "host_bound"
)

// AuthDelivery identifies the runtime boundary used to deliver an auth binding
// without carrying credential material in the execution-cell contract.
type AuthDelivery string

// Supported authentication delivery boundaries.
const (
	DeliveryEnvironment          AuthDelivery = "environment"
	DeliveryEndpointHeader       AuthDelivery = "endpoint_header"
	DeliveryBrokeredToken        AuthDelivery = "brokered_token"
	DeliveryHostCLIHomeReference AuthDelivery = "host_cli_home_reference"
	DeliveryPlatformGateway      AuthDelivery = "platform_gateway"
	DeliveryNone                 AuthDelivery = "none"
)

// AuthBindingRef identifies non-secret authentication and entitlement metadata.
type AuthBindingRef struct {
	ID             string           `json:"id"`
	Mechanism      AuthMechanism    `json:"mechanism"`
	CommercialMode CommercialMode   `json:"commercialMode"`
	Authority      string           `json:"authority"`
	BindingScope   AuthBindingScope `json:"bindingScope"`
	Portability    AuthPortability  `json:"portability"`
	Delivery       AuthDelivery     `json:"delivery"`
}

// PlacementKind identifies the class of execution placement.
type PlacementKind string

// Supported placement kinds.
const (
	PlacementHost       PlacementKind = "host"
	PlacementPool       PlacementKind = "pool"
	PlacementSandbox    PlacementKind = "sandbox"
	PlacementRemotePeer PlacementKind = "remote_peer"
)

// PlacementResolution identifies whether placement is exact or resolved at claim.
type PlacementResolution string

// Supported placement resolution modes.
const (
	PlacementExact      PlacementResolution = "exact"
	PlacementClaimBound PlacementResolution = "claim_bound"
)

// PlacementRef identifies an execution host, pool, sandbox, or remote peer.
type PlacementRef struct {
	ID         string              `json:"id"`
	Kind       PlacementKind       `json:"kind"`
	Resolution PlacementResolution `json:"resolution"`
}

// SessionMode identifies autonomous or human-controlled execution.
type SessionMode string

// Supported session modes.
const (
	SessionAutonomous      SessionMode = "autonomous"
	SessionHumanControlled SessionMode = "human_controlled"
)

// CapabilityRequest names a requested capability and optional parameter digest.
type CapabilityRequest struct {
	Name             string `json:"name"`
	ParametersDigest string `json:"parametersDigest,omitempty"`
}

// CapabilityRequirement is the canonical ADR spelling.
type CapabilityRequirement = CapabilityRequest

// FallbackAlternative names one explicitly permitted axis substitution.
type FallbackAlternative struct {
	ID          string              `json:"id"`
	Harness     *HarnessRef         `json:"harness,omitempty"`
	Model       *ModelRef           `json:"model,omitempty"`
	Endpoint    *ServingEndpointRef `json:"endpoint,omitempty"`
	AuthBinding *AuthBindingRef     `json:"authBinding,omitempty"`
	Placement   *PlacementRef       `json:"placement,omitempty"`
}

// FallbackPolicy contains only the alternatives explicitly allowed by a caller.
type FallbackPolicy []FallbackAlternative

// DispatchIntent is the caller's independent execution-axis request.
type DispatchIntent struct {
	ContractVersion      string                  `json:"contractVersion"`
	RequestID            string                  `json:"requestId"`
	Harness              *HarnessRef             `json:"harness,omitempty"`
	Model                ModelRef                `json:"model"`
	Endpoint             *ServingEndpointRef     `json:"endpoint,omitempty"`
	AuthBinding          *AuthBindingRef         `json:"authBinding,omitempty"`
	Placement            *PlacementRef           `json:"placement,omitempty"`
	SessionMode          SessionMode             `json:"sessionMode"`
	RequiredCapabilities []CapabilityRequirement `json:"requiredCapabilities"`
	OptionalCapabilities []CapabilityRequirement `json:"optionalCapabilities"`
	FallbackAlternatives FallbackPolicy          `json:"fallbackAlternatives"`
}

// EvidenceTier identifies the strongest evidence supporting a resolved cell.
type EvidenceTier string

// Supported evidence tiers, ordered conceptually from declaration to production.
const (
	EvidenceDeclared            EvidenceTier = "declared"
	EvidenceImplemented         EvidenceTier = "implemented"
	EvidenceUnitVerified        EvidenceTier = "unit_verified"
	EvidenceIntegrationVerified EvidenceTier = "integration_verified"
	EvidenceSmoked              EvidenceTier = "smoked"
	EvidenceProductionEligible  EvidenceTier = "production_eligible"
)

// ResolvedExecutionCell is the exact execution identity known before enqueue.
type ResolvedExecutionCell struct {
	ContractVersion        string                  `json:"contractVersion"`
	Harness                HarnessRef              `json:"harness"`
	Model                  ModelRef                `json:"model"`
	Endpoint               ServingEndpointRef      `json:"endpoint"`
	AuthBinding            AuthBindingRef          `json:"authBinding"`
	Placement              PlacementRef            `json:"placement"`
	SessionMode            SessionMode             `json:"sessionMode"`
	GrantedCapabilities    []CapabilityRequirement `json:"grantedCapabilities"`
	EvidenceTier           EvidenceTier            `json:"evidenceTier"`
	CompatibilityDigest    string                  `json:"compatibilityDigest"`
	RuntimeInventoryDigest string                  `json:"runtimeInventoryDigest"`
}

// AdmissionDenialCode classifies a denied pre-enqueue admission.
type AdmissionDenialCode string

// Supported admission denial codes.
const (
	DenialUnsupportedContractVersion AdmissionDenialCode = "unsupported_contract_version"
	DenialUnknownHarness             AdmissionDenialCode = "unknown_harness"
	DenialUnsupportedHarnessVersion  AdmissionDenialCode = "unsupported_harness_version"
	DenialUnknownModel               AdmissionDenialCode = "unknown_model"
	DenialUnknownEndpoint            AdmissionDenialCode = "unknown_endpoint"
	DenialUnknownAuthBinding         AdmissionDenialCode = "unknown_auth_binding"
	DenialUnknownPlacement           AdmissionDenialCode = "unknown_placement"
	DenialUnsupportedSessionMode     AdmissionDenialCode = "unsupported_session_mode"
	DenialHarnessUnavailable         AdmissionDenialCode = "harness_unavailable"
	DenialEndpointUnreachable        AdmissionDenialCode = "endpoint_unreachable"
	DenialAuthUnavailable            AdmissionDenialCode = "auth_unavailable"
	DenialPlacementUnsatisfied       AdmissionDenialCode = "placement_unsatisfied"
	DenialCapabilityUnsupported      AdmissionDenialCode = "capability_unsupported"
	DenialEvidenceInsufficient       AdmissionDenialCode = "evidence_insufficient"
	DenialFallbackNotAllowed         AdmissionDenialCode = "fallback_not_allowed"
)

// ResolverDecisionKind identifies why an axis or capability was selected.
type ResolverDecisionKind string

// Supported resolver decision kinds.
const (
	DecisionExplicit        ResolverDecisionKind = "explicit"
	DecisionDefault         ResolverDecisionKind = "default"
	DecisionInheritance     ResolverDecisionKind = "inheritance"
	DecisionFallback        ResolverDecisionKind = "fallback"
	DecisionLegacyInference ResolverDecisionKind = "legacy_inference"
)

// ResolverDecision records explicit selection or visible inference provenance.
type ResolverDecision struct {
	Kind        ResolverDecisionKind `json:"kind"`
	Field       string               `json:"field"`
	SelectedRef string               `json:"selectedRef"`
	SourceRef   string               `json:"sourceRef,omitempty"`
	Reason      string               `json:"reason"`
}

// AdmissionDecision discriminates admitted and denied receipts.
type AdmissionDecision string

// Supported admission receipt decisions.
const (
	AdmissionAdmitted AdmissionDecision = "admitted"
	AdmissionDenied   AdmissionDecision = "denied"
)

// AdmissionReceipt is immutable evidence produced before enqueue.
type AdmissionReceipt struct {
	ContractVersion          string                 `json:"contractVersion"`
	ReceiptID                string                 `json:"receiptId"`
	RequestID                string                 `json:"requestId"`
	Decision                 AdmissionDecision      `json:"decision"`
	IntentDigest             string                 `json:"intentDigest"`
	OperationalPayloadDigest string                 `json:"operationalPayloadDigest"`
	Cell                     *ResolvedExecutionCell `json:"cell,omitempty"`
	DenialCode               AdmissionDenialCode    `json:"denialCode,omitempty"`
	DenialDetail             string                 `json:"denialDetail,omitempty"`
	ResolverDecisions        []ResolverDecision     `json:"resolverDecisions"`
	RecordedAt               string                 `json:"recordedAt"`
}

// ClaimDenialCode classifies a denied claim-time narrowing attempt.
type ClaimDenialCode string

// Supported claim denial codes.
const (
	ClaimConflict            ClaimDenialCode = "claim_conflict"
	ClaimHostIneligible      ClaimDenialCode = "host_ineligible"
	ClaimInventoryChanged    ClaimDenialCode = "inventory_changed"
	ClaimAuthUnavailable     ClaimDenialCode = "auth_unavailable"
	ClaimCapabilityRegressed ClaimDenialCode = "capability_regressed"
	ClaimEvidenceRegressed   ClaimDenialCode = "evidence_regressed"
)

// ClaimDecision discriminates claimed and denied claim receipts.
type ClaimDecision string

// Supported claim receipt decisions.
const (
	ClaimClaimed ClaimDecision = "claimed"
	ClaimDenied  ClaimDecision = "denied"
)

// ClaimReceipt records claim-time placement narrowing without mutating admission.
type ClaimReceipt struct {
	ContractVersion    string                 `json:"contractVersion"`
	ClaimReceiptID     string                 `json:"claimReceiptId"`
	AdmissionReceiptID string                 `json:"admissionReceiptId"`
	ClaimID            string                 `json:"claimId"`
	Decision           ClaimDecision          `json:"decision"`
	EffectiveCell      *ResolvedExecutionCell `json:"effectiveCell,omitempty"`
	DenialCode         ClaimDenialCode        `json:"denialCode,omitempty"`
	DenialDetail       string                 `json:"denialDetail,omitempty"`
	RecordedAt         string                 `json:"recordedAt"`
}

// SessionCapabilities are lifecycle operations supported by a session.
type SessionCapabilities struct {
	Watch       bool `json:"watch"`
	Replay      bool `json:"replay"`
	Cancel      bool `json:"cancel"`
	TakeControl bool `json:"takeControl"`
}

// SessionRef is the common lifecycle handle for every session mode.
type SessionRef struct {
	ContractVersion    string              `json:"contractVersion"`
	SessionID          string              `json:"sessionId"`
	AdmissionReceiptID string              `json:"admissionReceiptId"`
	ClaimReceiptID     string              `json:"claimReceiptId,omitempty"`
	Mode               SessionMode         `json:"mode"`
	Capabilities       SessionCapabilities `json:"capabilities"`
}

// DelegationTransport identifies how a parent delivers work to a child.
type DelegationTransport string

// Supported parent-child edge transports.
const (
	TransportNativeHarness    DelegationTransport = "native_harness"
	TransportPlatformDispatch DelegationTransport = "platform_dispatch"
	TransportA2A              DelegationTransport = "a2a"
	TransportHostCLI          DelegationTransport = "host_cli"
)

// DelegationEdgeIntent records transport on the edge, not the child identity.
type DelegationEdgeIntent struct {
	ContractVersion string              `json:"contractVersion"`
	EdgeID          string              `json:"edgeId"`
	Parent          SessionRef          `json:"parent"`
	ChildRequestID  string              `json:"childRequestId"`
	Transport       DelegationTransport `json:"transport"`
	InheritedFields []string            `json:"inheritedFields"`
	Detached        bool                `json:"detached"`
}
