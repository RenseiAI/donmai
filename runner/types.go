package runner

import (
	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
)

// QueuedWork is the runner's input contract — the per-session payload
// the daemon hands to [Runner.Run]. It embeds the prompt package's
// [prompt.QueuedWork] (which carries the issue/identifier/context the
// prompt builder consumes) and adds the runner-specific knobs the
// orchestrator needs (resolved profile, branch, worker id).
//
// Wire shape: matches the platform Redis session payload at
// "agent:session:<id>" verbatim. F.1.1 §1 + the live payload observed
// during F.2.7 (REN2-1) drive the field set.
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
	// "https://app.rensei.ai" or "http://127.0.0.1:3010"). The runner
	// forwards this to result.Poster + heartbeat.Pulser. Required.
	PlatformURL string `json:"-"`
}

// ResolvedProfile names the profile knobs the platform resolved for
// this session. Mirrors F.1.1 §4 ResolvedProfile shape.
//
// JSON tags follow the platform-side camelCase wire shape (consumed
// by the daemon poll handler).
type ResolvedProfile struct {
	// Provider names the provider family that should run the session
	// (claude/codex/stub for v0.5.0). When empty the runner falls
	// back to the legacy `Runner` field, then to agent.ProviderClaude.
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

// resolvedProvider returns the effective provider name for this
// QueuedWork, falling back through the Provider/Runner/default chain.
func (q *QueuedWork) resolvedProvider() agent.ProviderName {
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
	// "REN-1459"). Echoed for log correlation.
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
	// post-session block (REN-1467) — e.g. "Linear updateIssueStatus
	// failed: …" or "diagnostic comment post failed: …". These are
	// strictly observability — they do NOT change the session's
	// terminal Status. Surface them in operator dashboards so a
	// silently-failed transition is visible.
	PostSessionWarnings []string `json:"postSessionWarnings,omitempty"`

	// LinearStatusTransition records the result of the post-session
	// Linear status-update attempt (REN-1467). Empty when no
	// transition was attempted (non-result-sensitive type with no
	// mapping, or marker was unknown). Non-nil even on failure so the
	// caller can correlate dashboard signals to runner logs.
	LinearStatusTransition *LinearStatusTransition `json:"linearStatusTransition,omitempty"`
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
