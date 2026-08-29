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
	id, err := ProcessIdentityFor(os.Getpid())
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("sessionshim: own process identity: %w", err)
	}
	return id, nil
}

// ProcessIdentityFor pins a live process by PID, reading the OS-reported start
// time that disambiguates it from a later reuse of the same PID.
//
// Pinning has to happen while the process is still running: once it exits the
// start time is unreadable, and a tombstone written from a bare PID could not
// tell "this group is gone" from "a new process reused the pid". Anything that
// owns a child it will later have to prove dead pins it here at spawn time.
func ProcessIdentityFor(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, fmt.Errorf("%w: non-positive pid %d", ErrProcessIdentity, pid)
	}
	started, err := processStartTime(pid)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("sessionshim: process identity for pid %d: %w", pid, err)
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
	if started != p.StartedAt {
		// The pid is live but it is a DIFFERENT process. Reporting "not alive"
		// is the safe answer: the recorded process is gone.
		return false, nil
	}
	return true, nil
}

// Matches reports whether other names the same process incarnation.
func (p ProcessIdentity) Matches(other ProcessIdentity) bool {
	return p.PID == other.PID && p.StartedAt == other.StartedAt && p.PID > 0
}

// String renders the identity for diagnostics.
func (p ProcessIdentity) String() string {
	return fmt.Sprintf("pid=%d start=%d", p.PID, p.StartedAt)
}
