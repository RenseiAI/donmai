//go:build darwin

package sessionshim

import (
	"errors"

	"golang.org/x/sys/unix"
)

// darwinZombie is SZOMB from <sys/proc.h>: an exited process that has not been
// waited. It holds no running code and is not a live group member.
const darwinZombie = 5

// processGroupHasLiveMember counts the members of process group pgid from the
// kernel's process table (kern.proc.pgrp), which lists every process in the
// group whatever user owns it. It is how ProcessGroupGone resolves darwin's
// EPERM, which kill also answers for a group with no live member left.
func processGroupHasLiveMember(pgid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	return liveGroupMember(pgid, procs, err)
}

// liveGroupMember reads a kern.proc.pgrp answer. An empty answer, or ESRCH,
// proves the group has no member. Any other error is unproved and returned:
// this is the reap proof, so a failed read must never become "gone" (the
// per-process isNoSuchProcess mapping also accepts EINVAL, EIO and ENOENT,
// which say nothing about a whole group).
func liveGroupMember(pgid int, procs []unix.KinfoProc, err error) (bool, error) {
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		return false, err
	}
	for _, kp := range procs {
		if int(kp.Eproc.Pgid) == pgid && kp.Proc.P_stat != darwinZombie {
			return true, nil
		}
	}
	return false, nil
}
