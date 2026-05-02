package daemon

import (
	"sync"
)

// SessionDetail is the per-session payload `af agent run` reads from
// the daemon's local control HTTP API on spawn. It carries the full
// runner-side QueuedWork shape (issue context, resolved profile,
// branch) plus the platform-side credentials the runner needs to talk
// back (auth token, platform URL, worker id, lock id).
//
// The daemon stores one SessionDetail per accepted session in an
// in-memory map. A spawned `af agent run` process fetches its detail
// via GET /api/daemon/sessions/<id> at start-up.
//
// Wire shape: JSON, camelCase tags. Forward-compat — new fields can be
// added freely; clients ignore unknown fields.
type SessionDetail struct {
	// SessionID is the platform session UUID. Always populated.
	SessionID string `json:"sessionId"`

	// IssueID is the Linear issue UUID this session was triggered for.
	IssueID string `json:"issueId,omitempty"`

	// IssueIdentifier is the human-readable Linear identifier
	// (e.g. "REN-1457").
	IssueIdentifier string `json:"issueIdentifier,omitempty"`

	// LinearSessionID is the Linear-side agent-session id.
	LinearSessionID string `json:"linearSessionId,omitempty"`

	// ProviderSessionID is the provider-native session id when this
	// is a resume (e.g. Claude session UUID).
	ProviderSessionID string `json:"providerSessionId,omitempty"`

	// ProjectName is the canonical Linear project identifier.
	ProjectName string `json:"projectName,omitempty"`

	// OrganizationID is the Rensei tenant UUID.
	OrganizationID string `json:"organizationId,omitempty"`

	// Repository is the git URL (or owner/name slug) the agent should
	// operate on.
	Repository string `json:"repository,omitempty"`

	// Ref is the base branch / ref to check out from.
	Ref string `json:"ref,omitempty"`

	// WorkType is the workflow discriminant ("development", "qa",
	// "research", ...).
	WorkType string `json:"workType,omitempty"`

	// PromptContext is the rendered Linear issue context block produced
	// by the platform-side dispatcher.
	PromptContext string `json:"promptContext,omitempty"`

	// Body is the raw Linear issue description text.
	Body string `json:"body,omitempty"`

	// Title is the Linear issue title.
	Title string `json:"title,omitempty"`

	// MentionContext is the optional user-mention text from the Linear
	// agent-session create event.
	MentionContext string `json:"mentionContext,omitempty"`

	// ParentContext is the optional parent-issue context block built
	// by the coordinator when this session is a sub-agent.
	ParentContext string `json:"parentContext,omitempty"`

	// Branch is the working branch the agent should create/use.
	Branch string `json:"branch,omitempty"`

	// ResolvedProfile carries the model-profile knobs the platform
	// resolved before queueing this work. Daemon stores opaquely.
	ResolvedProfile *SessionResolvedProfile `json:"resolvedProfile,omitempty"`

	// WorkerID is the daemon worker id that claimed this session.
	WorkerID string `json:"workerId,omitempty"`

	// AuthToken is the runtime JWT the runner uses for platform API
	// calls (heartbeat, result post). Scoped to this worker.
	AuthToken string `json:"authToken,omitempty"`

	// PlatformURL is the base URL of the platform.
	PlatformURL string `json:"platformUrl,omitempty"`
}

// SessionResolvedProfile mirrors runner.ResolvedProfile but lives in
// the daemon package to avoid an import cycle (the daemon package must
// stay independent of the runner package — `af agent run` constructs
// its own runner from this opaque payload).
type SessionResolvedProfile struct {
	Provider       string         `json:"provider,omitempty"`
	Runner         string         `json:"runner,omitempty"`
	Model          string         `json:"model,omitempty"`
	Effort         string         `json:"effort,omitempty"`
	CredentialID   string         `json:"credentialId,omitempty"`
	ProviderConfig map[string]any `json:"providerConfig,omitempty"`
}

// sessionDetailStore holds the per-session payloads the daemon hands
// to spawned `af agent run` processes. Concurrent-safe; the spawner
// writes on AcceptWork and the HTTP server reads on
// /api/daemon/sessions/<id>.
type sessionDetailStore struct {
	mu      sync.RWMutex
	details map[string]*SessionDetail
}

// newSessionDetailStore returns an empty store.
func newSessionDetailStore() *sessionDetailStore {
	return &sessionDetailStore{details: make(map[string]*SessionDetail)}
}

// Set stores the detail under d.SessionID. Overwrites any prior entry.
func (s *sessionDetailStore) Set(d *SessionDetail) {
	if d == nil || d.SessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.details[d.SessionID] = d
}

// Get returns the detail for the given session id, or (nil, false)
// when absent.
func (s *sessionDetailStore) Get(id string) (*SessionDetail, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.details[id]
	return d, ok
}

// Delete removes the detail for the given session id (idempotent).
// Called by the daemon when a session terminates so stale auth tokens
// don't linger in memory.
func (s *sessionDetailStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.details, id)
}

// Len reports the number of currently-tracked sessions. Useful for
// dashboards + tests.
func (s *sessionDetailStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.details)
}
