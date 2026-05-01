package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
type PollWorkItem struct {
	SessionID    string            `json:"sessionId"`
	ProjectName  string            `json:"projectName,omitempty"`
	Repository   string            `json:"repository,omitempty"`
	Ref          string            `json:"ref,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	MaxDuration  int               `json:"maxDurationSeconds,omitempty"`
	Resources    *SessionResources `json:"resources,omitempty"`
	QueuedAt     string            `json:"queuedAt,omitempty"`
	ProjectScope string            `json:"projectScope,omitempty"`
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
		p.opts.LogWarn("daemon poll rejected (%v) — re-registering", err)
		newWorkerID, newJWT, regErr := p.opts.OnReregister(ctx)
		if regErr != nil {
			p.opts.LogWarn("daemon poll re-register failed: %v", regErr)
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

// pollItemToSessionSpec maps a PollWorkItem to a SessionSpec the WorkerSpawner
// can dispatch. The repository fallback exists because some platform-emitted
// QueuedWork rows carry projectName as the canonical repo identifier rather
// than a separate repository field.
func pollItemToSessionSpec(item PollWorkItem) SessionSpec {
	repo := item.Repository
	if repo == "" {
		repo = item.ProjectName
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
