package runner

import (
	"encoding/json"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// QueuedWork is the runner's input contract — the per-session payload
// the daemon hands to [Runner.Run]. It embeds the prompt package's
// [prompt.QueuedWork] (which carries the issue/identifier/context the
// prompt builder consumes) and adds the runner-specific knobs the
// orchestrator needs (resolved profile, branch, worker id).
//
// Wire shape: matches the platform Redis session payload at
// "agent:session:<id>" verbatim. F.1.1 §1 + the live payload observed
// during F.2.7 drive the field set.
type QueuedWork struct {
	prompt.QueuedWork

	// AdmissionReceipt is the platform-produced, immutable execution-cell
	// admission evidence. The daemon forwards it opaquely; the runner's closed
	// decoder validates it before selecting a harness. Keeping the raw bytes at
	// the wire boundary prevents an intermediate mirror from silently accepting
	// or dropping fields from a newer contract version.
	AdmissionReceipt json.RawMessage `json:"admissionReceipt,omitempty"`

	// ClaimReceipt is required when AdmissionReceipt pins a claim-bound pool.
	// It remains raw until the runner's closed decoder proves that the claim is
	// a narrow-only transition to the effective host cell.
	ClaimReceipt json.RawMessage `json:"claimReceipt,omitempty"`

	// EffectiveCell is the secret-free runtime projection that the worker says
	// it will actually execute. Receipt-bearing work must supply it even for an
	// exact admission; inference from ambient provider, endpoint, auth, or host
	// state is forbidden.
	EffectiveCell json.RawMessage `json:"effectiveCell,omitempty"`

	ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding,omitempty"`
	OperationalPayload      json.RawMessage `json:"operationalPayload,omitempty"`
	HostAdaptationReceipt   json.RawMessage `json:"hostAdaptationReceipt,omitempty"`

	// ResolvedProfile carries the model-profile knobs the platform
	// resolved before queueing this work. An explicit Harness is authoritative;
	// only the named legacy adapter may infer from Provider/Runner/defaults.
	ResolvedProfile ResolvedProfile `json:"resolvedProfile,omitempty"`

	// Branch is the working branch name the runner should use when
	// provisioning the worktree. Empty falls back to "agent/<sessionID>".
	Branch string `json:"branch,omitempty"`

	// WorkerID is the daemon worker that claimed this session. Used
	// for ownership probes inside the worktree retry loop and as the
	// {workerId} in the heartbeat refresh body. Required.
	WorkerID string `json:"workerId,omitempty"`

	// AuthToken is the worker's bearer token used for platform API
	// calls (heartbeat, result post). The daemon resolves this from
	// the registration store; the runner just forwards it.
	AuthToken string `json:"-"`

	// McpAuthToken is the platform-supplied bearer for the platform's
	// per-session MCP gateway, and for NOTHING else. AuthToken remains the
	// bearer for heartbeat, result-post, activity-post, and the session
	// preflight fetch; the two are not interchangeable in either direction.
	//
	// Empty means the platform minted none (self-hosted, or one that predates
	// the field), in which case the gateway falls back to AuthToken — see
	// mcpGatewayBearer. That fallback is the standalone contract, not a
	// migration shim.
	//
	// `json:"-"` mirrors AuthToken deliberately: QueuedWork is re-unmarshalled
	// from OperationalPayload and serialized onto other payloads, and a bearer
	// must not round-trip through those paths.
	McpAuthToken string `json:"-"`

	// McpAuthTokenExpiresAt is the RFC3339 UTC instant McpAuthToken stops being
	// accepted. ADVISORY ONLY — the runner logs it once at spawn so the cliff
	// is visible in advance, and never branches on it: it must not refuse a
	// spawn, shorten a session, or suppress the gateway. Excluded from JSON for
	// the same round-trip reason as the bearer it describes.
	McpAuthTokenExpiresAt string `json:"-"`

	// PlatformURL is the base URL of the platform (e.g.
	// "https://platform.example.com" or "http://127.0.0.1:3010"). The runner
	// forwards this to result.Poster + heartbeat.Pulser. Required.
	PlatformURL string `json:"-"`

	// TerminalWorkareaLease requests bounded retention of a successful workarea.
	// Nil preserves the legacy immediate-teardown behavior.
	TerminalWorkareaLease *workarea.TerminalLeaseRequest `json:"terminalWorkareaLease,omitempty"`

	// Capabilities carries the daemon-advertised worker capability flags for
	// the session, threaded from the daemon's SessionDetail. A capability is
	// PRESENT-and-true only when the daemon explicitly advertises it; a missing
	// key reads false. The runner uses this to gate behaviour that depends on a
	// daemon-side adapter being available — e.g. CapabilityMergeQueue gates the
	// acceptance merge-queue deferral in post_session.go. Nil/absent → every
	// capability is false, which is the mixed-version-safe default (an older
	// daemon that does not advertise capabilities keeps the prior behaviour).
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

// Capability keys advertised by the daemon on QueuedWork.Capabilities. Kept as
// typed constants so the producer (daemon SessionDetail) and the consumer
// (runner post-session) agree on the wire string and a typo can't silently
// disable a capability gate.
const (
	// CapabilityMergeQueue is true when the daemon provides a merge-queue
	// adapter, so a passing acceptance session should DEFER its
	// Delivered → Accepted promotion to the merge worker rather than
	// transitioning the issue directly. Default false (no adapter today) —
	// the platform owns the merge gate via the gate.merge workflow node.
	CapabilityMergeQueue = "merge-queue"
	// CapabilitySpanIngest is true when the dispatch target accepts the
	// additive span batch contract. Missing/false keeps mixed-version workers
	// from posting to a server that has not shipped the ingest route yet.
	CapabilitySpanIngest = "llm-span-ingest"
)

// hasCapability reports whether the daemon advertised the named capability as
// true. A nil map or a missing key returns false (mixed-version-safe default).
func (q *QueuedWork) hasCapability(name string) bool {
	if q.Capabilities == nil {
		return false
	}
	return q.Capabilities[name]
}

// ResolvedProfile names the profile knobs the platform resolved for
// this session. Mirrors F.1.1 §4 ResolvedProfile shape.
//
// JSON tags follow the platform-side camelCase wire shape (consumed
// by the daemon poll handler).
type ResolvedProfile struct {
	// Harness is the loop-driver attribute the platform catalog models on
	// the model identity (e.g. "antigravity", "gemini-direct", or "ollama";
	// legacy wire aliases such as "agy", "native", and "raw" remain accepted).
	// When present it is AUTHORITATIVE for binary/provider selection: the
	// runner maps the harness token onto its concrete provider impl
	// regardless of the Provider value (so a catalog that models the
	// model as Provider="gemini" with Harness="agy" still resolves to the
	// agy-cli provider). Empty is handled only by the named legacy harness
	// adapter. A non-empty value never falls through to Provider, Runner, a
	// default, or posterior routing.
	Harness string `json:"harness,omitempty"`

	// Provider names the provider family that should run the session
	// (claude/codex/stub for v0.5.0). When empty, only the named legacy
	// harness adapter falls back to `Runner`, then to agent.ProviderClaude.
	// Superseded by Harness when Harness is non-empty.
	Provider agent.ProviderName `json:"provider,omitempty"`

	// Runner is the legacy field name some platform deployments use
	// for the same value. The runner reads Provider first and falls
	// back to Runner so an in-flight wire-shape transition does not
	// break dispatch.
	Runner string `json:"runner,omitempty"`

	// Model identifies the model variant within the provider family
	// (e.g. "claude-sonnet-4-5"). Empty falls back to the provider
	// default.
	Model string `json:"model,omitempty"`

	// Effort is the normalized reasoning-effort tier the provider
	// should pass through to its native knob. Honored by providers
	// with SupportsReasoningEffort=true.
	Effort agent.EffortLevel `json:"effort,omitempty"`

	// CredentialID identifies the credential entry the daemon should
	// resolve into provider-native auth (e.g. ANTHROPIC_API_KEY) and
	// inject via Spec.Env.
	CredentialID string `json:"credentialId,omitempty"`

	// ProviderConfig carries provider-specific knobs from the matched
	// model profile. Forwarded into agent.Spec.ProviderConfig.
	ProviderConfig map[string]any `json:"providerConfig,omitempty"`

	// Endpoint is the resolved serving binding forwarded to agent.Spec.
	Endpoint *agent.EndpointBinding `json:"endpoint,omitempty"`
}

// interactiveRunMode is the QueuedWork.Mode value that activates the
// PTY-hosted interactive-session dispatch path (spawn-under-PTY + outbound
// relay attach, interactive-attach-v1). It is a DISTINCT mode from
// interview.InterviewRunMode: the interview loop is a park-and-inject
// model turn-taking loop with no PTY, whereas interactive mode runs a real
// terminal under a pseudo-terminal and attaches a live byte stream. The
// two must never be conflated — interactive must not trip the interview
// branch or inherit its thinking-only tool lockdown. The platform emits
// this literal on the wire (opaquely forwarded by the daemon SessionDetail
// as QueuedWork.Mode); the runner and the prompt renderer (which suppresses
// the work-type user template for interactive sessions) are the consumers.
// Aliased to the canonical prompt.InteractiveRunMode so the wire literal is
// spelled exactly once.
const interactiveRunMode = prompt.InteractiveRunMode

// isInterview reports whether this QueuedWork runs the interactive
// interview loop rather than the one-shot headless path. The
// discriminant is the platform-frozen Mode value (CONTRACT-FREEZE §4 /
// internal/interview.InterviewRunMode). Anything else (including the empty
// string) is a normal headless run.
func (q *QueuedWork) isInterview() bool {
	return q.Mode == interview.InterviewRunMode
}

// isInteractive reports whether this QueuedWork runs the PTY-hosted
// interactive-session dispatch path (interactiveRunMode). It mirrors
// isInterview and is mutually exclusive with it: a Mode value matches at
// most one branch, so an interactive dispatch never enters the interview
// loop (nor vice versa). Anything unrecognised (including the empty
// string) falls through both to the normal headless path.
func (q *QueuedWork) isInteractive() bool {
	return q.Mode == interactiveRunMode
}

// Result is the terminal output of a [Runner.Run] call.
//
// Today it is a thin alias around [agent.Result] with the addition of
// runner-internal fields (StartedAt, FinishedAt) and a direct field
// echo of the platform-relevant identifiers so callers do not have to
// thread the QueuedWork through their result handler. Forward-compat:
// new runner-wave hooks can extend Result without touching the
// agent/types.go contract.
type Result struct {
	agent.Result

	// HarnessRef is the independently admitted loop-driver identity. It is
	// distinct from Result.ProviderName, the legacy concrete registry key.
	HarnessRef *executioncell.HarnessRef `json:"harnessRef,omitempty"`

	// ResolverDecisions carries canonical explicit/default/legacy provenance
	// from the harness selector. The full admitted receipt remains upstream.
	ResolverDecisions []executioncell.ResolverDecision `json:"resolverDecisions,omitempty"`

	// AdmissionReceipt is populated on a local pre-spawn denial. It is the
	// canonical v1alpha1 denied receipt, never a runner-specific lookalike.
	AdmissionReceipt *executioncell.AdmissionReceipt `json:"admissionReceipt,omitempty"`

	// SessionID is the platform-side session UUID this result
	// belongs to. Echoed for caller convenience.
	SessionID string `json:"sessionId,omitempty"`

	// IssueIdentifier is the human-readable issue identifier (e.g.
	// "ENG-1459"). Echoed for log correlation.
	IssueIdentifier string `json:"issueIdentifier,omitempty"`

	// StartedAt is the unix-ms timestamp when [Runner.Run] entered
	// step 1 of the loop.
	StartedAt int64 `json:"startedAt,omitempty"`

	// FinishedAt is the unix-ms timestamp when [Runner.Run] returned
	// the Result (after teardown).
	FinishedAt int64 `json:"finishedAt,omitempty"`

	// SteeringTriggered reports whether tail-recovery stage 1 fired.
	SteeringTriggered bool `json:"steeringTriggered,omitempty"`

	// PostSessionWarnings collects non-fatal warnings raised by the
	// post-session block — e.g. "Linear updateIssueStatus
	// failed: …" or "diagnostic comment post failed: …". These are
	// strictly observability — they do NOT change the session's
	// terminal Status. Surface them in operator dashboards so a
	// silently-failed transition is visible.
	PostSessionWarnings []string `json:"postSessionWarnings,omitempty"`

	// LinearStatusTransition records the result of the post-session
	// Linear status-update attempt. Empty when no
	// transition was attempted (non-result-sensitive type with no
	// mapping, or marker was unknown). Non-nil even on failure so the
	// caller can correlate dashboard signals to runner logs.
	LinearStatusTransition *LinearStatusTransition `json:"linearStatusTransition,omitempty"`

	// BudgetReport captures the per-stage budget enforcement record.
	// Non-nil
	// for every Run; the .Enforced flag distinguishes
	// stage-dispatched work (caps configured) from legacy work
	// (caps absent). When a cap was breached .CapBreached + .BreachDetail
	// surface the reason; the session's Status is "failed" with
	// FailureMode=FailureBudgetExceeded.
	BudgetReport *BudgetReport `json:"budgetReport,omitempty"`

	// TerminalWorkareaLease is the exact four-field external projection attached
	// after the full descriptor and host-local path are durable.
	TerminalWorkareaLease *workarea.TerminalLeaseProjection `json:"terminalWorkareaLease,omitempty"`
}

// LinearStatusTransition records the runner's post-session attempt to
// transition the Linear issue's workflow state. Built from
// resolveTargetStatus and the UpdateIssueStatus call result.
type LinearStatusTransition struct {
	// WorkType is the agent work type the decision was made for.
	WorkType string `json:"workType,omitempty"`

	// WorkResult is the parsed marker driving the transition
	// ("passed" | "failed" | "unknown" | "").
	WorkResult string `json:"workResult,omitempty"`

	// TargetStatus is the Linear workflow-state name the runner
	// attempted to transition to. Empty when no transition was
	// attempted.
	TargetStatus string `json:"targetStatus,omitempty"`

	// Attempted is true when the runner called UpdateIssueStatus.
	Attempted bool `json:"attempted,omitempty"`

	// Succeeded is true when UpdateIssueStatus returned nil.
	Succeeded bool `json:"succeeded,omitempty"`

	// Reason is a short identifier from PostSessionDecision.Reason
	// ("passed", "failed", "unknown", "completed-non-sensitive",
	// "deferred-merge-queue", "no-mapping", ...).
	Reason string `json:"reason,omitempty"`

	// Error is the human-readable error message when the transition
	// failed. Empty on success.
	Error string `json:"error,omitempty"`

	// DiagnosticPosted is true when the runner posted the
	// "missing WORK_RESULT" diagnostic comment to Linear (i.e. the
	// Reason was "unknown" and the comment post succeeded).
	DiagnosticPosted bool `json:"diagnosticPosted,omitempty"`
}
