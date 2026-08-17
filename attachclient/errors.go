package attachclient

import (
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// ErrEpochStale is the terminal sentinel RunHost returns when the relay rejects
// the host leg with error.code = epoch-stale (§ 6.2): a newer host process for
// the session exists, so this process is a zombie and MUST NOT keep retrying.
// It is distinct from a transient disconnect (which reconnects with backoff).
var ErrEpochStale = errors.New("attachclient: epoch-stale — a newer host process owns the room (§6.2)")

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
