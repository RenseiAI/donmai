package worker

import (
	"encoding/json"
	"time"
)

// AgentRuntimeProviderCapabilities mirrors the TypeScript AgentProviderCapabilities
// struct (packages/core/src/providers/types.ts) and is used for typed capability
// advertisement during worker registration (REN-1282).
//
// The orchestrator prefers CapabilitiesTyped over the legacy Capabilities []string
// field when both are present; the string slice is retained for backward
// compatibility with coordinators that have not yet been updated.
//
// Field names use JSON camelCase to match the TypeScript wire format.
type AgentRuntimeProviderCapabilities struct {
	// SupportsMessageInjection indicates whether injectMessage() works for
	// this provider (stateful providers: Claude, A2A).
	SupportsMessageInjection bool `json:"supportsMessageInjection"`
	// SupportsSessionResume indicates whether resume() can continue a prior
	// session.
	SupportsSessionResume bool `json:"supportsSessionResume"`
	// SupportsToolPlugins indicates whether the provider can use MCP tool
	// plugins delivered via stdio servers (af_linear_*, af_code_*).
	SupportsToolPlugins bool `json:"supportsToolPlugins,omitempty"`
	// NeedsBaseInstructions indicates that the provider requires persistent
	// base instructions via AgentSpawnConfig.baseInstructions.
	NeedsBaseInstructions bool `json:"needsBaseInstructions,omitempty"`
	// NeedsPermissionConfig indicates that the provider requires structured
	// permission config via AgentSpawnConfig.permissionConfig.
	NeedsPermissionConfig bool `json:"needsPermissionConfig,omitempty"`
	// SupportsCodeIntelligenceEnforcement indicates that the provider supports
	// canUseTool-style code intelligence enforcement.
	SupportsCodeIntelligenceEnforcement bool `json:"supportsCodeIntelligenceEnforcement,omitempty"`
	// ToolPermissionFormat is the tool permission format used by this provider
	// (one of "claude", "codex", "spring-ai"). Empty string means default.
	ToolPermissionFormat string `json:"toolPermissionFormat,omitempty"`
	// EmitsSubagentEvents indicates whether the provider emits Anthropic-style
	// subagent events (e.g. Task tool progress). Used by the Topology view.
	// Only true for the Claude provider; Codex and Spring AI have no equivalent
	// emission today.
	EmitsSubagentEvents bool `json:"emitsSubagentEvents"`
	// HumanLabel is the human-readable display name for this provider family
	// (e.g. "Claude", "Codex", "Spring AI"). Used in UI and log messages.
	HumanLabel string `json:"humanLabel,omitempty"`
}

// RegisterRequest is the body of POST /api/workers/register. It is sent with
// the provisioning token (rsp_live_...) in the Authorization header and
// describes the worker that is coming online.
type RegisterRequest struct {
	// Hostname is the machine hostname reported by the worker process.
	Hostname string `json:"hostname"`
	// PID is the operating system process id of the worker.
	PID int `json:"pid"`
	// Version is the worker binary version string (semver or git sha).
	Version string `json:"version"`
	// Capabilities is the list of capability tags this worker advertises
	// (e.g. "claude", "codex"). Empty when the worker has no special tags.
	// Deprecated: prefer CapabilitiesTyped when available; this field is
	// retained for backward compatibility with older coordinators.
	Capabilities []string `json:"capabilities,omitempty"`
	// CapabilitiesTyped carries a fully-typed capability struct for this
	// worker's agent runtime provider. When present the orchestrator uses this
	// in preference to the untyped Capabilities slice. Nil when the worker
	// has not been updated to advertise typed capabilities.
	CapabilitiesTyped *AgentRuntimeProviderCapabilities `json:"capabilities_typed,omitempty"`
	// MaxAgents is the maximum number of concurrent agent sessions this
	// worker will run. Zero means unspecified/default.
	MaxAgents int `json:"max_agents,omitempty"`
}

// ResolveCapabilities returns a summary of the effective capability tags for
// this registration request, implementing the "prefer typed when present"
// migration path (REN-1282 / ADR-002).
//
// If CapabilitiesTyped is non-nil the orchestrator should use it as the
// authoritative source. The legacy Capabilities slice is still returned for
// logging and backward-compatible coordinator behaviour.
//
// Returns the typed struct (or nil) and the effective string tags to use.
// When CapabilitiesTyped is present the string tags are derived from the
// typed struct; otherwise the original Capabilities slice is returned
// unchanged.
func (r *RegisterRequest) ResolveCapabilities() (*AgentRuntimeProviderCapabilities, []string) {
	if r.CapabilitiesTyped != nil {
		// Derive a minimal string tag set from the typed struct for coordinators
		// that still read the string slice.
		var tags []string
		if r.CapabilitiesTyped.HumanLabel != "" {
			// Normalise to lower-case for the tag (e.g. "Spring AI" → "spring-ai"
			// would require package-level mapping; use the raw label here and let
			// the coordinator normalise if needed).
			tags = append(tags, r.CapabilitiesTyped.HumanLabel)
		}
		// Merge any explicit legacy tags the caller may have set.
		for _, t := range r.Capabilities {
			tags = append(tags, t)
		}
		return r.CapabilitiesTyped, tags
	}
	return nil, r.Capabilities
}

// RegisterResponse is the response body from POST /api/workers/register.
// The coordinator returns a short-lived runtime JWT that must be used for
// subsequent Poll/Heartbeat calls, together with the assigned worker id
// and the cadence the worker should heartbeat at.
type RegisterResponse struct {
	// WorkerID is the coordinator-assigned identifier for this worker.
	WorkerID string `json:"worker_id"`
	// RuntimeJWT is the short-lived bearer token used for all subsequent
	// Poll and Heartbeat calls.
	RuntimeJWT string `json:"runtime_jwt"`
	// HeartbeatIntervalSeconds is the cadence at which the coordinator
	// expects heartbeats, expressed in seconds. Use HeartbeatInterval to
	// obtain a time.Duration.
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
}

// HeartbeatInterval returns the heartbeat cadence as a time.Duration.
func (r RegisterResponse) HeartbeatInterval() time.Duration {
	return time.Duration(r.HeartbeatIntervalSeconds) * time.Second
}

// PollRequest describes optional query parameters for GET /api/workers/{id}/poll.
// The endpoint uses GET with no body today; this struct is declared for
// forward compatibility and is unused by the current client helpers.
type PollRequest struct {
	// MaxItems caps the number of work items the coordinator may return
	// in a single poll. Zero lets the server pick a default.
	MaxItems int `json:"max_items,omitempty"`
}

// PollResponse is the response body from GET /api/workers/{id}/poll. It carries
// the batch of work items the coordinator has assigned to this worker
// since the last poll (possibly empty).
type PollResponse struct {
	// WorkItems is the batch of work items assigned to the worker. May be
	// empty when the coordinator has no pending work.
	WorkItems []WorkItem `json:"work_items"`
}

// HeartbeatRequest is the body of POST /api/workers/{id}/heartbeat. It reports
// the worker's current liveness signal back to the coordinator.
type HeartbeatRequest struct {
	// ActiveAgentCount is the number of agent sessions this worker is
	// currently running.
	ActiveAgentCount int `json:"active_agent_count"`
	// Status is an optional free-form status label (e.g. "idle", "busy",
	// "draining"). May be empty.
	Status string `json:"status,omitempty"`
}

// HeartbeatResponse is the response body from POST /api/workers/{id}/heartbeat.
// It is intentionally minimal today; a non-error status code is the
// acknowledgement.
type HeartbeatResponse struct {
	// Ack is true when the coordinator accepted the heartbeat. The field
	// is optional in the wire format and defaults to the zero value when
	// the coordinator returns an empty body.
	Ack bool `json:"ack,omitempty"`
}

// WorkItem is a single unit of work handed to a worker by the coordinator.
// The Payload is opaque to this package and is forwarded verbatim to the
// agent runner that knows how to interpret the given Type.
type WorkItem struct {
	// ID is the coordinator-assigned identifier for the work item.
	ID string `json:"id"`
	// Type is the work item kind (e.g. "session.start", "session.stop").
	Type string `json:"type"`
	// Payload is the opaque, type-specific payload. Kept as
	// json.RawMessage so the worker package does not need to know the
	// shape of every work item kind.
	Payload json.RawMessage `json:"payload"`
	// CreatedAt is the server-side timestamp at which the work item was
	// created. Encoded as RFC3339/ISO8601 by the default time.Time JSON
	// marshaler.
	CreatedAt time.Time `json:"created_at"`
}
