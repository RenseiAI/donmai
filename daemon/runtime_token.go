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

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// RuntimeTokenRefreshEndpoint is the probed orchestrator endpoint the daemon
// hits to refresh an expired runtime JWT WITHOUT re-registering. The
// orchestrator owes a handler at this path that:
//   - accepts the registration token in the Authorization: Bearer header
//   - takes the existing workerId in the URL path
//   - mints a fresh runtime JWT bound to the SAME workerId
//   - returns { runtimeToken, runtimeTokenExpiresAt, heartbeatInterval, pollInterval }
//
// A 200 from this endpoint is ALSO the daemon's liveness probe for its own
// durable registration: the handler answers from the orchestrator's durable
// worker record, so a 200 proves the registration still exists and a 404
// proves it does not. That distinction is what keeps a transient rejection
// from burning a perfectly good worker identity — see RefreshRuntimeToken.
//
// An orchestrator that has not deployed the handler answers 404/405 and the
// daemon falls back to a full re-register (which mints a new workerId).
// #nosec G101 -- URL endpoint path, not a credential
const RuntimeTokenRefreshEndpoint = "/api/workers/refresh-token"

const (
	// DefaultMinReregisterInterval is the floor between two FULL
	// re-registrations from one refresher. A full re-register mints a new
	// worker identity and, on orchestrators that keep one live registration
	// per (machine, tenant), retires the previous one — so an unbounded
	// re-register path can hot-loop: register, get rejected, register again.
	//
	// This is a SAFEGUARD, not the fix. The fix is that a rejection now
	// re-presents the existing registration instead of replacing it
	// (RefreshRuntimeToken step 1). The floor only bounds the blast radius
	// if some future rejection mode escapes that logic. It is well under the
	// runtime-JWT TTL, so a genuinely-needed re-registration is never
	// starved.
	DefaultMinReregisterInterval = 60 * time.Second

	// reasonWorkerNotFound is the classified reason for an HTTP 404 on poll
	// or heartbeat: the orchestrator does not recognise the worker id we
	// presented.
	reasonWorkerNotFound = "worker-not-found"
)

// runtimeTokenRefresher holds the per-registration state that keeps two lanes
// (heartbeat + poll, and any additional tenant lanes sharing one credential
// cache) from racing each other into competing registrations:
//
//   - mu serializes refreshes for one registration, so simultaneous rejections
//     collapse into a single refresh.
//   - last/lastAt let a lane that was queued behind another adopt that result
//     instead of issuing a second, redundant refresh.
//   - lastReregisterAt enforces the full-re-register floor.
//
// One refresher exists per credential cache path (falling back to the
// orchestrator URL when no cache path is configured), because that is exactly
// the granularity at which lanes share a worker identity: the daemon's own
// lanes share a cache file, and separate tenant identities have separate ones.
type runtimeTokenRefresher struct {
	mu sync.Mutex

	last   *RefreshTokenResult
	lastAt time.Time

	lastReregisterAt time.Time

	// nowFn is overridable in tests; nil means time.Now.
	nowFn func() time.Time
}

// refresherResultTTL bounds how long a completed refresh stays adoptable by a
// lane that arrives afterwards. Long enough to absorb a slow sibling tick,
// far short of the runtime-JWT lifetime.
const refresherResultTTL = 30 * time.Second

var (
	refreshersMu sync.Mutex
	refreshers   = map[string]*runtimeTokenRefresher{}
)

// refresherFor returns the refresher that owns this registration's identity.
func refresherFor(opts RegistrationOptions) *runtimeTokenRefresher {
	key := opts.JWTPath
	if key == "" {
		key = "url:" + opts.OrchestratorURL
	}
	refreshersMu.Lock()
	defer refreshersMu.Unlock()
	r, ok := refreshers[key]
	if !ok {
		r = &runtimeTokenRefresher{}
		refreshers[key] = r
	}
	return r
}

// resetRefreshersForTest drops all per-registration refresher state so tests
// that share a JWT path cannot leak single-flight or cooldown state into one
// another.
func resetRefreshersForTest() {
	refreshersMu.Lock()
	defer refreshersMu.Unlock()
	refreshers = map[string]*runtimeTokenRefresher{}
}

func (r *runtimeTokenRefresher) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

// lastResultFor returns the most recent refresh result when it is still fresh
// AND it superseded the identity the caller is complaining about. Caller holds
// r.mu.
func (r *runtimeTokenRefresher) lastResultFor(staleWorkerID string, now time.Time) *RefreshTokenResult {
	if r.last == nil || r.last.WorkerID == "" {
		return nil
	}
	if now.Sub(r.lastAt) > refresherResultTTL {
		return nil
	}
	// A result for the SAME id the caller just had rejected is not an
	// adoption candidate — that id is exactly what failed.
	if r.last.WorkerID == staleWorkerID {
		return nil
	}
	return r.last
}

// remember records a completed refresh for sibling lanes to adopt. Caller
// holds r.mu.
func (r *runtimeTokenRefresher) remember(result *RefreshTokenResult) *RefreshTokenResult {
	r.last = result
	r.lastAt = r.now()
	return result
}

func (r *runtimeTokenRefresher) rememberValidated(regOpts RegistrationOptions, result *RefreshTokenResult) (*RefreshTokenResult, error) {
	if result == nil {
		return nil, errors.New("runtime token refresh produced no result")
	}
	if regOpts.ValidateCredentials != nil {
		if err := regOpts.ValidateCredentials(result.WorkerID, result.RuntimeToken); err != nil {
			return nil, fmt.Errorf("validate refreshed credentials: %w", err)
		}
	}
	if err := validateSessionShimCredentialReceipt(regOpts.SessionShim, result.SessionShim, result.WorkerID); err != nil {
		return nil, fmt.Errorf("validate refreshed session shim receipt: %w", err)
	}
	return r.remember(result), nil
}

// markReregistered stamps the full-re-register cooldown. Caller holds r.mu.
func (r *runtimeTokenRefresher) markReregistered() {
	r.lastReregisterAt = r.now()
}

// reregisterCooldown returns how long the caller must wait before another full
// re-registration is permitted, or 0 when it may proceed. Caller holds r.mu.
func (r *runtimeTokenRefresher) reregisterCooldown(opts RegistrationOptions) time.Duration {
	if r.lastReregisterAt.IsZero() {
		return 0
	}
	interval := minReregisterInterval(opts)
	if interval <= 0 {
		return 0
	}
	elapsed := r.now().Sub(r.lastReregisterAt)
	if elapsed >= interval {
		return 0
	}
	return interval - elapsed
}

func minReregisterInterval(opts RegistrationOptions) time.Duration {
	if opts.MinReregisterInterval != 0 {
		return opts.MinReregisterInterval
	}
	return DefaultMinReregisterInterval
}

// RefreshTokenResult is the outcome of an attempted runtime-token
// refresh. The OnReregister callback wired into HeartbeatService and
// PollService synthesises one of these per attempt; logged via the
// `[runtime-token]` structured line.
type RefreshTokenResult struct {
	// Mode is the path the refresh actually took: "refresh" (platform
	// honoured the refresh probe and minted a new JWT bound to the
	// same workerId), "reregister" (probe returned 404 / endpoint
	// missing — the daemon fell back to full POST /api/workers/register
	// and got a NEW workerId), or "error" (both paths failed).
	Mode string

	// WorkerID is the worker id in effect after the refresh attempt.
	// On Mode=refresh this is the SAME workerId; on Mode=reregister
	// it's a fresh one.
	WorkerID string

	// RuntimeToken is the fresh runtime JWT.
	RuntimeToken string

	// RuntimeTokenExpiresAt, HeartbeatInterval and PollInterval mirror the
	// platform's response (refresh endpoint or full re-register). Callers need
	// them to persist a COMPLETE CachedJWT after a refresh — the on-disk cache
	// entry carries the expiry + cadence, not just the token, so a daemon that
	// refreshes its token in-memory but writes back only the token would leave
	// a half-populated cache. Empty/zero when the platform omitted them.
	RuntimeTokenExpiresAt string
	HeartbeatInterval     int
	PollInterval          int

	// SessionShim is the exact non-secret server receipt returned by the
	// refresh or full re-registration path. It is retained in the credential
	// cache so hosted recovery never accepts a fresh-by-expiry legacy bearer.
	SessionShim *SessionShimCredentialReceipt

	// RegistrationTokenSwapped is true when Mode=reregister produced a
	// different workerId. Operators care about this signal because the
	// platform forgets the old workerId after a fresh registration —
	// any in-flight heartbeats / polls keyed on it 404 until the daemon
	// swaps credentials.
	RegistrationTokenSwapped bool

	// Reason is the structured reason the refresh path was taken
	// (e.g. "runtime-token-expired", "worker-not-found"). Surfaces in
	// the [runtime-token] log line.
	Reason string
}

// RefreshRuntimeToken refreshes the daemon's runtime JWT while PRESERVING the
// worker identity wherever the durable registration is still valid. Behaviour,
// in order:
//
//  1. Probe POST /api/workers/<id>/refresh-token with the registration token
//     in the Authorization: Bearer header. This runs for EVERY reason,
//     including a 404 "worker-not-found" — see the note below. On 200 the
//     orchestrator has minted a fresh JWT bound to the same workerId: the
//     durable registration is alive, the identity is kept, and no new
//     registration row is created.
//  2. On 404/405 from the probe, the durable registration is genuinely gone
//     (or the endpoint is not deployed). Before minting a new identity,
//     consult the on-disk credential cache: another lane in this process may
//     ALREADY have re-registered, in which case adopting its (verified)
//     credentials is correct and re-registering again is what turns two lanes
//     into a mutual-eviction loop.
//  3. Only when neither the current nor the cached identity can be
//     re-presented does the daemon fall back to a FULL re-register via
//     Register(ForceReregister=true), which mints a new workerId. Bounded by
//     MinReregisterInterval.
//  4. On any other probe failure (5xx, network, 401-on-registration-token),
//     return an error. Caller logs + retries on next tick.
//
// # WHY 404 NO LONGER SHORT-CIRCUITS TO RE-REGISTER
//
// This function used to treat reason=="worker-not-found" as proof that the
// worker was gone and re-register immediately. That is not what a 404 proves.
// An orchestrator answers 404 for several distinct conditions — an evicted
// cache entry in front of a live durable record, a scoping lookup that missed,
// a record retired moments earlier by a SIBLING lane's registration — and only
// one of them actually means "you no longer exist". Re-registering on all of
// them makes the daemon manufacture the very condition it is reacting to: the
// new registration retires the previous record, the sibling lane still holding
// that record is 404ed in turn, it re-registers, and the two lanes evict each
// other forever at poll/heartbeat cadence, one new registration per tick, for
// as long as the process lives. Restarting does not clear it, because boot
// resumes from a cached identity that the last iteration already retired.
//
// The refresh probe is the discriminator: it answers from the durable record,
// so a 200 means "still registered, here is a fresh token" and a 404 means
// "genuinely gone". Asking first costs one HTTP round trip and is the
// difference between re-presenting an identity and burning it.
//
// This is the only path that should call Register() with ForceReregister=true
// outside boot. All in-flight 401/404 detection in HeartbeatService /
// PollService routes through here so the `[runtime-token]` log line is the
// single source of truth for operators.
func RefreshRuntimeToken(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) (*RefreshTokenResult, error) {
	return refresherFor(regOpts).refresh(ctx, regOpts, currentWorkerID, reason)
}

// RepresentRuntimeToken re-presents the CURRENT registration and returns fresh
// credentials bound to the SAME worker id. It is step 1 of RefreshRuntimeToken
// and nothing else: no cached-sibling adoption, and no fall back to a full
// re-registration.
//
// That refusal is the point. A full re-register mints a new worker identity and
// retires the one every lane is presenting, which is the mutual-eviction shape
// documented above — an acceptable last resort when the alternative is a daemon
// with no credentials at all, and never acceptable for a caller that has
// working credentials and only wants to change what it attests about itself. A
// caller here can always keep the identity it has and try again later; it must
// not be able to burn it as a side effect of an optional feature.
func RepresentRuntimeToken(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) (*RefreshTokenResult, error) {
	if strings.TrimSpace(currentWorkerID) == "" {
		return nil, errors.New("re-present runtime token: no worker identity to re-present")
	}
	if !looksLikeRegistrationToken(regOpts.RegistrationToken) {
		return nil, errors.New("re-present runtime token: no usable registration token")
	}
	r := refresherFor(regOpts)
	r.mu.Lock()
	defer r.mu.Unlock()
	fresh, err := callRefreshEndpoint(ctx, regOpts, currentWorkerID)
	if err != nil {
		return nil, fmt.Errorf("re-present runtime token: %w", err)
	}
	slog.Default().Info("[runtime-token]",
		"event", "represent",
		"workerId", currentWorkerID,
		"reason", reason,
	)
	return r.rememberValidated(regOpts, &RefreshTokenResult{
		Mode:                  "refresh",
		WorkerID:              currentWorkerID,
		RuntimeToken:          fresh.RuntimeToken,
		RuntimeTokenExpiresAt: fresh.RuntimeTokenExpiresAt,
		HeartbeatInterval:     fresh.HeartbeatInterval,
		PollInterval:          fresh.PollInterval,
		SessionShim:           cloneSessionShimCredentialReceipt(fresh.SessionShim),
		Reason:                reason,
	})
}

// refresh is the body of RefreshRuntimeToken, serialized per registration so
// that two lanes reacting to the same rejection produce ONE refresh rather
// than two competing registrations.
func (r *runtimeTokenRefresher) refresh(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) (*RefreshTokenResult, error) {
	logger := slog.Default()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Single-flight adoption: while this call was queued behind a sibling
	// lane's refresh, that refresh may have already produced credentials for
	// the very identity we are complaining about. Hand them straight back
	// instead of doing the work twice.
	if adopted := r.lastResultFor(currentWorkerID, r.now()); adopted != nil {
		logger.Info("[runtime-token]",
			"event", "refresh.coalesced",
			"workerId", adopted.WorkerID,
			"staleWorkerId", currentWorkerID,
			"reason", reason,
			"detail", "a concurrent lane already refreshed this registration; reusing its credentials instead of issuing a second refresh",
		)
		coalesced := *adopted
		coalesced.Reason = reason
		return &coalesced, nil
	}

	// One line per refresh attempt. The reason carries the trigger:
	// reactive ("runtime-token-expired", "worker-not-found", ...) or the
	// scheduled "proactive-expiry" path.
	logger.Info("[runtime-token]",
		"event", "refresh-requested",
		"workerId", currentWorkerID,
		"reason", reason,
	)

	probeUsable := looksLikeRegistrationToken(regOpts.RegistrationToken)

	// 1. Re-present the CURRENT registration.
	if currentWorkerID != "" && probeUsable {
		fresh, err := callRefreshEndpoint(ctx, regOpts, currentWorkerID)
		if err == nil {
			logger.Info("[runtime-token]",
				"event", "refresh",
				"workerId", currentWorkerID,
				"reason", reason,
			)
			return r.rememberValidated(regOpts, &RefreshTokenResult{
				Mode:                  "refresh",
				WorkerID:              currentWorkerID,
				RuntimeToken:          fresh.RuntimeToken,
				RuntimeTokenExpiresAt: fresh.RuntimeTokenExpiresAt,
				HeartbeatInterval:     fresh.HeartbeatInterval,
				PollInterval:          fresh.PollInterval,
				SessionShim:           cloneSessionShimCredentialReceipt(fresh.SessionShim),
				Reason:                reason,
			})
		}
		// 404 / 405 → the durable registration is gone, or the endpoint is
		// not deployed. Fall through. Anything else surfaces as an error so
		// the caller logs + retries on next tick WITHOUT burning the
		// identity — a 5xx or a network blip is never evidence that a
		// registration has been retired.
		if !isMissingEndpointOrWorker(err) {
			logger.Warn("[runtime-token]",
				"event", "refresh.error",
				"workerId", currentWorkerID,
				"reason", reason,
				"err", err.Error(),
			)
			return nil, fmt.Errorf("refresh probe failed: %w", err)
		}
		logger.Info("[runtime-token]",
			"event", "refresh.unavailable",
			"workerId", currentWorkerID,
			"reason", reason,
			"detail", "orchestrator would not re-present this registration; checking for a sibling lane's registration before minting a new identity",
		)
	}

	// 2. Adopt a sibling lane's registration from the shared credential cache
	// rather than minting a competing one. Verified by the same probe, so an
	// unusable cache entry can never be adopted.
	if probeUsable {
		if adopted := r.adoptCachedRegistration(ctx, regOpts, currentWorkerID, reason); adopted != nil {
			return r.rememberValidated(regOpts, adopted)
		}
	}

	// 3. Full re-register — mints a NEW worker identity. Rate-limited: see
	// DefaultMinReregisterInterval.
	if wait := r.reregisterCooldown(regOpts); wait > 0 {
		logger.Warn("[runtime-token]",
			"event", "reregister.throttled",
			"workerId", currentWorkerID,
			"reason", reason,
			"retryIn", wait.String(),
			"detail", "refused to mint another worker identity this soon after the last one; the caller retries on its next tick",
		)
		return nil, fmt.Errorf(
			"reregister throttled: last re-registration was under %s ago (retry in %s)",
			minReregisterInterval(regOpts), wait,
		)
	}

	logger.Info("[runtime-token]",
		"event", "reregister.registration-gone",
		"workerId", currentWorkerID,
		"reason", reason,
		"detail", "orchestrator has no durable record of this worker and no sibling registration was available; minting a new identity",
	)

	regOpts.ForceReregister = true
	rr, rerr := Register(ctx, regOpts)
	if rerr != nil {
		logger.Warn("[runtime-token]",
			"event", "reregister.error",
			"workerId", currentWorkerID,
			"reason", reason,
			"err", rerr.Error(),
		)
		return nil, fmt.Errorf("reregister: %w", rerr)
	}
	r.markReregistered()
	swapped := rr.WorkerID != "" && rr.WorkerID != currentWorkerID
	logger.Info("[runtime-token]",
		"event", "reregister",
		"workerId", rr.WorkerID,
		"oldWorkerId", currentWorkerID,
		"reason", reason,
		"workerIdSwapped", swapped,
	)
	return r.rememberValidated(regOpts, &RefreshTokenResult{
		Mode:                     "reregister",
		WorkerID:                 rr.WorkerID,
		RuntimeToken:             rr.RuntimeToken,
		RuntimeTokenExpiresAt:    rr.RuntimeTokenExpiresAt,
		HeartbeatInterval:        rr.HeartbeatInterval,
		PollInterval:             rr.PollInterval,
		SessionShim:              cloneSessionShimCredentialReceipt(rr.SessionShim),
		RegistrationTokenSwapped: swapped,
		Reason:                   reason,
	})
}

// adoptCachedRegistration looks for a DIFFERENT worker id in the on-disk
// credential cache and, if the orchestrator will re-present it, returns
// credentials for it.
//
// This is the cross-lane repair. Every lane that refreshes writes the result
// to the shared cache, so a lane that has fallen behind finds its sibling's
// identity here. Adopting it converges the process on ONE registration;
// re-registering instead would retire the sibling's row and start the mutual
// eviction loop described on RefreshRuntimeToken. Adoption is always verified
// by the refresh probe, so a stale or foreign cache entry is rejected rather
// than presented.
//
// Returns nil when there is nothing safe to adopt.
func (r *runtimeTokenRefresher) adoptCachedRegistration(
	ctx context.Context,
	regOpts RegistrationOptions,
	currentWorkerID string,
	reason string,
) *RefreshTokenResult {
	if regOpts.JWTPath == "" {
		return nil
	}
	cached, err := LoadCachedJWT(regOpts.JWTPath)
	if err != nil || cached == nil {
		return nil
	}
	candidate := cached.WorkerID
	if candidate == "" || candidate == currentWorkerID || isStubRuntimeToken(cached.RuntimeToken) {
		return nil
	}
	if !cachedMatchesSessionShim(cached, regOpts.SessionShim) {
		return nil
	}

	fresh, probeErr := callRefreshEndpoint(ctx, regOpts, candidate)
	if probeErr != nil {
		return nil
	}
	slog.Default().Info("[runtime-token]",
		"event", "refresh.adopted-sibling",
		"workerId", candidate,
		"staleWorkerId", currentWorkerID,
		"reason", reason,
		"detail", "another lane in this process holds a live registration; adopting it instead of minting a competing identity",
	)
	return &RefreshTokenResult{
		Mode:                  "refresh",
		WorkerID:              candidate,
		RuntimeToken:          fresh.RuntimeToken,
		RuntimeTokenExpiresAt: fresh.RuntimeTokenExpiresAt,
		HeartbeatInterval:     fresh.HeartbeatInterval,
		PollInterval:          fresh.PollInterval,
		SessionShim:           cloneSessionShimCredentialReceipt(fresh.SessionShim),
		Reason:                reason,
	}
}

// isMissingEndpointOrWorker reports whether the refresh probe failed with the
// only two statuses that justify abandoning the current identity: 404 (no such
// worker, or no such endpoint) and 405 (endpoint not implemented for POST).
// Everything else — 5xx, timeouts, transport errors, an auth failure on the
// registration token — is a reason to retry, never to re-register.
func isMissingEndpointOrWorker(err error) bool {
	var probeErr *refreshHTTPError
	if !errors.As(err, &probeErr) {
		return false
	}
	return probeErr.status == http.StatusNotFound ||
		probeErr.status == http.StatusMethodNotAllowed
}

// persistRefreshedToken writes the refreshed credentials to the on-disk JWT
// cache (daemon.jwt) so processes that read the token from disk — the
// per-session credential resolver and the runner's platform client — pick up
// the fresh token instead of the now-stale cached one. The daemon's heartbeat
// and poll loops swap the token in memory, but anything reading daemon.jwt does
// not see that swap; without this write they keep presenting the expired token
// (the platform then 307-redirects credential snapshots to an HTML login page
// and 401s status updates — the stale-cached-token credential root cause).
//
// Best-effort by contract: callers log and continue on error; a cache-write
// failure must never abort the refresh, since the in-memory swap already keeps
// poll + heartbeat alive. Returns nil for an empty path or nil result.
func persistRefreshedToken(jwtPath string, result *RefreshTokenResult, nowFn func() time.Time) error {
	if jwtPath == "" || result == nil {
		return nil
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return SaveCachedJWT(jwtPath, &RegisterResponse{
		WorkerID:              result.WorkerID,
		RuntimeToken:          result.RuntimeToken,
		HeartbeatInterval:     result.HeartbeatInterval,
		PollInterval:          result.PollInterval,
		RuntimeTokenExpiresAt: result.RuntimeTokenExpiresAt,
		SessionShim:           cloneSessionShimCredentialReceipt(result.SessionShim),
	}, nowFn())
}

// refreshHTTPError carries the HTTP status from the refresh probe so
// callers can distinguish "endpoint missing" (404 / 405) from other
// failures.
type refreshHTTPError struct {
	status int
	body   string
}

func (e *refreshHTTPError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("refresh: HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("refresh: HTTP %d", e.status)
}

// refreshResponse mirrors the (planned) platform refresh-endpoint
// response body. Only RuntimeToken is load-bearing today; the cadence
// fields are honoured when present and ignored when absent (existing
// services keep their current cadence).
type refreshResponse struct {
	RuntimeToken          string                        `json:"runtimeToken"`
	RuntimeTokenExpiresAt string                        `json:"runtimeTokenExpiresAt,omitempty"`
	HeartbeatInterval     int                           `json:"heartbeatInterval,omitempty"`
	PollInterval          int                           `json:"pollInterval,omitempty"`
	SessionShim           *SessionShimCredentialReceipt `json:"sessionShim,omitempty"`
}

// callRefreshEndpoint posts to the platform's refresh probe with the
// registration token in Authorization: Bearer + the workerId in the
// URL path. The path the daemon probes is
// `/api/workers/<id>/refresh-token`; until the platform side ships
// its companion handler the platform 404s and we fall through to
// re-register.
func callRefreshEndpoint(
	ctx context.Context,
	opts RegistrationOptions,
	workerID string,
) (*refreshResponse, error) {
	if err := opts.SessionShim.validate(); err != nil {
		return nil, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(opts.OrchestratorURL, "/") +
		"/api/workers/" + workerID + "/refresh-token"
	refreshAttestation := struct {
		SessionShimHostAttestation
		WorkareaExecutors []workarea.ExecutorCapabilityAttestation `json:"workareaExecutors,omitempty"`
	}{
		SessionShimHostAttestation: cloneSessionShimHostAttestation(opts.SessionShim),
		WorkareaExecutors:          append([]workarea.ExecutorCapabilityAttestation(nil), opts.WorkareaExecutors...),
	}
	bodyBytes, err := json.Marshal(refreshAttestation)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	body := bytes.NewReader(bodyBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opts.RegistrationToken)
	req.Header.Set("User-Agent", "rensei-daemon/"+Version)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 {
		errBuf, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return nil, &refreshHTTPError{status: res.StatusCode, body: strings.TrimSpace(string(errBuf))}
	}
	var resp refreshResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.RuntimeToken == "" {
		return nil, fmt.Errorf("refresh response missing runtimeToken")
	}
	if err := validateSessionShimCredentialReceipt(opts.SessionShim, resp.SessionShim, workerID); err != nil {
		return nil, fmt.Errorf("validate refresh session shim receipt: %w", err)
	}
	return &resp, nil
}
