package rulesetsnapshot

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotConfigured means no snapshot source is configured
	// (Config.Endpoint == ""). This is the default, self-hosted posture: no
	// behaviour change when nothing was ever asked for.
	ErrNotConfigured = errors.New("rulesetsnapshot: no snapshot source is configured")

	// ErrNoCachedSnapshot means a source IS configured but no snapshot has
	// ever verified successfully — neither over the network nor from the
	// on-disk last-known-good copy.
	ErrNoCachedSnapshot = errors.New("rulesetsnapshot: no cached ruleset snapshot is available yet")

	// ErrVerificationFailed covers a content-hash mismatch, a signature that
	// does not verify, or a malformed response. The previous cached
	// snapshot (if any) is always kept — see Client.Refresh.
	ErrVerificationFailed = errors.New("rulesetsnapshot: snapshot failed verification")

	// ErrKeyUnresolved means a well-formed snapshot's signingKeyId could not
	// be resolved to a trusted Ed25519 public key.
	ErrKeyUnresolved = errors.New("rulesetsnapshot: signing key could not be resolved to a trusted public key")

	// ErrExpired is the loud, typed TTL refusal: the cached snapshot's age
	// has crossed the configured RefuseAfter bound. See ExpiredError.
	ErrExpired = errors.New("rulesetsnapshot: cached ruleset snapshot exceeded its refuse-after TTL")

	// ErrPermissionRefused means the cached snapshot's pool/host inventory
	// and capacity-profile sections no longer grant the admitted cell's
	// target pool. See PermissionRefusedError.
	ErrPermissionRefused = errors.New("rulesetsnapshot: pool is not currently eligible per the cached ruleset snapshot")
)

// ExpiredError is the typed refusal returned once a cached snapshot's age
// exceeds RefuseAfter. It is always loud (a returned error, never a silent
// stall or a fall-through to "permitted") and never fires before the bound
// — a snapshot between DegradedAfter and RefuseAfter is still served, just
// flagged degraded.
type ExpiredError struct {
	Rev         string
	Age         time.Duration
	RefuseAfter time.Duration
}

func (e *ExpiredError) Error() string {
	return fmt.Sprintf("rulesetsnapshot: cached snapshot %s is %s old, exceeding the %s refuse-after TTL",
		e.Rev, e.Age.Round(time.Second), e.RefuseAfter)
}

func (e *ExpiredError) Unwrap() error { return ErrExpired }

// PermissionRefusedError names the pool and reason a fail-static permission
// re-check refused a claim.
type PermissionRefusedError struct {
	PoolID string
	Reason string
}

func (e *PermissionRefusedError) Error() string {
	return fmt.Sprintf("rulesetsnapshot: pool %q refused: %s", e.PoolID, e.Reason)
}

func (e *PermissionRefusedError) Unwrap() error { return ErrPermissionRefused }
