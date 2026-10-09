//go:build darwin || linux

package sessionshim

import (
	"errors"
	"fmt"
	"syscall"
)

// ProcessGroupGone reports whether process group pgid has no live member left.
// It is the group-gone proof a headless runner gives the shim as its reap proof
// (RunnerProcess.HarnessGroupsReaped), and it never depends on the runner's
// PID, which under ADR-2026-10-07 D1 Option C is the shim's own process.
//
// kill(-pgid, 0) delivers no signal; it only asks the kernel whether the group
// has a member. ESRCH proves that none exists, and a nil answer proves one
// does. EPERM means no member could be signalled, which is ambiguous: on
// Linux it means a live member owned by another user, but darwin also answers
// EPERM for a group that still exists with no live member left while it is
// being torn down. So on EPERM the members are counted directly
// (processGroupHasLiveMember) rather than guessed. Any other error, or a count
// that cannot be read, is unproved and returned.
//
// A runner waits its harness before it asks, so "gone" means reaped rather
// than merely exited. The caller's own process group can never be proved gone,
// and pgid 0 or 1 would name the caller's group or every process, so all three
// are refused rather than probed.
func ProcessGroupGone(pgid int) (bool, error) {
	return processGroupGone(pgid, syscall.Kill)
}

// processGroupGone is ProcessGroupGone with the probe injected, so the EPERM
// branch can be driven deterministically.
func processGroupGone(pgid int, kill func(int, syscall.Signal) error) (bool, error) {
	if pgid <= 1 {
		return false, fmt.Errorf("%w: process group %d cannot be probed", ErrProcessIdentity, pgid)
	}
	if pgid == syscall.Getpgrp() {
		return false, fmt.Errorf("%w: process group %d is this process's own group", ErrProcessIdentity, pgid)
	}
	err := kill(-pgid, 0)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, syscall.ESRCH):
		return true, nil
	case errors.Is(err, syscall.EPERM):
		live, countErr := processGroupHasLiveMember(pgid)
		if countErr != nil {
			return false, fmt.Errorf("sessionshim: count members of process group %d: %w", pgid, countErr)
		}
		return !live, nil
	default:
		return false, fmt.Errorf("sessionshim: probe process group %d: %w", pgid, err)
	}
}
