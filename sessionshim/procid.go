package sessionshim

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrProcessIdentity reports that a PID/start-identity pair could not be
// confirmed live.
var ErrProcessIdentity = errors.New("sessionshim: process identity mismatch")

// ProcessIdentity is a PID paired with the OS-reported start time that
// disambiguates it.
//
// The pairing is not defensive over-engineering: PID reuse is ordinary on a
// busy host, and §D10 requires that a registry record whose PID has been reused
// classify as STALE — never as a live shim, and never as something to signal.
// A bare PID cannot express that distinction, so this type is the only currency
// the adoption and janitor paths accept.
type ProcessIdentity struct {
	PID       int
	StartedAt int64 // Unix nanoseconds, OS-reported
}

// Self returns the running process's own identity.
func Self() (ProcessIdentity, error) {
	pid := os.Getpid()
	started, err := processStartTime(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("sessionshim: own process identity: %w", err)
	}
	return ProcessIdentity{PID: pid, StartedAt: started}, nil
}

// Alive reports whether the process named by p is still running AND is still the
// same process that was recorded.
//
// Both halves are required. "The PID exists" alone answers the wrong question:
// after reuse it is true of a completely unrelated process, and acting on that
// answer is how a janitor signals the wrong target.
func (p ProcessIdentity) Alive() (bool, error) {
	if p.PID <= 0 {
		return false, fmt.Errorf("%w: non-positive pid %d", ErrProcessIdentity, p.PID)
	}
	started, err := processStartTime(p.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil // gone: not an error, just not alive
		}
		return false, err
	}
	if started != p.StartedAt && !matchesLegacyStartEncoding(p.PID, p.StartedAt) {
		// The pid is live but it is a DIFFERENT process. Reporting "not alive"
		// is the safe answer: the recorded process is gone.
		return false, nil
	}
	return true, nil
}

// legacyStartEncodingCeiling bounds the values the legacy-encoding fallback
// will even consider.
//
// A Unix-nanosecond timestamp for any instant after 1970-01-12 exceeds it, and
// a clock-tick count since boot cannot reach it (a decade of uptime at 1000 Hz
// is around 3e11). So a recorded value below the ceiling cannot be a start time
// in the unit this package reports today, and one above it never reaches the
// fallback at all.
const legacyStartEncodingCeiling = int64(1e15)

// matchesLegacyStartEncoding reports whether recorded is the value a binary
// PREDATING this platform's current start-time encoding would have written for
// pid — and therefore still names this exact process.
//
// Without it, changing a platform's encoding orphans every LIVE workload the
// moment the controller is upgraded: the recorded identity stops matching, the
// running process is classified stale, and its session is lost. The fallback is
// exactly as discriminating as the encoding it accepts, because it is still
// that pid's own kernel-reported start read fresh from the OS — a reused pid
// cannot produce the recorded value. The ceiling keeps a genuine current-format
// identity from ever being compared this way.
func matchesLegacyStartEncoding(pid int, recorded int64) bool {
	if recorded <= 0 || recorded >= legacyStartEncodingCeiling {
		return false
	}
	legacy, ok := legacyStartEncoding(pid)
	return ok && legacy == recorded
}

// Matches reports whether other names the same process incarnation.
func (p ProcessIdentity) Matches(other ProcessIdentity) bool {
	return p.PID == other.PID && p.StartedAt == other.StartedAt && p.PID > 0
}

// String renders the identity for diagnostics.
func (p ProcessIdentity) String() string {
	return fmt.Sprintf("pid=%d start=%d", p.PID, p.StartedAt)
}
