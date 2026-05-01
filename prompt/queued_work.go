package prompt

// QueuedWork is the input contract for prompt rendering. It mirrors the
// session payload the platform stores in Redis under
// "agent:session:<sessionId>" and serves to the daemon via
// GET /api/workers/<id>/poll. Field names follow the platform wire
// shape (camelCase JSON tags) and are kept compatible with any future
// afclient.QueuedWork mirror.
//
// Field set is the verbatim subset the prompt renderer consumes today;
// callers may pass values they have available and leave the rest empty.
//
// Source: legacy TS QueuedWork
// (../agentfactory/packages/server/src/work-queue.ts) and the live
// Redis session payload observed during F.2.7 verification (REN2-1).
type QueuedWork struct {
	// SessionID is the Rensei session UUID (e.g.
	// "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c"). It uniquely identifies
	// this session record on the platform side.
	SessionID string `json:"sessionId,omitempty"`

	// IssueID is the Linear issue UUID this session was triggered for.
	// May be empty for governor-generated sessions.
	IssueID string `json:"issueId,omitempty"`

	// IssueIdentifier is the human-readable Linear identifier
	// (e.g. "REN-1457"). Used in the user prompt header so the agent
	// knows which issue it is working on.
	IssueIdentifier string `json:"issueIdentifier,omitempty"`

	// LinearSessionID is the Linear-side agent-session id the platform
	// posts activities to. Distinct from SessionID — same value today,
	// but reserved as a separate field per the platform's wire shape.
	LinearSessionID string `json:"linearSessionId,omitempty"`

	// ProviderSessionID is the provider-native session id (Claude UUID,
	// Codex thread id) when this is a resume. Empty for a fresh spawn.
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// ProjectName is the canonical project identifier (Linear project
	// name). Used both for routing and as a context hint in the system
	// prompt so the agent knows which project it is operating in.
	ProjectName string `json:"projectName,omitempty"`

	// OrganizationID is the Rensei tenant UUID (e.g.
	// "org_ejkmv9ojdyifipydw5l1"). Surfaced in the system prompt so
	// templated org-aware instructions can render.
	OrganizationID string `json:"organizationId,omitempty"`

	// Repository is the git remote URL or owner/name slug the agent
	// should operate on. Empty for governor work types that do not
	// touch a repo (e.g. research-only on issue description).
	Repository string `json:"repository,omitempty"`

	// Ref is the base branch / ref the worktree was checked out at.
	Ref string `json:"ref,omitempty"`

	// WorkType is the work-type discriminant (e.g. "development",
	// "qa", "research"). Drives template selection in [Builder.Build].
	// Unknown values fall through to the development template.
	WorkType string `json:"workType,omitempty"`

	// PromptContext is the rendered Linear issue context block produced
	// by the platform-side dispatcher. Includes the <issue>, <user>,
	// <team>, <project>, <title>, <description> XML envelope. The
	// renderer embeds it verbatim into the user prompt — it already
	// carries the issue body, identifier, title, and project metadata.
	PromptContext string `json:"promptContext,omitempty"`

	// Body is the raw Linear issue description text. Optional; when
	// non-empty and PromptContext is empty, the renderer falls back to
	// composing a minimal context block from Body + IssueIdentifier.
	Body string `json:"body,omitempty"`

	// Title is the Linear issue title. Optional; used when Body is
	// present but PromptContext is empty.
	Title string `json:"title,omitempty"`

	// MentionContext is the optional user-mention text from the Linear
	// agent-session create event (e.g. "please take this on"). Surfaced
	// in the user prompt when present.
	MentionContext string `json:"mentionContext,omitempty"`

	// ParentContext is the optional parent-issue context block built by
	// the coordinator when this session is a sub-agent. Surfaced in the
	// user prompt when present.
	ParentContext string `json:"parentContext,omitempty"`
}
