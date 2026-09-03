package daemon

import (
	"bytes"
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

	"github.com/RenseiAI/donmai/sessionshim"
)

// HeartbeatOptions configure a HeartbeatService.
type HeartbeatOptions struct {
	WorkerID        string
	Hostname        string
	OrchestratorURL string
	// RuntimeJWT is the runtime token (a JWT) returned by /api/workers/register
	// and sent in Authorization: Bearer on every heartbeat.
	RuntimeJWT      string
	IntervalSeconds int
	GetActiveCount  func() int
	GetMaxCount     func() int
	GetStatus       func() RegistrationStatus
	Region          string

	// GetActiveSessionCounts returns one coherent occupancy snapshot. active is
	// the unclassed count across every run mode; activeInteractive is the union of
	// PTY "interactive" and legacy "interview" sessions. Both values are sampled
	// together so concurrent session lifecycle changes cannot split them across
	// different instants. A configured callback classifies interactive occupancy,
	// so the outbound body includes `activeInteractiveCount` even when its value
	// is zero.
	//
	// Optional and nil-safe: nil falls back to GetActiveCount and, when configured,
	// the legacy GetActiveInteractiveCount callback below. New embedders should use
	// this paired callback so the two occupancy values come from the same instant.
	GetActiveSessionCounts func() (active, activeInteractive int)

	// GetQuarantinedSessions returns the bounded per-session-shim quarantine
	// projection for this beat (ADR-2026-08-17 §D7).
	//
	// A quarantined shim is a live harness this daemon could not adopt. §D7
	// requires it to be VISIBLE — in host diagnostics and in every heartbeat —
	// and to count against capacity, precisely so it cannot become silent,
	// unreachable load that a consumer keeps dispatching against.
	//
	// Optional and nil-safe: nil omits the key entirely, which is the honest
	// shape for an embedder that does not run shim adoption at all. An EMPTY
	// slice is also omitted, so a beat only carries the key when there is
	// something quarantined to report.
	GetQuarantinedSessions func() []sessionshim.QuarantinedSession

	// GetSessionShim returns one coherent, non-secret adoption projection. When
	// configured, every network heartbeat carries it under sessionShim and a
	// successful call requires the server to echo the exact typed projection.
	// One callback is load-bearing: assembling host/revision/quarantine fields
	// through separate callbacks would permit a torn readiness claim.
	GetSessionShim func() (SessionShimHeartbeatProjection, error)
	// OnSessionShimAcknowledged runs only after the server has acknowledged and
	// exactly echoed the session-shim projection. A daemon recovering from a
	// dynamic proof-v2 readiness withdrawal uses this edge to reopen admission;
	// successful local projection alone is deliberately insufficient.
	OnSessionShimAcknowledged func(SessionShimHeartbeatProjection)

	// OnSessionShimRevisionStale runs when the server answers a beat with the
	// closed session-shim revision-stale conflict: this daemon presented a
	// superseded adoption revision, which means a batch commit advanced the
	// server while the daemon's copy of the answer was lost. The daemon wires
	// it to schedule commit-outcome reconciliation, so a stale beat is a
	// repair trigger rather than a skip-forever. It must not block: the beat
	// that observed the conflict is already being reported as failed, and the
	// reconciliation it arms runs on its own goroutine.
	OnSessionShimRevisionStale func()

	// GetActiveInteractiveCount is the legacy separately sampled interactive
	// occupancy callback. It remains source-compatible with embedders that adopted
	// the initial activeInteractiveCount contract before GetActiveSessionCounts was
	// introduced. When the paired callback above is nil, sendOne samples
	// GetActiveCount first and this callback second. A sample outside the subset
	// invariant (0 <= activeInteractive <= active) is omitted and logged; nil omits
	// the interactive key because occupancy classification is unavailable.
	//
	// Deprecated: use GetActiveSessionCounts to avoid a torn occupancy pair during
	// concurrent session lifecycle changes.
	GetActiveInteractiveCount func() int

	// GetLoad returns the current CPU and memory utilisation percentages
	// (0–100) for this beat. ok=false means "no sample this beat" and the
	// outbound body omits the `load` key entirely (the platform then leaves
	// worker_hosts.last_cpu_pct / last_mem_pct unchanged). Called once per
	// beat. Optional — leave nil to never sample, matching the
	// GetAllowlist/OnHostStatus optional-callback convention above. Wire it to
	// SampleLoad for the stdlib best-effort probe (item 8, per-beat load →
	// last_cpu_pct/last_mem_pct).
	GetLoad func() (cpuPct, memPct float64, ok bool)

	// GetLoadAverage returns Unix load averages (1/5/15 min). Same
	// best-effort contract as GetLoad: ok=false omits the key. Wire it
	// to SampleLoadAverage. Additive: absence preserves the Load-only
	// beat byte-identical.
	GetLoadAverage func() (one, five, fifteen float64, ok bool)

	// GetAllowlist returns the daemon's current project allowlist entries
	// (derived from cfg.Projects). Called every beat so a hot yaml reload
	// (when that lands) or in-process mutation reflects in the next
	// heartbeat. Returning nil is the canonical "no projects configured"
	// signal and triggers an empty AllowlistHash. Optional — callers that
	// don't care about allowlist sync can leave it nil.
	//
	// Phase 1d of 2026-05-18-daemon-config-sync-DESIGN.md.
	GetAllowlist func() []ProjectAllowlistEntry

	// GetProjectAdmission returns the daemon's complete admission state —
	// consent mode, enabled project ids, and repository entries. When set it
	// SUPERSEDES GetAllowlist, and the beat reports all three fields under one
	// hash.
	//
	// Prefer this over GetAllowlist. The entries-only report cannot express a
	// project admitted without a repository, nor the all-routed consent mode,
	// so an orchestrator fed by GetAllowlist alone can only learn about an
	// admission change when the daemon re-registers — i.e. on restart.
	// Optional, for the same reason GetAllowlist is.
	GetProjectAdmission func() ProjectAdmissionReport

	// OnPendingMutations is invoked when the platform attaches one or more
	// queued daemon-config mutations to a heartbeat response. The callback
	// is expected to apply each mutation against daemon.yaml and return
	// which ones succeeded (appliedIDs) and which failed
	// (failures). The HeartbeatService buffers these and includes them in
	// the NEXT beat's appliedMutations[] / mutationFailures[] fields so
	// the platform can ACK and emit audit events.
	//
	// Optional — leave nil to ignore platform-initiated mutations (the
	// daemon will keep working off its yaml as-edited locally). Phase 2c.
	OnPendingMutations func(ctx context.Context, mutations []PendingMutation) (appliedIDs []string, failures []HeartbeatMutationFailure)

	// OnHostStatus is invoked when the platform's heartbeat response
	// reports a non-ok hostStatus (e.g. pool_deleted). The daemon can
	// use this to surface re-register guidance in `donmai daemon stats` or
	// to enter a non-claiming state. Called with the latest status on
	// every beat that includes one, so callers can rely on it to clear
	// (status='ok') as well.
	//
	// Optional — leave nil to ignore. Phase 2e.
	OnHostStatus func(detail HostStatusDetail)

	// HTTPClient is the client used for the real-endpoint call.
	HTTPClient *http.Client
	// LogWarn is called when the real-endpoint call fails (transient
	// failures are non-fatal — the platform will detect via missed
	// heartbeats and a worker the orchestrator no longer recognises).
	LogWarn func(format string, args ...any)
	// LogInfo is called for routine, self-healing events (e.g. a token
	// rejection that immediately triggers a refresh). Defaults to no-op.
	LogInfo func(format string, args ...any)
	// Now provides the heartbeat sentAt timestamp.
	Now func() time.Time
	// OnHeartbeat is invoked after each heartbeat payload is composed
	// (whether or not the network call succeeded). Used by tests and
	// observability.
	OnHeartbeat func(payload HeartbeatPayload)
	// OnReregister is called when the orchestrator rejects a heartbeat:
	// HTTP 401 (runtime token expired/invalid) or HTTP 404 (worker not
	// recognised). Implementations return the worker id + runtime token to
	// continue with — USUALLY THE SAME worker id, because the recovery path
	// re-presents the existing registration rather than replacing it.
	// Returning a non-nil error leaves the heartbeat in its prior state and
	// logs via LogWarn; the next tick retries and re-triggers this path.
	//
	// reason is the structured failure reason ("worker-not-found",
	// "runtime-token-expired", "unauthorized", "auth-failure"). Callers
	// should pass it through to RefreshRuntimeToken, which owns the recovery
	// decision. In particular "worker-not-found" is NOT evidence that the
	// registration is gone — see RefreshRuntimeToken for what treating it
	// that way cost.
	//
	// Required when the daemon runs against a real platform; tests that
	// only exercise the local stub path can leave it nil.
	OnReregister func(ctx context.Context, reason string) (workerID, runtimeJWT string, err error)
}

// HeartbeatService manages the periodic heartbeat goroutine. It is safe to
// Start / Stop multiple times; consecutive Starts are idempotent.
type HeartbeatService struct {
	opts HeartbeatOptions

	// sendMu serializes whole beats. Every send path — the periodic tick,
	// StartSynchronized, and the out-of-band SendNow — funnels through
	// sendOneResult and takes this lock for the entire compose/POST/ACK-drop
	// sequence. It is what makes an out-of-band beat unable to interleave with
	// a tick: two concurrent senders would otherwise both snapshot the same
	// pending-ACK buffer and report the same mutation acknowledgements twice.
	// It is deliberately separate from mu, which guards field access only and
	// is never held across the network call.
	sendMu sync.Mutex

	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	last     HeartbeatPayload
	workerID string // mutable: refreshed by OnReregister
	jwt      string // mutable: refreshed by OnReregister

	// lastAllowlistHash tracks the most recently transmitted allowlist
	// hash so we only re-send the full entry list when it changes. Empty
	// string forces the next beat to include the list (covers the boot
	// case and the "previously empty, now populated" transition).
	lastAllowlistHash string

	// pendingApplied / pendingFailures buffer mutation ACKs that arrived
	// in the previous heartbeat response, were applied locally, and are
	// waiting to be reported on the next outbound beat. Cleared only on a
	// successful heartbeat call — a network failure leaves them buffered
	// so they re-ride the next attempt.
	pendingApplied  []string
	pendingFailures []HeartbeatMutationFailure

	// shimProjection / shimAcknowledged are the session-shim hooks, seeded from
	// opts and REPLACEABLE afterwards (SetSessionShimProjection). A daemon whose
	// durable-session composition lands after startup has to be able to start
	// carrying the projection on a heartbeat that is already running; keeping
	// them here rather than in opts is what makes that swap race-free, because
	// opts is read by the beat without a lock.
	shimProjection   func() (SessionShimHeartbeatProjection, error)
	shimAcknowledged func(SessionShimHeartbeatProjection)
}

// NewHeartbeatService constructs a HeartbeatService from opts. Required
// callbacks are GetMaxCount, GetStatus, and either GetActiveSessionCounts or
// GetActiveCount.
func NewHeartbeatService(opts HeartbeatOptions) *HeartbeatService {
	if opts.IntervalSeconds <= 0 {
		opts.IntervalSeconds = int(HeartbeatDefaultInterval / time.Second)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LogWarn == nil {
		opts.LogWarn = func(string, ...any) {}
	}
	if opts.LogInfo == nil {
		opts.LogInfo = func(string, ...any) {}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &HeartbeatService{
		opts:             opts,
		workerID:         opts.WorkerID,
		jwt:              opts.RuntimeJWT,
		shimProjection:   opts.GetSessionShim,
		shimAcknowledged: opts.OnSessionShimAcknowledged,
	}
}

// SetSessionShimProjection installs (or, with a nil getter, withdraws) the
// session-shim projection this heartbeat carries.
//
// Passing nil is the explicit stand-down: the beat goes back to omitting the
// key entirely, which is what a control plane reads as "this host is not
// projecting a shim" rather than "this host is projecting an empty one".
//
// The two hooks move together on purpose. An acknowledgement callback left
// behind a withdrawn projection would fire against a state nobody published,
// and a projection installed without one would never let the daemon learn that
// authority accepted it.
func (h *HeartbeatService) SetSessionShimProjection(
	get func() (SessionShimHeartbeatProjection, error),
	onAcknowledged func(SessionShimHeartbeatProjection),
) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shimProjection = get
	h.shimAcknowledged = onAcknowledged
}

// sessionShimHooks returns the projection hooks in effect for one beat. Both
// are read under one lock so a beat can never compose a projection with one
// generation's getter and acknowledge it with another's callback.
func (h *HeartbeatService) sessionShimHooks() (
	func() (SessionShimHeartbeatProjection, error),
	func(SessionShimHeartbeatProjection),
) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shimProjection, h.shimAcknowledged
}

// Start launches the heartbeat goroutine. It sends an immediate heartbeat,
// then continues at IntervalSeconds. Subsequent calls are no-ops.
func (h *HeartbeatService) Start() {
	h.start(true)
}

// StartSynchronized sends and strictly validates one heartbeat before starting
// the periodic loop. Hosted session-shim recovery uses this after adoption and
// carrier activation so poll/claim cannot race ahead of the first accepted
// host/controller/revision projection.
func (h *HeartbeatService) StartSynchronized(ctx context.Context) error {
	if err := h.sendOneResult(ctx); err != nil {
		return err
	}
	h.start(false)
	return nil
}

func (h *HeartbeatService) start(immediate bool) {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.running = true
	h.mu.Unlock()

	go h.loop(ctx, immediate)
}

// Stop terminates the heartbeat goroutine. Safe to call multiple times.
func (h *HeartbeatService) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.running = false
}

// IsRunning reports whether the heartbeat goroutine is active.
func (h *HeartbeatService) IsRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// LastPayload returns the most recently composed heartbeat payload (for
// debugging / status surfaces).
func (h *HeartbeatService) LastPayload() HeartbeatPayload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// CurrentCredentials returns the worker id and runtime JWT currently in
// use. They may differ from the values passed at construction time after a
// re-register on 401.
func (h *HeartbeatService) CurrentCredentials() (workerID, runtimeJWT string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workerID, h.jwt
}

func (h *HeartbeatService) loop(ctx context.Context, immediate bool) {
	if immediate {
		h.sendOne(ctx)
	}

	tick := time.NewTicker(time.Duration(h.opts.IntervalSeconds) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.sendOne(ctx)
		}
	}
}

func (h *HeartbeatService) sendOne(ctx context.Context) {
	if err := h.sendOneResult(ctx); err != nil {
		h.opts.LogWarn("daemon heartbeat skipped: %v", err)
	}
}

// SendNow sends exactly one heartbeat immediately, out of band with the
// periodic loop, and reports that single beat's result.
//
// It exists for the edges where waiting out a heartbeat interval is an outage
// rather than a rounding error. A session-shim adoption published after startup
// raises the recovery barrier: until the control plane has acknowledged the
// completed projection this host claims no new work and is not visible as
// adoption-complete. Ringing the beat the moment carrier activation finishes
// turns an interval-long stall into one round-trip.
//
// It never starts the periodic loop. When the loop is not running there is no
// beat lane to ride, so this is a no-op returning nil — a caller cannot use it
// to bring a stopped or never-started service to life.
//
// It cannot race the ticker either: sendOneResult takes sendMu for the whole
// compose/POST/ACK-drop sequence, so an out-of-band beat and a tick are
// ordered, never interleaved. The corollary is that a heartbeat callback must
// not call SendNow reentrantly.
//
// A returned error is that one beat's failure and is safe to log and continue
// on — the periodic loop is untouched and its next tick retries.
func (h *HeartbeatService) SendNow(ctx context.Context) error {
	if h == nil || !h.IsRunning() {
		return nil
	}
	return h.sendOneResult(ctx)
}

// sendOneResult composes, sends, and reconciles exactly one beat. The lock is
// the whole point of the split below: see sendMu.
func (h *HeartbeatService) sendOneResult(ctx context.Context) error {
	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	return h.sendOneSerialized(ctx)
}

func (h *HeartbeatService) sendOneSerialized(ctx context.Context) error {
	var (
		activeCount       int
		activeInteractive *int
	)
	switch {
	case h.opts.GetActiveSessionCounts != nil:
		active, interactive := h.opts.GetActiveSessionCounts()
		activeCount = active
		activeInteractive = &interactive
	default:
		activeCount = h.opts.GetActiveCount()
		if h.opts.GetActiveInteractiveCount != nil {
			interactive := h.opts.GetActiveInteractiveCount()
			if interactive < 0 || interactive > activeCount {
				h.opts.LogWarn(
					"heartbeat: omitting invalid legacy occupancy sample: activeCount=%d activeInteractiveCount=%d",
					activeCount,
					interactive,
				)
			} else {
				activeInteractive = &interactive
			}
		}
	}

	payload := HeartbeatPayload{
		WorkerID:                  h.workerIDLocked(),
		Hostname:                  h.opts.Hostname,
		Status:                    h.opts.GetStatus(),
		ActiveSessions:            activeCount,
		ActiveInteractiveSessions: activeInteractive,
		MaxSessions:               h.opts.GetMaxCount(),
		Region:                    h.opts.Region,
		SentAt:                    h.opts.Now().UTC().Format(time.RFC3339),
	}

	// Phase 1d: attach the admission hash every beat, the full report only on
	// change. GetProjectAdmission is the complete report (mode + enabled ids +
	// entries); GetAllowlist is the entries-only legacy shape kept for
	// embedders that have not moved over.
	if report, ok := h.projectAdmissionReport(); ok {
		hash := admissionHash(report)
		payload.AllowlistHash = hash
		h.mu.Lock()
		if hash != h.lastAllowlistHash {
			payload.Allowlist = report.Entries
			payload.EnabledProjectIDs = report.EnabledProjectIDs
			payload.ProjectAdmissionMode = normalizeProjectAdmissionMode(report.Mode)
			h.lastAllowlistHash = hash
		}
		h.mu.Unlock()
	}

	// §D7: quarantined shims ride every beat, not just the beat that discovered
	// them. A consumer that learned about a quarantine once and then stopped
	// hearing about it would reasonably conclude it had cleared.
	if h.opts.GetQuarantinedSessions != nil {
		if q := h.opts.GetQuarantinedSessions(); len(q) > 0 {
			payload.QuarantinedSessions = q
		}
	}
	getSessionShim, onSessionShimAcknowledged := h.sessionShimHooks()
	if getSessionShim != nil {
		projection, projectionErr := getSessionShim()
		if projectionErr != nil {
			return fmt.Errorf("heartbeat: session shim projection: %w", projectionErr)
		}
		projection = cloneSessionShimHeartbeatProjection(projection)
		if err := projection.validateReady(); err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		payload.SessionShim = &projection
	}

	// Item 8: sample per-beat CPU/mem load when a sampler is configured.
	// ok=false leaves payload.Load nil so the wire body omits the key
	// entirely (best-effort — a sampling miss must never fail a beat).
	if h.opts.GetLoad != nil {
		if cpu, mem, ok := h.opts.GetLoad(); ok {
			payload.Load = &heartbeatLoadFields{CPU: cpu, Memory: mem}
		}
	}
	if h.opts.GetLoadAverage != nil {
		if one, five, fifteen, ok := h.opts.GetLoadAverage(); ok {
			payload.LoadAverage = &heartbeatLoadAverageFields{One: one, Five: five, Fifteen: fifteen}
		}
	}

	// Phase 2c: pull any ACKs we owe the platform from the buffer. Cleared
	// only on a SUCCESSFUL POST below; a network failure leaves them
	// queued for the next attempt.
	h.mu.Lock()
	ackApplied := append([]string(nil), h.pendingApplied...)
	ackFailures := append([]HeartbeatMutationFailure(nil), h.pendingFailures...)
	h.mu.Unlock()

	h.mu.Lock()
	h.last = payload
	h.mu.Unlock()

	if h.opts.OnHeartbeat != nil {
		h.opts.OnHeartbeat(payload)
	}

	// Real-endpoint call is gated on (a) operator-requested stub mode and
	// (b) whether the runtime JWT is a stub (the stub path returns a token
	// with the "stub." prefix). v0.4.1 inverted the default to real
	// registration; the JWT-prefix check ensures a daemon configured with
	// a non-rs[pk]_live token still does not try to call prod with a
	// stub token.
	h.mu.Lock()
	jwt := h.jwt
	h.mu.Unlock()
	if stubModeRequested() || strings.HasPrefix(jwt, "stub.") {
		return nil
	}
	resp, err := h.callEndpoint(ctx, payload, ackApplied, ackFailures)
	if err == nil {
		if payload.SessionShim != nil && !resp.Acknowledged {
			return errors.New("heartbeat response did not acknowledge session shim projection")
		}
		if validationErr := validateHeartbeatSessionShimResponse(payload.SessionShim, resp.SessionShim); validationErr != nil {
			return validationErr
		}
		// Successful POST — drop the ACKs we just confirmed, then process
		// the platform's response (hostStatus + pendingMutations).
		h.dropConfirmedAcks(ackApplied, ackFailures)
		h.handleHeartbeatResponse(ctx, resp)
		if payload.SessionShim != nil && onSessionShimAcknowledged != nil {
			onSessionShimAcknowledged(cloneSessionShimHeartbeatProjection(*payload.SessionShim))
		}
		return nil
	}
	// On 401 (token expired/invalid) or 404 (worker not recognised),
	// re-register and retry once with fresh credentials. Any other error
	// is logged and left for the platform to detect via missed heartbeats.
	if isAuthFailure(err) && h.opts.OnReregister != nil {
		// Surface the structured [runtime-token] event so token-refresh
		// observers can grep one line per cycle rather than parsing
		// the LogWarn body. The OnReregister implementation is also
		// expected to log the resolution event ("refresh" or
		// "reregister") via RefreshRuntimeToken.
		reason := authFailureReason(err)
		slog.Info("[runtime-token]",
			"event", "auth-failure-detected",
			"path", "heartbeat",
			"reason", reason,
		)
		// Routine + self-healing (the refresh fires right below), so Info
		// not Warn — with the proactive refresher running this path is the
		// backstop, not the steady state.
		h.opts.LogInfo("daemon heartbeat rejected (%v) — refreshing runtime token (reason=%s)", err, reason)
		newWorkerID, newJWT, regErr := h.opts.OnReregister(ctx, reason)
		if regErr != nil {
			h.opts.LogWarn("daemon runtime-token refresh failed: %v", regErr)
			return fmt.Errorf("heartbeat runtime-token refresh: %w", regErr)
		}
		h.mu.Lock()
		h.workerID = newWorkerID
		h.jwt = newJWT
		h.mu.Unlock()
		// Re-send with fresh credentials. If this also fails we log and
		// move on — the next tick will try again.
		retryPayload := payload
		retryPayload.WorkerID = newWorkerID
		retryResp, retryErr := h.callEndpoint(ctx, retryPayload, ackApplied, ackFailures)
		if retryErr != nil {
			h.noteSessionShimRevisionStaleBeat(retryErr)
			h.opts.LogWarn("daemon heartbeat post-refresh also failed: %v", retryErr)
			return retryErr
		}
		if retryPayload.SessionShim != nil && !retryResp.Acknowledged {
			return errors.New("heartbeat post-refresh response did not acknowledge session shim projection")
		}
		if validationErr := validateHeartbeatSessionShimResponse(retryPayload.SessionShim, retryResp.SessionShim); validationErr != nil {
			return validationErr
		}
		h.dropConfirmedAcks(ackApplied, ackFailures)
		h.handleHeartbeatResponse(ctx, retryResp)
		return nil
	}
	h.noteSessionShimRevisionStaleBeat(err)
	h.opts.LogWarn("daemon heartbeat HTTP call failed: %v — orchestrator will detect via missed heartbeats", err)
	return err
}

// noteSessionShimRevisionStaleBeat turns the server's revision-stale heartbeat
// conflict into the configured reconciliation trigger.
func (h *HeartbeatService) noteSessionShimRevisionStaleBeat(err error) {
	if h.opts.OnSessionShimRevisionStale == nil || !isSessionShimRevisionStale(err) {
		return
	}
	h.opts.OnSessionShimRevisionStale()
}

// isSessionShimRevisionStale reports whether a heartbeat error is the control
// plane's closed 409 for a superseded session-shim adoption revision.
func isSessionShimRevisionStale(err error) bool {
	var hErr *heartbeatHTTPError
	return errors.As(err, &hErr) && hErr.status == http.StatusConflict &&
		strings.Contains(hErr.body, sessionShimRevisionStaleCode)
}

// workerIDLocked returns the current worker id under the lock.
func (h *HeartbeatService) workerIDLocked() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.workerID
}

// SetCredentials swaps the worker id + runtime JWT this service presents.
// Called by the daemon whenever ANOTHER path re-minted credentials (the
// proactive token refresher, or the poll loop's reactive refresh) so the
// heartbeat does not have to burn its own 401 round-trip — and its own log
// cycle — to discover them. Empty values are ignored.
func (h *HeartbeatService) SetCredentials(workerID, jwt string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if workerID != "" {
		h.workerID = workerID
	}
	if jwt != "" {
		h.jwt = jwt
	}
}

// projectAdmissionReport resolves the beat's admission report from whichever
// callback the embedder wired. GetProjectAdmission wins; GetAllowlist is
// lifted into the same shape so both paths share one hash and one wire format.
// ok=false means the embedder reports no admission state at all, and the beat
// omits every admission key.
func (h *HeartbeatService) projectAdmissionReport() (ProjectAdmissionReport, bool) {
	if h.opts.GetProjectAdmission != nil {
		report := h.opts.GetProjectAdmission()
		report.Mode = normalizeProjectAdmissionMode(report.Mode)
		return report, true
	}
	if h.opts.GetAllowlist != nil {
		return ProjectAdmissionReport{
			Mode:    ProjectAdmissionModeEnumerated,
			Entries: h.opts.GetAllowlist(),
		}, true
	}
	return ProjectAdmissionReport{}, false
}

// heartbeatRequestBody is the JSON body sent on POST
// /api/workers/<id>/heartbeat. Matches the platform contract:
//
//	{ status?, activeCount, activeInteractiveCount?, maxSessions?, load?,
//	  allowlistHash?, allowlist?, appliedMutations?, mutationFailures? }
//
// activeInteractiveCount is the interactive-occupancy split of activeCount:
// live Mode == "interactive" or legacy Mode == "interview" sessions count,
// while headless and unknown modes do not. allowlistHash + allowlist are Phase 1d
// fields; appliedMutations + mutationFailures are Phase 2c ACK fields.
type heartbeatRequestBody struct {
	// Status is the daemon's own lifecycle status (idle | busy | draining),
	// sourced from HeartbeatOptions.GetStatus via HeartbeatPayload.Status.
	// The server uses it to exclude a draining host from dispatch, so it MUST
	// reach the wire — this struct is the one that gets marshalled, and the
	// value used to be computed every beat and then dropped here.
	//
	// omitempty is load-bearing: a daemon with nothing meaningful to report
	// sends no key at all, leaving the server free to read "absent" as "no
	// signal" rather than having to disambiguate an empty string.
	Status      string `json:"status,omitempty"`
	ActiveCount int    `json:"activeCount"`
	// ActiveInteractiveCount is a *int so a nil (unreported) value drops the
	// key via omitempty — an embedder that does not classify interactive
	// occupancy must not send a misleading 0.
	ActiveInteractiveCount *int                        `json:"activeInteractiveCount,omitempty"`
	MaxSessions            int                         `json:"maxSessions,omitempty"`
	Load                   *heartbeatLoadFields        `json:"load,omitempty"`
	LoadAverage            *heartbeatLoadAverageFields `json:"loadAverage,omitempty"`
	AllowlistHash          string                      `json:"allowlistHash,omitempty"`
	Allowlist              []ProjectAllowlistEntry     `json:"allowlist,omitempty"`
	EnabledProjectIDs      []string                    `json:"enabledProjectIds,omitempty"`
	ProjectAdmissionMode   string                      `json:"projectAdmissionMode,omitempty"`
	AppliedMutations       []string                    `json:"appliedMutations,omitempty"`
	MutationFailures       []HeartbeatMutationFailure  `json:"mutationFailures,omitempty"`
	// QuarantinedSessions is the §D7 projection: every live per-session shim
	// this daemon refused to adopt, each carrying consumesCapacity:true.
	QuarantinedSessions []sessionshim.QuarantinedSession `json:"quarantinedSessions,omitempty"`
	SessionShim         *SessionShimHeartbeatProjection  `json:"sessionShim,omitempty"`
}

type heartbeatLoadFields struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
}

type heartbeatLoadAverageFields struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// HeartbeatMutationFailure is sent in the request body's mutationFailures[]
// to ACK a queued daemon-config mutation that failed locally.
type HeartbeatMutationFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// PendingMutation mirrors the platform's serializePendingMutation wire shape
// — included in the heartbeat response so the daemon can apply queued
// proposals and ACK on the next beat. Phase 2 of
// 2026-05-18-daemon-config-sync-DESIGN.md.
type PendingMutation struct {
	ID          string          `json:"id"`
	Op          string          `json:"op"` // session.kill | pool.deleted | project.enable | project.disable | legacy project.add | project.remove
	Params      json.RawMessage `json:"params"`
	RequestedAt string          `json:"requestedAt"`
	RequestedBy string          `json:"requestedBy"`
}

// HostStatusDetail mirrors the platform's wire shape for hostStatus in the
// heartbeat response. The daemon uses this to decide whether to keep
// claiming work or surface a re-register recommendation.
type HostStatusDetail struct {
	Status            string   `json:"status"` // ok | pool_deleted | pool_draining | pool_disabled | unauthorized
	RecommendedAction string   `json:"recommendedAction,omitempty"`
	CandidatePoolIDs  []string `json:"candidatePoolIds,omitempty"`
}

// HostStatusPoolPrefix is the wire prefix shared by every host status that
// describes the state of the capacity pool this host is bound to
// (pool_deleted, pool_draining, pool_disabled, and any pool_* state a newer
// control plane introduces). A pool in any of those states will not accept
// new sessions from this host, so the daemon stops claiming while one is in
// force.
const HostStatusPoolPrefix = "pool_"

// SuspendsClaiming reports whether this status means the daemon must stop
// claiming NEW work. It never says anything about work already in flight:
// suspension is a claim-side gate only, and running sessions are left alone.
//
// The three-way distinction matters more than the predicate:
//
//   - nil receiver ⇒ NO status has been observed yet — the first heartbeat is
//     still in flight, the daemon talks to a server that predates the
//     hostStatus field, or it runs with no control plane at all. Absent
//     status means claim normally. Failing closed here would silence every
//     deployment whose server never sends the field.
//   - "" or "ok" ⇒ the host is healthy; claim normally.
//   - "pool_*" ⇒ the bound pool is deleted / draining / disabled / otherwise
//     unavailable; stop claiming until the server says ok again.
//
// Any other value (today: "unauthorized", tomorrow: something we have not
// seen) is deliberately NOT a claim gate. Credential failures already have
// their own recovery rail — the 401/404 re-register path in the poll and
// heartbeat loops — and an unrecognised status from a newer server must not
// take a working daemon offline.
func (h *HostStatusDetail) SuspendsClaiming() bool {
	if h == nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(h.Status))
	return strings.HasPrefix(status, HostStatusPoolPrefix)
}

// heartbeatResponseBody is the JSON the platform sends back from the
// heartbeat endpoint. The pre-Phase-2 platform returned just
// {acknowledged, serverTime, pendingWorkCount}; Phase 2 adds hostStatus
// and pendingMutations. Both are optional — a daemon talking to a
// pre-Phase-2 platform unmarshals the missing fields to zero values.
type heartbeatResponseBody struct {
	Acknowledged     bool                            `json:"acknowledged"`
	ServerTime       string                          `json:"serverTime"`
	PendingWorkCount int                             `json:"pendingWorkCount"`
	HostStatus       *HostStatusDetail               `json:"hostStatus,omitempty"`
	PendingMutations []PendingMutation               `json:"pendingMutations,omitempty"`
	SessionShim      *SessionShimHeartbeatProjection `json:"sessionShim,omitempty"`
}

func (h *HeartbeatService) callEndpoint(
	ctx context.Context,
	payload HeartbeatPayload,
	ackApplied []string,
	ackFailures []HeartbeatMutationFailure,
) (*heartbeatResponseBody, error) {
	h.mu.Lock()
	workerID := h.workerID
	jwt := h.jwt
	h.mu.Unlock()
	if workerID == "" {
		return nil, fmt.Errorf("no worker id")
	}
	url := strings.TrimRight(h.opts.OrchestratorURL, "/") + "/api/workers/" + workerID + "/heartbeat"

	body := heartbeatRequestBody{
		Status:                 strings.TrimSpace(string(payload.Status)),
		ActiveCount:            payload.ActiveSessions,
		ActiveInteractiveCount: payload.ActiveInteractiveSessions,
		MaxSessions:            payload.MaxSessions,
		Load:                   payload.Load,
		LoadAverage:            payload.LoadAverage,
		AllowlistHash:          payload.AllowlistHash,
		Allowlist:              payload.Allowlist,
		EnabledProjectIDs:      payload.EnabledProjectIDs,
		ProjectAdmissionMode:   payload.ProjectAdmissionMode,
		AppliedMutations:       ackApplied,
		MutationFailures:       ackFailures,
		QuarantinedSessions:    payload.QuarantinedSessions,
		SessionShim:            payload.SessionShim,
	}
	// NB: region is deliberately NOT sent on the heartbeat leg. The platform's
	// heartbeat route parses no `region` key — region is a register-time-only
	// field written from RegisterRequest.Region (registration.go). Do not add a
	// region field to heartbeatRequestBody: it would be silently ignored and
	// duplicate the register-time write. (Wave-3 item 8 note.)
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := h.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		snippet := strings.TrimSpace(string(errBuf))
		return nil, &heartbeatHTTPError{status: res.StatusCode, body: snippet}
	}
	// Parse response body (Phase 2). Pre-Phase-2 platforms send only
	// {acknowledged, serverTime, pendingWorkCount} — the additional
	// fields unmarshal as zero values.
	var resp heartbeatResponseBody
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		// Unparseable response body — log and fall back to "ACK accepted,
		// no mutations to apply". The next beat will re-try the round trip.
		h.opts.LogWarn("daemon heartbeat response unparseable: %v", err)
		return &heartbeatResponseBody{Acknowledged: true}, nil
	}
	return &resp, nil
}

func validateHeartbeatSessionShimResponse(
	want *SessionShimHeartbeatProjection,
	got *SessionShimHeartbeatProjection,
) error {
	if want == nil {
		return nil
	}
	if got == nil {
		return errors.New("heartbeat response missing session shim acceptance")
	}
	if err := got.validateReady(); err != nil {
		return fmt.Errorf("heartbeat response: %w", err)
	}
	if !want.exactEqual(*got) {
		return errors.New("heartbeat response did not exactly echo the session shim projection")
	}
	return nil
}

// dropConfirmedAcks removes the ACK entries the platform just accepted
// from the pending buffer. Compares by mutation id so concurrent applies
// landing during the in-flight beat don't get lost.
func (h *HeartbeatService) dropConfirmedAcks(applied []string, failures []HeartbeatMutationFailure) {
	if len(applied) == 0 && len(failures) == 0 {
		return
	}
	confirmedApplied := make(map[string]bool, len(applied))
	for _, id := range applied {
		confirmedApplied[id] = true
	}
	confirmedFailed := make(map[string]bool, len(failures))
	for _, f := range failures {
		confirmedFailed[f.ID] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.pendingApplied[:0]
	for _, id := range h.pendingApplied {
		if !confirmedApplied[id] {
			next = append(next, id)
		}
	}
	h.pendingApplied = append([]string(nil), next...)
	nextF := h.pendingFailures[:0]
	for _, f := range h.pendingFailures {
		if !confirmedFailed[f.ID] {
			nextF = append(nextF, f)
		}
	}
	h.pendingFailures = append([]HeartbeatMutationFailure(nil), nextF...)
}

// handleHeartbeatResponse invokes the response-side callbacks: hostStatus
// surfacing and pending-mutation application. Applied/failed mutation ids
// returned by OnPendingMutations are buffered for ACK on the next beat.
func (h *HeartbeatService) handleHeartbeatResponse(ctx context.Context, resp *heartbeatResponseBody) {
	if resp == nil {
		return
	}
	if resp.HostStatus != nil && h.opts.OnHostStatus != nil {
		h.opts.OnHostStatus(*resp.HostStatus)
	}
	if len(resp.PendingMutations) > 0 && h.opts.OnPendingMutations != nil {
		applied, failures := h.opts.OnPendingMutations(ctx, resp.PendingMutations)
		if len(applied) > 0 || len(failures) > 0 {
			h.mu.Lock()
			h.pendingApplied = append(h.pendingApplied, applied...)
			h.pendingFailures = append(h.pendingFailures, failures...)
			h.mu.Unlock()
		}
	}
}

// heartbeatHTTPError carries the HTTP status so callers can branch on 401
// without parsing strings.
type heartbeatHTTPError struct {
	status int
	body   string
}

func (e *heartbeatHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// isAuthFailure returns true for the HTTP statuses that mean "this heartbeat
// was rejected on identity/credential grounds and the credentials must be
// refreshed": 401 (Unauthorized — JWT expired or invalid) and 404 (Worker not
// found). Refreshing does not imply re-registering — see RefreshRuntimeToken.
func isAuthFailure(err error) bool {
	var hErr *heartbeatHTTPError
	if errors.As(err, &hErr) {
		return hErr.status == http.StatusUnauthorized || hErr.status == http.StatusNotFound
	}
	return false
}

// authFailureReason classifies the auth-failure error into a stable
// short string for the [runtime-token] log line. Distinguishes
// runtime-token-expired (the canonical trigger) from
// worker-not-found and generic-unauthorized so operators can tell
// which path the daemon entered without scraping the response body.
func authFailureReason(err error) string {
	var hErr *heartbeatHTTPError
	if errors.As(err, &hErr) {
		switch hErr.status {
		case http.StatusUnauthorized:
			if strings.Contains(hErr.body, "Runtime token expired") {
				return "runtime-token-expired"
			}
			return "unauthorized"
		case http.StatusNotFound:
			return reasonWorkerNotFound
		}
	}
	return "auth-failure"
}
