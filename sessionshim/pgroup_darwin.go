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
//
// A zombie (SZOMB) member is unproved, not gone: an exited-but-unwaited
// process still exists in the kernel's table, and its parent — the runner,
// which must wait its harness before asking for the reap proof — may simply
// not have reaped it yet. Reporting a zombie-only group as gone would claim
// a reap that has not happened, while Linux (where a zombie still answers
// kill, so the group reads as alive) reports the same group as present. The
// two platforms agree only when darwin treats zombies as unproved: the
// caller retries after its harness wait, and the tombstone claims reaped
// only for a group with no member left at all.
func liveGroupMember(pgid int, procs []unix.KinfoProc, err error) (bool, error) {
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return false, nil
		}
		return false, err
	}
	for _, kp := range procs {
		if int(kp.Eproc.Pgid) == pgid {
			if kp.Proc.P_stat == darwinZombie {
				return false, errZombieMemberPending
			}
			return true, nil
		}
	}
	return false, nil
}

// errZombieMemberPending reports a process group whose table entry still
// names a member, but only a zombie one: the reap is unproved, not
// disproved. Callers record the tombstone as not reaped and — after the
// runner has waited its harness — may ask again. It wraps no syscall
// error; errors.Is matches it by identity.
var errZombieMemberPending = errors.New("sessionshim: process group still names a zombie member; reap unproved")
