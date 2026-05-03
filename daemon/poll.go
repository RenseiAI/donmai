package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PollWorkItem mirrors one element of the platform's poll response `work[]`
// array. The platform serves GET /api/workers/<id>/poll and returns:
//
//	{
//	  work: QueuedWork[],
//	  inboxMessages: { [sessionId]: InboxMessage[] },
//	  hasInboxMessages: boolean,
//	  preClaimed: boolean,
//	  claimedSessionIds: string[],
//	  gitCredentials: { token, cloneUrl, expiresAt }[],
//	}
//
// QueuedWork carries the session-spec fields the daemon needs to dispatch a
// session to the spawner. Field names follow the platform wire shape (camelCase).
//
// QueuedAt is a Unix-millisecond epoch number on the wire — the platform's
// QueuedWork interface (packages/agentfactory-server work-queue.ts) defines it
// as `queuedAt: number`, and the Redis-stored session payload confirms a
// numeric value (e.g. 1777658441780). v0.4.1 mistakenly typed it as `string`,
// which caused the daemon's poll loop to fail decoding ("cannot unmarshal
// number into Go struct field PollWorkItem.work.queuedAt of type string") and
// silently drop pre-claimed sessions.
type PollWorkItem struct {
	SessionID    string            `json:"sessionId"`
	ProjectName  string            `json:"projectName,omitempty"`
	Repository   string            `json:"repository,omitempty"`
	Ref          string            `json:"ref,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	MaxDuration  int               `json:"maxDurationSeconds,omitempty"`
	Resources    *SessionResources `json:"resources,omitempty"`
	QueuedAt     int64             `json:"queuedAt,omitempty"`
	ProjectScope string            `json:"projectScope,omitempty"`

	// REN-1461 / F.2.8 — enriched fields the platform may send so the
	// `af agent run` worker has the runner context it needs without
	// requiring a separate platform fetch. Optional during the rollout
	// window; absent fields fall through to the default render path.
	IssueID           string                  `json:"issueId,omitempty"`
	IssueIdentifier   string                  `json:"issueIdentifier,omitempty"`
	LinearSessionID   string                  `json:"linearSessionId,omitempty"`
	ProviderSessionID string                  `json:"providerSessionId,omitempty"`
	OrganizationID    string                  `json:"organizationId,omitempty"`
	WorkType          string                  `json:"workType,omitempty"`
	PromptContext     string                  `json:"promptContext,omitempty"`
	Body              string                  `json:"body,omitempty"`
	Title             string                  `json:"title,omitempty"`
	MentionContext    string                  `json:"mentionContext,omitempty"`
	ParentContext     string                  `json:"parentContext,omitempty"`
	Branch            string                  `json:"branch,omitempty"`
	ResolvedProfile   *SessionResolvedProfile `json:"resolvedProfile,omitempty"`

	// REN-1485 / REN-1487 Phase 2 stage-driven SDLC fields. Populated
	// by the platform's `agent.dispatch_stage` action; absent when the
	// work was queued by the legacy `agent.dispatch_to_queue` action.
	// Round-trip opaquely on the QueuedWork JSON; the daemon forwards
	// them onto SessionDetail without interpreting them.
	StagePrompt        string           `json:"stagePrompt,omitempty"`
	StageID            string           `json:"stageId,omitempty"`
	StageBudget        *PollStageBudget `json:"stageBudget,omitempty"`
	StageLifecycle     map[string]any   `json:"stageLifecycle,omitempty"`
	StageSourceEventID string           `json:"stageSourceEventId,omitempty"`
}

// PollStageBudget mirrors the platform's StageBudget shape so the
// daemon can decode + forward it without depending on the runner
// package (cardinal package-architecture rule: daemon does not import
// runner). The runner re-types this into prompt.StageBudget when it
// constructs the QueuedWork. (REN-1485 / REN-1487.)
type PollStageBudget struct {
	MaxDurationSeconds int   `json:"maxDurationSeconds,omitempty"`
	MaxSubAgents       int   `json:"maxSubAgents,omitempty"`
	MaxTokens          int64 `json:"maxTokens,omitempty"`
}

// PollResponse is the body of GET /api/workers/<id>/poll. Only the fields the
// daemon currently consumes are decoded; unknown fields are ignored.
type PollResponse struct {
	Work              []PollWorkItem `json:"work"`
	HasInboxMessages  bool           `json:"hasInboxMessages,omitempty"`
	PreClaimed        bool           `json:"preClaimed,omitempty"`
	ClaimedSessionIDs []string       `json:"claimedSessionIds,omitempty"`
}

// PollHTTPError is returned by callPollEndpoint for non-2xx responses so the
// loop can branch on the HTTP status (401 → re-register).
type PollHTTPError struct {
	Status int
	Body   string
}

func (e *PollHTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// PollOptions configure a single poll loop run.
type PollOptions struct {
	WorkerID        string
	OrchestratorURL string
	RuntimeJWT      string
	IntervalSeconds int

	// HTTPClient is the client used for poll calls. Defaults to a 30s-timeout
	// http.Client.
	HTTPClient *http.Client
	// LogWarn is called for transient poll failures. Defaults to no-op.
	LogWarn func(format string, args ...any)
	// LogInfo is called when work is dispatched / re-register fires.
	LogInfo func(format string, args ...any)
	// OnWork is invoked for each item returned in the work[] slice. Errors are
	// logged at warn and do not stop the loop. Required.
	OnWork func(item PollWorkItem) error
	// OnReregister is called on HTTP 401 (runtime JWT expired) or 404 (worker
	// fell out of Redis). Implementations re-issue Register() and return the
	// fresh worker id + runtime token. The poll loop swaps credentials and
	// continues. Returning an error logs and the loop retries on the next tick.
	OnReregister func(ctx context.Context) (workerID, runtimeJWT string, err error)
}

// PollService manages the periodic poll goroutine. Like HeartbeatService it is
// safe to Start / Stop multiple times; consecutive Starts are idempotent.
type PollService struct {
	opts PollOptions

	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	workerID string // mutable: refreshed by OnReregister
	jwt      string // mutable: refreshed by OnReregister
}

// NewPollService constructs a PollService from opts. OnWork must be non-nil.
func NewPollService(opts PollOptions) *PollService {
	if opts.IntervalSeconds <= 0 {
		opts.IntervalSeconds = 5 // platform default in ms is 5000
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.LogWarn == nil {
		opts.LogWarn = func(string, ...any) {}
	}
	if opts.LogInfo == nil {
		opts.LogInfo = func(string, ...any) {}
	}
	return &PollService{
		opts:     opts,
		workerID: opts.WorkerID,
		jwt:      opts.RuntimeJWT,
	}
}

// Start launches the poll goroutine. Subsequent calls are no-ops.
func (p *PollService) Start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.running = true
	p.mu.Unlock()

	go p.loop(ctx)
}

// Stop terminates the poll goroutine. Safe to call multiple times.
func (p *PollService) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.running = false
}

// IsRunning reports whether the poll goroutine is active.
func (p *PollService) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *PollService) loop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(p.opts.IntervalSeconds) * time.Second)
	defer tick.Stop()
	// Immediate first poll so a worker comes online and requests work without
	// waiting one full interval.
	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *PollService) pollOnce(ctx context.Context) {
	p.mu.Lock()
	workerID := p.workerID
	jwt := p.jwt
	p.mu.Unlock()
	if workerID == "" {
		return
	}
	resp, err := callPollEndpoint(ctx, p.opts.HTTPClient, p.opts.OrchestratorURL, workerID, jwt)
	if err == nil {
		if len(resp.Work) > 0 {
			p.opts.LogInfo("daemon poll: %d work item(s) received", len(resp.Work))
		}
		for _, item := range resp.Work {
			if herr := p.opts.OnWork(item); herr != nil {
				p.opts.LogWarn("poll handler error for session %s: %v", item.SessionID, herr)
			}
		}
		return
	}
	if isPollAuthFailure(err) && p.opts.OnReregister != nil {
		// Surface the structured [runtime-token] event mirroring the
		// heartbeat path — REN-1481 observers see one log line per
		// cycle on either path.
		reason := pollAuthFailureReason(err)
		slog.Info("[runtime-token]",
			"event", "auth-failure-detected",
			"path", "poll",
			"reason", reason,
		)
		p.opts.LogWarn("daemon poll rejected (%v) — refreshing runtime token (reason=%s)", err, reason)
		newWorkerID, newJWT, regErr := p.opts.OnReregister(ctx)
		if regErr != nil {
			p.opts.LogWarn("daemon poll runtime-token refresh failed: %v", regErr)
			return
		}
		p.mu.Lock()
		p.workerID = newWorkerID
		p.jwt = newJWT
		p.mu.Unlock()
		return
	}
	p.opts.LogWarn("daemon poll failed: %v", err)
}

// callPollEndpoint issues a GET against /api/workers/<id>/poll with the given
// runtime JWT and returns the decoded response. Non-2xx responses surface as
// *PollHTTPError so the loop can switch on the status.
func callPollEndpoint(ctx context.Context, client *http.Client, orchestratorURL, workerID, jwt string) (*PollResponse, error) {
	if workerID == "" {
		return nil, errors.New("no worker id")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimRight(orchestratorURL, "/") + "/api/workers/" + workerID + "/poll"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, &PollHTTPError{Status: res.StatusCode, Body: strings.TrimSpace(string(errBuf))}
	}
	var resp PollResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	return &resp, nil
}

// isPollAuthFailure returns true for HTTP statuses that indicate the runtime
// token must be refreshed via re-register: 401 (Unauthorized) and 404 (Worker
// not found — fell out of Redis after TTL).
func isPollAuthFailure(err error) bool {
	var hErr *PollHTTPError
	if errors.As(err, &hErr) {
		return hErr.Status == http.StatusUnauthorized || hErr.Status == http.StatusNotFound
	}
	return false
}

// pollAuthFailureReason mirrors heartbeat.authFailureReason for the
// poll path: classifies a 401/404 into a short structured reason for
// the [runtime-token] log line. Uses the platform's specific
// "Runtime token expired" message as the smoking-gun signal for the
// REN-1481 refresh path.
func pollAuthFailureReason(err error) string {
	var hErr *PollHTTPError
	if errors.As(err, &hErr) {
		switch hErr.Status {
		case http.StatusUnauthorized:
			if strings.Contains(hErr.Body, "Runtime token expired") {
				return "runtime-token-expired"
			}
			return "unauthorized"
		case http.StatusNotFound:
			return "worker-not-found"
		}
	}
	return "auth-failure"
}

// resolveProjectFromAllowlist looks up a daemon ProjectConfig by the value
// the platform sent as the poll-item project identifier (a Linear project
// slug, the GitHub URL, or a suffix-equivalent of either).
//
// The match logic mirrors WorkerSpawner.findProjectLocked (REN-1448) so
// the SessionDetail.repository the runner sees is the SAME entry the
// spawner will later validate the SessionSpec against:
//
//   - exact match on p.ID (the slug, e.g. "smoke-alpha")
//   - exact match on p.Repository (the URL)
//   - URL-suffixes ".../<id>" or ".../<repository>"
//
// Returns (nil, false) when the value is empty or no entry matches.
// (REN-1464 / v0.5.2.)
func resolveProjectFromAllowlist(value string, projects []ProjectConfig) (*ProjectConfig, bool) {
	if value == "" {
		return nil, false
	}
	for i := range projects {
		p := &projects[i]
		if p.ID == value ||
			p.Repository == value ||
			strings.HasSuffix(value, "/"+p.Repository) ||
			strings.HasSuffix(p.Repository, "/"+value) {
			return p, true
		}
	}
	return nil, false
}

// pollItemToSessionSpec maps a PollWorkItem to a SessionSpec the
// WorkerSpawner can dispatch.
//
// The platform's QueuedWork wire shape historically carried a
// projectName slug (e.g. "smoke-alpha") with no separate repository
// URL. The runner needs a clone target — a slug is not one. When the
// daemon's project allowlist matches the slug we substitute the URL
// from p.Repository so `git clone <repo>` actually targets a real URL
// instead of failing with "fatal: repository 'smoke-alpha' does not
// exist" (the v0.5.1 failure mode this v0.5.2 hotfix is for —
// REN-1463 / REN-1464).
//
// When no allowlist match exists we fall through to whatever the
// platform sent (preserving prior behaviour) and emit a Warn log so
// operators can see the misconfiguration. The downstream
// WorkerSpawner.findProjectLocked check will reject the spec at
// AcceptWork time, but the explicit log makes the resolution failure
// observable immediately at poll dispatch.
func pollItemToSessionSpec(item PollWorkItem, projects []ProjectConfig) SessionSpec {
	repo := item.Repository
	if repo == "" {
		repo = item.ProjectName
	}
	if proj, ok := resolveProjectFromAllowlist(repo, projects); ok {
		repo = proj.Repository
	} else if repo != "" {
		slog.Warn(
			"daemon poll: no allowlist match for projectName, falling back to as-given repo string (worker will fail clone unless this is a real URL)",
			"sessionId", item.SessionID,
			"projectName", item.ProjectName,
			"repository", item.Repository,
			"fallback", repo,
		)
	}
	return SessionSpec{
		SessionID:          item.SessionID,
		Repository:         repo,
		Ref:                item.Ref,
		Resources:          item.Resources,
		Env:                item.Env,
		MaxDurationSeconds: item.MaxDuration,
	}
}

// pollItemToSessionDetail constructs the SessionDetail payload `af agent
// run` will fetch from the daemon's HTTP API for the given poll item.
// platformURL + authToken + workerID come from the daemon's
// registration state; the issue-context fields come from the platform-
// supplied poll item (or are empty when absent during the rollout
// window).
//
// SessionDetail.Repository is resolved against the daemon's project
// allowlist using the SAME matcher as the WorkerSpawner (slug, URL, or
// URL-suffix). The runner uses this URL for `git clone` — a slug
// passed through unchanged would fail with "fatal: repository '<slug>'
// does not exist" (REN-1463 / REN-1464). When no match is found we
// fall back to whatever the platform sent and emit a Warn log so the
// fallback is visible in operator logs.
//
// SessionDetail.ProjectName is also normalised to the canonical
// allowlist `id` when a match is found, so downstream code that uses
// the project id (env vars, dashboards) sees a stable value.
func pollItemToSessionDetail(item PollWorkItem, projects []ProjectConfig, platformURL, authToken, workerID string) *SessionDetail {
	repo := item.Repository
	if repo == "" {
		repo = item.ProjectName
	}
	projectName := item.ProjectName
	if proj, ok := resolveProjectFromAllowlist(firstNonEmptyStr(item.ProjectName, item.Repository), projects); ok {
		repo = proj.Repository
		if proj.ID != "" {
			projectName = proj.ID
		}
	} else if repo != "" {
		slog.Warn(
			"daemon poll: no allowlist match for projectName, falling back to as-given repo string (worker will fail clone unless this is a real URL)",
			"sessionId", item.SessionID,
			"projectName", item.ProjectName,
			"repository", item.Repository,
			"fallback", repo,
		)
	}
	return &SessionDetail{
		SessionID:          item.SessionID,
		IssueID:            item.IssueID,
		IssueIdentifier:    item.IssueIdentifier,
		LinearSessionID:    item.LinearSessionID,
		ProviderSessionID:  item.ProviderSessionID,
		ProjectName:        projectName,
		OrganizationID:     item.OrganizationID,
		Repository:         repo,
		Ref:                item.Ref,
		WorkType:           item.WorkType,
		PromptContext:      item.PromptContext,
		Body:               item.Body,
		Title:              item.Title,
		MentionContext:     item.MentionContext,
		ParentContext:      item.ParentContext,
		Branch:             item.Branch,
		ResolvedProfile:    item.ResolvedProfile,
		WorkerID:           workerID,
		AuthToken:          authToken,
		PlatformURL:        platformURL,
		StagePrompt:        item.StagePrompt,
		StageID:            item.StageID,
		StageBudget:        item.StageBudget,
		StageLifecycle:     item.StageLifecycle,
		StageSourceEventID: item.StageSourceEventID,
	}
}

// firstNonEmptyStr returns the first non-empty string from values.
// Used by the allowlist resolver to prefer projectName (the slug) over
// the repository field when both are present, matching the platform's
// canonical wire shape.
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
