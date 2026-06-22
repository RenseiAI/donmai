package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is the per-session heartbeat cadence. Aligned with
// the worker-level heartbeat in daemon/heartbeat.go for visual
// consistency in operator dashboards.
const DefaultInterval = 30 * time.Second

// DefaultStrikesUntilLost is the consecutive-failure threshold that
// triggers a LostOwnership event. Mirrors MAX_HEARTBEAT_FAILURES from
// the legacy TS worker-runner.ts.
const DefaultStrikesUntilLost = 3

// DefaultMaxAttemptsPerTick is the HTTP retry budget for one tick.
// Mirrors apiRequestWithError in afclient/retry.go (3 attempts with
// 1s/2s/4s exponential backoff).
const DefaultMaxAttemptsPerTick = 3

// Sentinel errors. The runner uses errors.Is to detect a
// LostOwnership transition without scraping log lines.
var (
	// ErrLostOwnership is returned through Pulser.LostOwnership when
	// the platform has stopped accepting heartbeats for this session.
	// The runner halts work + relinquishes the worktree.
	ErrLostOwnership = errors.New("runtime/heartbeat: session ownership lost")
)

// Config carries the inputs Pulser needs. SessionID and BaseURL are
// required; the rest have sensible defaults.
type Config struct {
	// SessionID is the platform session UUID (path param of
	// /api/sessions/<id>/lock-refresh). Required.
	SessionID string
	// WorkerID is the daemon worker that owns the session. Sent in
	// the request body so the platform can detect a hand-off.
	WorkerID string
	// IssueID is the platform-side Linear issue UUID. The platform's
	// /api/sessions/<id>/lock-refresh handler keys the per-issue lock
	// on issue:lock:{IssueID} and rejects the request with 400 when
	// this is empty — so callers must populate it. Sourced
	// from prompt.QueuedWork.IssueID (camelCase "issueId" on the wire).
	IssueID string
	// BaseURL is the platform API base, e.g. "https://platform.example.com".
	// Required.
	BaseURL string
	// AuthToken is sent as Bearer in the Authorization header.
	// Optional — when empty no auth header is set (test paths use
	// httptest.Server without auth).
	AuthToken string
	// CredentialProvider returns the latest worker id + runtime token.
	// When set, every heartbeat tick calls it before posting so child
	// runners can pick up daemon-side runtime-token refreshes mid-session.
	CredentialProvider CredentialProvider

	// OnInject is invoked when the platform piggybacks an agent-memory
	// inject onto a successful lock-refresh response (refreshed=true AND
	// inject != nil). The pulser delivers the payload on the heartbeat
	// goroutine; implementations must be cheap + non-blocking (the runner
	// wires this to a non-blocking send onto its inject channel).
	//
	// The return value reports whether the consumer ACCEPTED the payload.
	// Only accepted injects are acked back to the platform (via the next
	// request's AckedInject echo, or the Stop-time flush); a rejected
	// inject (e.g. the runner's buffer was full) stays unacked so the
	// platform re-delivers it on a later refresh — ack-or-requeue, never
	// ack-and-drop. Nil-safe: when OnInject is nil the inject is decoded
	// and acked but otherwise dropped (no consumer will ever appear, so
	// leaving it unacked would re-deliver forever). Wave 3 runtime
	// memory-inject transport — the pulser already runs inside the worker
	// process that owns the live agent.Handle, so it is the only
	// authenticated channel that can reach Handle.Inject (the daemon poll
	// path is in the wrong process).
	OnInject func(InjectPayload) bool

	// Interval overrides DefaultInterval. Zero falls back to default.
	Interval time.Duration
	// StrikesUntilLost overrides DefaultStrikesUntilLost. Zero falls
	// back to default.
	StrikesUntilLost int
	// MaxAttemptsPerTick overrides DefaultMaxAttemptsPerTick. Zero
	// falls back to default.
	MaxAttemptsPerTick int

	// HTTPClient overrides http.DefaultClient (tests inject
	// httptest.Server.Client()).
	HTTPClient *http.Client
	// Logger overrides slog.Default(). The pulser logs at debug for
	// successful ticks and warn for strikes.
	Logger *slog.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
	// Sleep overrides time.Sleep for inner-retry backoff in tests.
	Sleep func(time.Duration)
}

// RuntimeCredentials are the bearer-token credentials needed for a heartbeat
// request. Empty fields fall back to Config.WorkerID / Config.AuthToken.
type RuntimeCredentials struct {
	WorkerID  string
	AuthToken string
}

// CredentialProvider returns the freshest worker runtime credentials available
// to the caller. Implementations should be cheap and concurrency-safe.
type CredentialProvider func(context.Context) (RuntimeCredentials, error)

// InjectPayload is one agent-memory inject the platform piggybacks onto a
// lock-refresh response. DeliveryID is the platform-side queue row id the
// worker echoes back via [refreshRequest.AckedInject] on the next tick so
// the platform stops re-sending it. Text is the rendered memory block the
// runner injects into the live session as a follow-up user message.
//
// Kind discriminates the inject type:
//   - "" or "memory" — standard agent-memory block (existing behaviour; back-compat)
//   - "user"         — a user-turn message from an interactive interview session
//
// TurnID is present only for kind="user" injects and carries the turn
// correlation id the platform stamped when it enqueued the user turn
// (enqueueUserTurnInject). Absent for kind="memory" injects.
//
// Wire shape (platform → worker, nested under the lock-refresh response's
// optional "inject" object):
//
//	{"deliveryId": "...", "text": "...", "kind": "memory"|"user", "turnId": "..."}
//
// CONTRACT: defined in CONTRACT-FREEZE §3 and
// platform/src/lib/interview/wire-types.ts (INJECT_KIND_USER / INJECT_KIND_MEMORY).
type InjectPayload struct {
	DeliveryID string `json:"deliveryId"`
	Text       string `json:"text"`
	// Kind is "memory" (default, back-compat) or "user" (interview turn).
	// Empty string is treated as "memory" by all consumers.
	Kind string `json:"kind,omitempty"`
	// TurnID is the turn correlation id for kind="user" injects.
	// Empty for kind="memory".
	TurnID string `json:"turnId,omitempty"`
}

func (c Config) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultInterval
}

func (c Config) strikesUntilLost() int {
	if c.StrikesUntilLost > 0 {
		return c.StrikesUntilLost
	}
	return DefaultStrikesUntilLost
}

func (c Config) maxAttempts() int {
	if c.MaxAttemptsPerTick > 0 {
		return c.MaxAttemptsPerTick
	}
	return DefaultMaxAttemptsPerTick
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) credentials(ctx context.Context) RuntimeCredentials {
	creds := RuntimeCredentials{
		WorkerID:  c.WorkerID,
		AuthToken: c.AuthToken,
	}
	if c.CredentialProvider == nil {
		return creds
	}
	fresh, err := c.CredentialProvider(ctx)
	if err != nil {
		c.logger().Warn("heartbeat credential refresh failed; using cached credentials", "err", err)
		return creds
	}
	if fresh.WorkerID != "" {
		creds.WorkerID = fresh.WorkerID
	}
	if fresh.AuthToken != "" {
		creds.AuthToken = fresh.AuthToken
	}
	return creds
}

// Pulser drives the heartbeat loop for one session. Construct via New
// then call Start; Stop releases resources.
type Pulser struct {
	cfg Config

	mu       sync.Mutex
	stopped  bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	lostCh   chan struct{}
	strikes  atomic.Int64
	lastTick atomic.Int64 // unix-millis

	// stopRequested records that the platform sent the deterministic
	// operator-cancel signal ({"stop": true} on a lock-refresh response).
	// Set immediately before the LostOwnership channel is closed via the
	// fast in-band path so the runner can distinguish an operator cancel
	// (route to FailureOperatorCancelled — do NOT blind-re-dispatch) from
	// the 3-strike fuse (FailureLostOwnership). Read via StopRequested
	// after LostOwnership fires.
	stopRequested atomic.Bool

	// lastAckedInject is the DeliveryID of the most recent inject the
	// pulser delivered to OnInject AND the consumer accepted. It is echoed
	// back to the platform on every subsequent request via
	// [refreshRequest.AckedInject] so the platform can mark the inject
	// acked and stop re-sending it. Read/written on the single heartbeat
	// goroutine (tick → doRefresh); Stop reads it only after the loop has
	// exited (synchronised via doneCh), so it needs no lock.
	lastAckedInject string

	// lastEchoedAck is the AckedInject value most recently carried on a
	// request the platform answered 2xx — i.e. the ack we KNOW landed.
	// When Stop runs with lastAckedInject != lastEchoedAck the final ack
	// never rode a tick (short sessions exit before the next heartbeat
	// interval elapses) and flushPendingAck fires one best-effort
	// ack-only request so the platform does not strand the inject
	// delivered-but-unacked. Same synchronisation regime as
	// lastAckedInject.
	lastEchoedAck string
}

// New returns a Pulser configured for the given session. Returns an
// error when SessionID or BaseURL is missing.
func New(cfg Config) (*Pulser, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/heartbeat: SessionID required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("runtime/heartbeat: BaseURL required")
	}
	return &Pulser{cfg: cfg}, nil
}

// LostOwnership returns a channel that closes when the platform has
// stopped accepting heartbeats (3 consecutive ticks failed). The
// runner selects on this to abort the session early.
func (p *Pulser) LostOwnership() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh == nil {
		p.lostCh = make(chan struct{})
	}
	return p.lostCh
}

// Strikes returns the current consecutive-failure count. Useful for
// observability; the runner usually only watches LostOwnership.
func (p *Pulser) Strikes() int {
	return int(p.strikes.Load())
}

// StopRequested reports whether LostOwnership was closed by the platform's
// deterministic operator-cancel signal ({"stop": true} on a lock-refresh
// response) rather than by the 3-strike heartbeat fuse. The runner reads it
// after LostOwnership fires to fork to FailureOperatorCancelled (which is
// NOT blind-re-dispatched) instead of FailureLostOwnership.
func (p *Pulser) StopRequested() bool {
	return p.stopRequested.Load()
}

// LastTick returns the unix-ms timestamp of the most recent successful
// tick. Zero when no tick has succeeded yet.
func (p *Pulser) LastTick() int64 {
	return p.lastTick.Load()
}

// Start begins the heartbeat loop. The first tick fires synchronously
// before Start returns so the platform mirror updates without lag.
//
// The loop runs until ctx is cancelled, Stop is called, or the
// 3-strike threshold trips. Calling Start more than once on the same
// Pulser returns an error; build a new Pulser per session.
func (p *Pulser) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.stopCh != nil {
		p.mu.Unlock()
		return errors.New("runtime/heartbeat: Start called twice")
	}
	if p.lostCh == nil {
		p.lostCh = make(chan struct{})
	}
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	// Fire the first tick synchronously so the platform sees an
	// immediate refresh rather than waiting one Interval.
	p.tick(ctx)
	if p.tripped() {
		// First tick already tripped — surface it but still kick the
		// loop so Stop semantics remain consistent.
		p.cfg.logger().Warn("heartbeat: first tick already failed",
			"sessionId", p.cfg.SessionID)
	}

	go p.run(ctx)
	return nil
}

// Stop signals the loop to exit and blocks until it has. Idempotent
// and safe to call from a deferred cleanup path. Returns nil; the
// signature matches context-aware shutdown helpers elsewhere in the
// codebase for symmetry.
//
// After the loop has drained, Stop flushes any pending inject ack: a
// short session often receives + applies an inject on one tick and exits
// before the next tick would have echoed the ack, stranding the inject
// delivered-but-unacked on the platform (it can never requeue it to a
// session that no longer exists). The flush is one best-effort ack-only
// request with its own timeout.
func (p *Pulser) Stop() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	if p.stopCh != nil {
		close(p.stopCh)
	}
	doneCh := p.doneCh
	p.mu.Unlock()

	if doneCh != nil {
		<-doneCh
	}
	p.flushPendingAck()
	return nil
}

// finalAckFlushTimeout bounds the Stop-time ack flush so a dead platform
// cannot stall worker shutdown.
const finalAckFlushTimeout = 5 * time.Second

// flushPendingAck fires one best-effort ack-only lock-refresh when the
// most recently accepted inject's ack has not yet ridden a successful
// request. Called from Stop strictly after the heartbeat loop has exited
// (happens-before via doneCh), so reading the ack fields is race-free.
// The response is deliberately ignored: the session is over, so any NEW
// inject the platform might piggyback must stay unacked for the platform
// to requeue elsewhere.
func (p *Pulser) flushPendingAck() {
	if p.lastAckedInject == "" || p.lastAckedInject == p.lastEchoedAck {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), finalAckFlushTimeout)
	defer cancel()
	if _, err := p.postRefresh(ctx, p.lastAckedInject); err != nil {
		p.cfg.logger().Warn("final inject ack flush failed",
			"sessionId", p.cfg.SessionID,
			"deliveryId", p.lastAckedInject,
			"err", err)
		return
	}
	p.lastEchoedAck = p.lastAckedInject
	p.cfg.logger().Debug("final inject ack flushed",
		"sessionId", p.cfg.SessionID,
		"deliveryId", p.lastAckedInject)
}

// run is the inner loop driving subsequent ticks.
func (p *Pulser) run(ctx context.Context) {
	defer func() {
		p.mu.Lock()
		ch := p.doneCh
		p.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()

	ticker := time.NewTicker(p.cfg.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChannel():
			return
		case <-ticker.C:
			p.tick(ctx)
			// Exit on either the 3-strike fuse (tripped) or the fast
			// in-band immediate-lose path (ownershipLost set by
			// loseOwnershipNow inside doRefresh). Without the
			// ownershipLost check the loop would keep posting heartbeats
			// to a session the platform has already told us to stop until
			// strikes happen to reach the threshold.
			if p.tripped() || p.ownershipLost() {
				return
			}
		}
	}
}

func (p *Pulser) stopChannel() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCh
}

// ownershipLost reports whether the LostOwnership channel has been closed,
// by either the 3-strike fuse (tripped) or the fast in-band immediate-lose
// path (loseOwnershipNow). Used by the run loop to exit promptly after an
// operator stop / hand-off without waiting for the strike counter.
func (p *Pulser) ownershipLost() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh == nil {
		return false
	}
	select {
	case <-p.lostCh:
		return true
	default:
		return false
	}
}

// tripped reports whether the strike counter has reached the lost
// threshold. The first transition closes the LostOwnership channel.
func (p *Pulser) tripped() bool {
	if int(p.strikes.Load()) < p.cfg.strikesUntilLost() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh != nil {
		// Close idempotently — already-closed channels panic on
		// re-close; guard with stopped flag set on close path.
		select {
		case <-p.lostCh:
			// already closed
		default:
			close(p.lostCh)
		}
	}
	return true
}

// loseOwnershipNow closes the LostOwnership channel immediately, bypassing
// the 3-strike fuse. It is the fast in-band leg of the deterministic cancel
// wire (Guard 3): called from doRefresh the instant the platform answers a
// lock-refresh with {"stop": true}, so the session dies within one heartbeat
// interval instead of waiting out three failed ticks. Idempotent — the same
// guarded-close pattern as tripped() — so a later strike-trip is a no-op.
// The lostCh is allocated lazily in Start/LostOwnership; on the off chance
// neither has run yet, allocate it here so the close target exists.
func (p *Pulser) loseOwnershipNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostCh == nil {
		p.lostCh = make(chan struct{})
	}
	select {
	case <-p.lostCh:
		// already closed
	default:
		close(p.lostCh)
	}
}

// tick performs one heartbeat attempt — including up to maxAttempts
// inner HTTP retries — and updates the strike counter. Returns no
// error: state is observed via Strikes / LostOwnership.
func (p *Pulser) tick(ctx context.Context) {
	attempts := p.cfg.maxAttempts()
	var lastErr error
	for n := 1; n <= attempts; n++ {
		err := p.doRefresh(ctx)
		if err == nil {
			p.strikes.Store(0)
			p.lastTick.Store(p.cfg.now().UnixMilli())
			p.cfg.logger().Debug("heartbeat tick ok",
				"sessionId", p.cfg.SessionID, "attempt", n)
			return
		}
		lastErr = err
		if n < attempts {
			backoff := time.Duration(1<<(n-1)) * time.Second
			if p.cfg.Sleep != nil {
				p.cfg.Sleep(backoff)
			} else {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
		}
	}
	strike := p.strikes.Add(1)
	p.cfg.logger().Warn("heartbeat tick failed",
		"sessionId", p.cfg.SessionID,
		"strike", strike,
		"strikesUntilLost", p.cfg.strikesUntilLost(),
		"err", lastErr)
}

// refreshRequest is the body shape sent to /api/sessions/<id>/lock-refresh.
// Matches the legacy TS body plus the Wave 3 AckedInject echo.
type refreshRequest struct {
	WorkerID string `json:"workerId,omitempty"`
	IssueID  string `json:"issueId,omitempty"`
	// AckedInject echoes the DeliveryID of the inject the worker last
	// applied so the platform can mark it delivered and stop re-sending it.
	// Empty until the first inject is received. Wave 3 runtime memory-inject.
	AckedInject string `json:"ackedInject,omitempty"`
}

// refreshResponse is the body shape returned by lock-refresh. The
// {"refreshed": false} case is treated as a strike-eligible failure
// because it means the platform did not extend the lock — the session
// has likely already been handed off.
type refreshResponse struct {
	Refreshed bool `json:"refreshed"`
	// Stop is the deterministic operator-cancel signal: when the platform
	// has flipped the session to a terminal/stopping status it sets
	// {"stop": true} on the lock-refresh response. The pulser closes
	// LostOwnership IMMEDIATELY on this (one heartbeat interval, ~30s)
	// rather than waiting out the 3-strike fuse (~90s+) — the in-band fast
	// leg of the deterministic cancel wire (Guard 3). The wire field name
	// is EXACTLY "stop"; the platform half writes the same key.
	Stop bool `json:"stop"`
	// Inject is an optional agent-memory inject the platform piggybacks
	// onto a successful refresh. Only honoured when Refreshed is true (an
	// inject on a refused refresh is ignored — ownership was lost). Wave 3
	// runtime memory-inject.
	Inject *InjectPayload `json:"inject,omitempty"`
}

// postRefresh issues one POST to /api/sessions/<id>/lock-refresh carrying
// the given AckedInject echo and returns the decoded response. A nil
// response with a nil error means the platform answered 2xx with an empty
// body (some operator-mode deployments respond 204) — the request landed
// but there is nothing to decode.
func (p *Pulser) postRefresh(ctx context.Context, ack string) (*refreshResponse, error) {
	creds := p.cfg.credentials(ctx)
	body, err := json.Marshal(refreshRequest{
		WorkerID:    creds.WorkerID,
		IssueID:     p.cfg.IssueID,
		AckedInject: ack,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	url := p.cfg.BaseURL + "/api/sessions/" + p.cfg.SessionID + "/lock-refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if creds.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+creds.AuthToken)
	}
	resp, err := p.cfg.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lock-refresh: status %d", resp.StatusCode)
	}
	var out refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// doRefresh issues one POST to /api/sessions/<id>/lock-refresh and
// returns nil only when the platform reports the lock was extended.
func (p *Pulser) doRefresh(ctx context.Context) error {
	ack := p.lastAckedInject
	out, err := p.postRefresh(ctx, ack)
	if err != nil {
		return err
	}
	// The request landed (2xx) — the ack echo it carried is now known to
	// the platform, regardless of the refresh verdict below.
	p.lastEchoedAck = ack
	if out == nil {
		// Empty-body 204-style response: accepted, nothing to decode.
		return nil
	}
	if out.Stop {
		// Deterministic operator-cancel (Guard 3 fast in-band leg): the
		// platform flipped the session to a terminal/stopping status.
		// Close LostOwnership IMMEDIATELY (one heartbeat interval, ~30s)
		// instead of waiting out the 3-strike fuse, and record that this
		// was an operator cancel so the runner forks to
		// FailureOperatorCancelled rather than FailureLostOwnership. Do
		// NOT apply any piggybacked inject — the session is being torn
		// down.
		p.stopRequested.Store(true)
		p.loseOwnershipNow()
		return errors.New("lock-refresh: platform requested stop (stop=true)")
	}
	if !out.Refreshed {
		// Ownership refused — do NOT apply any piggybacked inject. The
		// platform only routes injects to the current lock holder; a
		// refused refresh means we are no longer it. Close LostOwnership
		// IMMEDIATELY (the session has already been handed off; there is
		// nothing to gain from three more failed ticks) — but leave
		// stopRequested unset so the runner classifies this as
		// FailureLostOwnership, the correct mode for a hand-off, not an
		// operator cancel.
		p.loseOwnershipNow()
		return errors.New("lock-refresh: platform refused (refreshed=false)")
	}
	// Wave 3 runtime memory-inject: the platform piggybacks at most one
	// pending inject per successful refresh. Deliver it to OnInject (the
	// runner's non-blocking channel send) and record its DeliveryID so the
	// next request acks it and the platform stops re-sending. A rejected
	// delivery (consumer buffer full) stays unacked so the platform
	// re-delivers on a later refresh — ack-or-requeue, never ack-and-drop.
	if out.Inject != nil {
		accepted := true
		if p.cfg.OnInject != nil {
			accepted = p.cfg.OnInject(*out.Inject)
		}
		if accepted {
			p.lastAckedInject = out.Inject.DeliveryID
		} else {
			p.cfg.logger().Warn("memory inject rejected by consumer; leaving unacked for re-delivery",
				"sessionId", p.cfg.SessionID,
				"deliveryId", out.Inject.DeliveryID)
		}
	}
	return nil
}
