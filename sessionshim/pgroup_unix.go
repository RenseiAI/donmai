//go:build darwin || linux

package sessionshim

import (
	"errors"
	"fmt"
	"syscall"
)

// ProcessGroupGone reports whether process group pgid has no member left. It
// is the group-gone proof a headless runner gives the shim as its reap proof
// (RunnerProcess.HarnessGroupsReaped), and it never depends on the runner's
// PID, which under ADR-2026-10-07 D1 Option C is the shim's own process.
//
// kill(-pgid, 0) delivers no signal; it only asks the kernel whether the group
// has a member. ESRCH is the proof that none exists. A nil answer or EPERM
// (a member owned by someone else) means the group is still populated. Any
// other error is unproved and returned. Some kernels still count an exited
// but unwaited (zombie) member, so a runner waits its harness before it asks;
// that is also what makes "gone" mean reaped rather than merely exited.
//
// The caller's own process group can never be proved gone, and pgid 0 or 1
// would name the caller's group or every process, so all three are refused
// rather than probed.
func ProcessGroupGone(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, fmt.Errorf("%w: process group %d cannot be probed", ErrProcessIdentity, pgid)
	}
	if pgid == syscall.Getpgrp() {
		return false, fmt.Errorf("%w: process group %d is this process's own group", ErrProcessIdentity, pgid)
	}
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return false, nil
	case errors.Is(err, syscall.ESRCH):
		return true, nil
	default:
		return false, fmt.Errorf("sessionshim: probe process group %d: %w", pgid, err)
	}
}
