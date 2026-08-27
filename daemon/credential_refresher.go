package daemon

// credential_refresher.go — the ONE place worker credentials get refreshed and
// handed to the services that present them.
//
// WHY THIS EXISTS
//
// A daemon runs several long-lived lanes that each present the same worker
// credentials: a heartbeat lane, a poll lane, and whatever else an embedder
// attaches. Each lane holds its own copy, and each lane independently notices
// when the orchestrator rejects it.
//
// The recovery hook is per-lane by construction — the rejected lane calls it
// and receives the refreshed credentials back. Nothing in that shape tells the
// OTHER lanes anything. So every call site had to remember, by hand, to push
// the result to every sibling lane. One did. One did not, and the one that did
// not produced a permanent re-registration loop: when a refresh changed the
// worker id, the un-updated sibling kept presenting an identity the
// orchestrator had just retired, was rejected on its next tick, re-registered,
// and retired the first lane's registration in turn. The two lanes evicted
// each other at tick cadence for as long as the process lived.
//
// A contract that has to be re-implemented at every call site will eventually
// be implemented wrong at one of them, and the failure is silent until it is
// catastrophic. CredentialRefresher makes fan-out structural: lanes are
// attached once, and every refresh reaches all of them. There is no per-call-
// site step left to forget.
//
// It is deliberately NOT tied to Daemon. Any host that runs credential lanes —
// the daemon's own, or an embedder multiplexing several tenant identities on
// one process — uses the same implementation.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// CredentialLane is a long-lived service that presents worker credentials and
// can be handed fresh ones. *HeartbeatService and *PollService both satisfy it.
type CredentialLane interface {
	SetCredentials(workerID, runtimeJWT string)
}

// CredentialRefresherOptions configures a CredentialRefresher.
type CredentialRefresherOptions struct {
	// Registration is the registration this refresher maintains credentials
	// for. Its JWTPath is also the on-disk cache the refresher keeps current.
	Registration RegistrationOptions

	// WorkerID and RuntimeJWT are the credentials in effect at construction —
	// normally straight from the initial Register call.
	WorkerID   string
	RuntimeJWT string

	// ValidateRefresh runs before refreshed credentials become visible to any
	// lane, cache, or callback. An error refuses the refresh atomically.
	ValidateRefresh func(result *RefreshTokenResult) error

	// OnRefreshed runs after every successful refresh, before the result is
	// returned, with the refresher's lock released. Use it for consumers that
	// are not CredentialLanes — a session-detail cache, an embedder's own
	// bookkeeping. Optional.
	OnRefreshed func(result *RefreshTokenResult)
}

// CredentialRefresher owns one registration's credentials and keeps every
// attached lane on the same ones.
type CredentialRefresher struct {
	opts CredentialRefresherOptions

	mu         sync.Mutex
	workerID   string
	runtimeJWT string
	lanes      []CredentialLane
}

// NewCredentialRefresher constructs a refresher seeded with the credentials a
// registration just produced.
func NewCredentialRefresher(opts CredentialRefresherOptions) *CredentialRefresher {
	return &CredentialRefresher{
		opts:       opts,
		workerID:   opts.WorkerID,
		runtimeJWT: opts.RuntimeJWT,
	}
}

// Attach registers lanes to receive every future refresh. Nil lanes are
// ignored so a caller can attach optional services unconditionally.
//
// Attached lanes are brought to the CURRENT credentials immediately: a lane
// attached after a refresh has already happened would otherwise start life on
// credentials the orchestrator has already retired.
func (r *CredentialRefresher) Attach(lanes ...CredentialLane) {
	r.mu.Lock()
	workerID, jwt := r.workerID, r.runtimeJWT
	for _, lane := range lanes {
		if lane == nil {
			continue
		}
		r.lanes = append(r.lanes, lane)
	}
	attached := append([]CredentialLane(nil), r.lanes...)
	r.mu.Unlock()

	for _, lane := range attached {
		lane.SetCredentials(workerID, jwt)
	}
}

// Current returns the credentials in effect.
func (r *CredentialRefresher) Current() (workerID, runtimeJWT string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workerID, r.runtimeJWT
}

// Refresh re-mints the runtime credentials and brings every attached lane onto
// the result.
//
// reason is the classified trigger from the rejected lane
// ("runtime-token-expired", "worker-not-found", …) or "proactive-expiry" from
// the scheduled refresher. It is passed through to RefreshRuntimeToken, which
// owns the decision of whether the existing registration can be re-presented.
func (r *CredentialRefresher) Refresh(ctx context.Context, reason string) (*RefreshTokenResult, error) {
	r.mu.Lock()
	current := r.workerID
	registration := r.opts.Registration
	r.mu.Unlock()

	result, err := RefreshRuntimeToken(ctx, registration, current, reason)
	if err != nil {
		return nil, err
	}
	return r.adopt(result, registration)
}

// adopt is the post-refresh half every path shares: validate, bring every lane
// onto the result, keep the on-disk cache current, and notify.
func (r *CredentialRefresher) adopt(
	result *RefreshTokenResult,
	registration RegistrationOptions,
) (*RefreshTokenResult, error) {
	if r.opts.ValidateRefresh != nil {
		if err := r.opts.ValidateRefresh(result); err != nil {
			return nil, fmt.Errorf("validate refreshed credentials: %w", err)
		}
	}

	r.mu.Lock()
	r.workerID = result.WorkerID
	r.runtimeJWT = result.RuntimeToken
	lanes := append([]CredentialLane(nil), r.lanes...)
	r.mu.Unlock()

	// EVERY lane, not just the one that asked. A lane left on a superseded
	// worker id is rejected on its next tick and re-registers, which retires
	// the registration this refresh just settled on.
	for _, lane := range lanes {
		lane.SetCredentials(result.WorkerID, result.RuntimeToken)
	}

	// Keep the on-disk cache current. Lanes hold their credentials in memory,
	// but anything that RE-READS the cache per call — a credential resolver, a
	// runner's client — keeps presenting the superseded token without this.
	// It is also what lets a lane in another process (or one that fell behind)
	// adopt this registration instead of minting a competing one. Best-effort:
	// a cache-write failure must never abort a refresh that already succeeded
	// in memory.
	if registration.JWTPath != "" {
		if err := persistRefreshedToken(registration.JWTPath, result, registration.Now); err != nil {
			slog.Warn("[runtime-token]",
				"event", "refresh.cache-write-failed",
				"workerId", result.WorkerID,
				"jwtPath", registration.JWTPath,
				"err", err.Error(),
			)
		} else {
			slog.Info("[runtime-token]",
				"event", "refresh.cached",
				"workerId", result.WorkerID,
			)
		}
	}

	if r.opts.OnRefreshed != nil {
		r.opts.OnRefreshed(result)
	}
	return result, nil
}

// SessionShimAttestation returns the host attestation this refresher currently
// presents on every refresh and full re-registration.
func (r *CredentialRefresher) SessionShimAttestation() SessionShimHostAttestation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSessionShimHostAttestation(r.opts.Registration.SessionShim)
}

// DeclareSessionShim swaps the host attestation this refresher presents and
// re-mints on the spot, so the swap and the round trip that carries it cannot
// be separated by an unrelated refresh.
//
// This is how a daemon moves between session-shim postures without re-
// registering: the refresh re-presents THIS worker id carrying the new
// attestation, where a full re-registration would mint a competing identity and
// retire the one every lane is already holding.
//
// A failed round trip restores the previous attestation before returning. The
// refresher must never be left presenting a claim the control plane refused —
// the next ordinary expiry refresh would present it again, unattended.
func (r *CredentialRefresher) DeclareSessionShim(
	ctx context.Context,
	attestation SessionShimHostAttestation,
	reason string,
) (*RefreshTokenResult, error) {
	r.mu.Lock()
	previous := cloneSessionShimHostAttestation(r.opts.Registration.SessionShim)
	r.opts.Registration.SessionShim = cloneSessionShimHostAttestation(attestation)
	r.mu.Unlock()

	r.mu.Lock()
	current := r.workerID
	registration := r.opts.Registration
	r.mu.Unlock()

	// RepresentRuntimeToken, not Refresh: a declaration must never be able to
	// mint a competing worker identity. If this control plane will not
	// re-present the identity we hold with the attestation we are declaring,
	// the honest outcome is that the declaration failed — not a new
	// registration that retires the one every lane is using for an optional
	// feature the caller was merely offering.
	result, err := RepresentRuntimeToken(ctx, registration, current, reason)
	if err == nil {
		result, err = r.adopt(result, registration)
	}
	if err != nil {
		r.mu.Lock()
		r.opts.Registration.SessionShim = previous
		r.mu.Unlock()
		return nil, err
	}
	return result, nil
}

// OnReregister adapts Refresh to the HeartbeatOptions / PollOptions hook
// signature. Wire this into every lane: the lane that takes the rejection
// drives the refresh, and all of them come out of it on the same credentials.
func (r *CredentialRefresher) OnReregister(ctx context.Context, reason string) (workerID, runtimeJWT string, err error) {
	result, err := r.Refresh(ctx, reason)
	if err != nil {
		return "", "", fmt.Errorf("refresh credentials (%s): %w", reason, err)
	}
	return result.WorkerID, result.RuntimeToken, nil
}
