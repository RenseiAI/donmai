package runner

import (
	"github.com/RenseiAI/donmai/agent"
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

	// ResolvedProfile carries the model-profile knobs the platform
	// resolved before queueing this work. The runner reads
	// ResolvedProfile.Provider to select which provider implementation
	// runs the session.
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
	// the model identity (e.g. "agy" for the Antigravity `agy` CLI-wrap).
	// When present it is AUTHORITATIVE for binary/provider selection: the
	// runner maps the harness token onto its concrete provider impl
	// regardless of the Provider value (so a catalog that models the
	// model as Provider="gemini" with Harness="agy" still resolves to the
	// agy-cli provider). Empty falls back to the Provider/Runner/default
	// chain — see [QueuedWork.resolvedProvider]. This lets the platform
	// drop the transitional Provider="agy-cli" wire token once every
	// runner reads Harness.
	Harness string `json:"harness,omitempty"`

	// Provider names the provider family that should run the session
	// (claude/codex/stub for v0.5.0). When empty the runner falls
	// back to the legacy `Runner` field, then to agent.ProviderClaude.
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
// as QueuedWork.Mode); the runner is the consumer.
const interactiveRunMode = "interactive"

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

// harnessToProvider maps a platform-wire harness token (the catalog's
// loop-driver attribute) onto the concrete provider impl that drives it.
// This is the harness-native selection seam: it lets the platform model a
// model as e.g. Provider="gemini" with Harness="agy" and still resolve to
// the agy-cli provider, so the platform can drop the transitional
// Provider="agy-cli" wire token.
//
// The token is the lowercase catalog attribute (e.g. "agy"), NOT the
// internal agent.HarnessName ("antigravity"); the two are deliberately
// distinct. An unrecognized token returns ("", false) so the caller falls
// back to the Provider/Runner/default chain — a forward-compatible default
// (a new harness token a stale runner doesn't know maps cleanly to its
// Provider field).
func harnessToProvider(harness string) (agent.ProviderName, bool) {
	switch harness {
	case "agy":
		return agent.ProviderAGYCLI, true
	default:
		return "", false
	}
}

// resolvedProvider returns the effective provider name for this
// QueuedWork.
//
// Selection order:
//  1. ResolvedProfile.Harness (when it maps to a known provider) — the
//     harness-native path; the catalog's loop-driver attribute is
//     authoritative over Provider.
//  2. ResolvedProfile.Provider — includes the legacy "agy-cli" alias the
//     platform still emits today (kept for one release so an in-flight
//     dispatch from a not-yet-updated platform cannot break a session).
//  3. ResolvedProfile.Runner (legacy field name).
//  4. agent.ProviderClaude (default).
func (q *QueuedWork) resolvedProvider() agent.ProviderName {
	if name, ok := harnessToProvider(q.ResolvedProfile.Harness); ok {
		return name
	}
	if q.ResolvedProfile.Provider != "" {
		return q.ResolvedProfile.Provider
	}
	if q.ResolvedProfile.Runner != "" {
		return agent.ProviderName(q.ResolvedProfile.Runner)
	}
	return agent.ProviderClaude
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

	// TerminalWorkareaLease is the path-free descriptor attached to the
	// successful terminal status after durable acquire + deferred teardown.
	TerminalWorkareaLease *workarea.TerminalLeaseDescriptor `json:"terminalWorkareaLease,omitempty"`
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
