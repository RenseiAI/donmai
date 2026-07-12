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
// epoch-stale, or when an unrecoverable ring miss occurs on the degraded-lane
// rewind (§ 14). Like ErrEpochStale it stops RunHost rather than reconnecting.
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

// errUpgraded is an internal control-flow signal (never returned from RunHost):
// the degraded carrier detected WSS is reachable again and drained, so RunHost
// switches back to the WSS lane (§ 14 upgrade-back).
var errUpgraded = errors.New("attachclient: upgraded back to the WSS lane")
