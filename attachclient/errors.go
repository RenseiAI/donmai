package attachclient

import (
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// ErrEpochStale is the terminal sentinel RunHost returns when current grant
// authority proves a successor PTY epoch, or when bounded same-epoch recovery
// after error.code = epoch-stale (§ 6.2) is exhausted. An equal local epoch is
// ambiguous while a prior carrier may be half-open and therefore retries first.
// It is distinct from a transient disconnect (which reconnects with backoff).
var ErrEpochStale = errors.New("attachclient: epoch-stale — host authority was superseded or bounded recovery was exhausted (§6.2)")

// Internal authority classifications returned by validatedToken. Both stay
// private: callers observe only RunHost's public ErrEpochStale or context/error
// result, never a second wire-visible taxonomy.
var (
	errEpochGrantSuperseded = errors.New("attachclient: current grant belongs to a newer local PTY epoch")
	errEpochGrantAmbiguous  = errors.New("attachclient: current grant does not yet match the local PTY epoch")
)

// RelayStopError is the terminal error RunHost returns when the relay closes the
// leg with a non-retryable error control (retryable == false, § 7) other than
// epoch-stale or ring-miss (those two get their own dispositions — ErrEpochStale
// and RelayRingMissError respectively). Like ErrEpochStale it stops RunHost
// rather than reconnecting.
type RelayStopError struct {
	Code    attachwire.ErrorCode
	Message string
}

func (e *RelayStopError) Error() string {
	return fmt.Sprintf("attachclient: relay terminated the host leg (code=%s, retryable=false): %s", e.Code, e.Message)
}

func isRelayStop(err error) bool {
	var e *RelayStopError
	return errors.As(err, &e)
}

// RelayRingMissError is the RESET-AND-RETRY signal used internally by the
// reconnect loop (never returned from RunHost itself — see the ring-miss case
// in host.run) when the relay has lost its ring/room history for this session.
// The dominant cause is a relay restart: § 13 states plainly that "the ring and
// pen state are relay-local; after a relay restart every viewer resume is a
// ring miss ... This is the designed repair path, sound because ring misses are
// always safe (they cost a snapshot, never correctness)." The host leg is no
// exception — it is the SAME relay-state loss, just observed from the producer
// side instead of a viewer.
//
// Two distinct call sites collapse to this type:
//
//   - the relay explicitly answers a control frame with error.code = ring-miss
//     (any carrier, § 7) — see handleControl;
//   - the degraded lane's own 409 rewind (§ 14) cannot satisfy the relay's
//     requested ack from the host's own retained local ring either — the host
//     lost the frames just as surely as the relay did — see runDegraded's
//     postRewind handling.
//
// Unlike RelayStopError and ErrEpochStale, a ring miss is NEVER a reason to
// give up: the host still owns the authoritative PTY, so the only correct
// response is to drop the local resume position and re-attach fresh (a new
// carrier attempt with fromSeq 0, i.e. no resume_from), letting the relay
// rebuild the room from a requested Snapshot (§ 12, § 13).
type RelayRingMissError struct {
	Code    attachwire.ErrorCode
	Message string
}

func (e *RelayRingMissError) Error() string {
	return fmt.Sprintf("attachclient: relay lost ring state (code=%s): %s — resetting for a fresh re-attach (§13)", e.Code, e.Message)
}

func isRelayRingMiss(err error) bool {
	var e *RelayRingMissError
	return errors.As(err, &e)
}

// errUpgraded is an internal control-flow signal (never returned from RunHost):
// the degraded carrier detected WSS is reachable again and drained, so RunHost
// switches back to the WSS lane (§ 14 upgrade-back).
var errUpgraded = errors.New("attachclient: upgraded back to the WSS lane")

// ErrV2CarrierCursorDrift classifies the ONE fresh-dial refusal a composing
// daemon may treat as ambiguous rather than terminal: the caller's local
// durable acknowledgement floor and the signed carrier boundary disagree in the
// direction that no live carrier can produce.
//
// The two cursors are deliberately independent. The local floor is a
// host-local, fsync-backed acknowledgement successor; the carrier boundary is
// the external carrier's own durable journal high water. The carrier is
// ALLOWED to be ahead — every frame it has journaled but not yet acknowledged
// back to the local sidecar lives in that window, and an abrupt daemon exit
// freezes it there permanently. Only the reverse skew is evidence of anything:
// a local floor above the signed boundary means the proof this dial is holding
// is stale, and the repair is to prepare a new one, not to condemn the lineage.
//
// A caller distinguishes it with errors.Is/errors.As through its own wrapping.
var ErrV2CarrierCursorDrift = errors.New("attachclient: v2 local durable high-water is ahead of the signed carrier boundary")

// V2CarrierCursorDriftError names both cursors so an operator can read the
// direction and the size of the skew without re-deriving either. It carries no
// credential, correlation, or frame bytes — only the two sequence numbers.
type V2CarrierCursorDriftError struct {
	// DurableHighWater is the caller-supplied local acknowledgement floor.
	DurableHighWater uint64
	// CarrierBoundary is the signed carrier boundary N from the authenticated
	// proof-v2 bearer.
	CarrierBoundary uint64
}

func (e *V2CarrierCursorDriftError) Error() string {
	return fmt.Sprintf(
		"attachclient: v2 local durable high-water %d is ahead of the signed carrier boundary %d (stale proof; re-prepare)",
		e.DurableHighWater, e.CarrierBoundary,
	)
}

func (e *V2CarrierCursorDriftError) Unwrap() error { return ErrV2CarrierCursorDrift }
