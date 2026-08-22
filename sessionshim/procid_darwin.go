//go:build darwin

package sessionshim

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// processStartTime reads the kernel's authoritative start time for pid.
//
// The value comes from the process table rather than from anything the process
// tells us about itself, which is what makes it usable as an anti-reuse
// discriminator: a new process that inherits a recycled pid cannot present the
// old one's start time.
func processStartTime(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if isNoSuchProcess(err) {
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("sessionshim: kern.proc.pid %d: %w", pid, err)
	}
	if kp == nil {
		return 0, os.ErrNotExist
	}
	tv := kp.Proc.P_starttime
	return tv.Sec*1e9 + int64(tv.Usec)*1e3, nil
}

// isNoSuchProcess maps the several ways this sysctl reports an unknown pid onto
// one answer.
//
// EIO is the surprising member and the one that matters most in practice: the
// kern.proc.pid MIB succeeds for an unknown pid but returns ZERO bytes, and the
// x/sys wrapper turns that short read into EIO. This MIB performs no actual I/O,
// so EIO here means "the kernel had no record", not a device failure. Treating
// it as a hard error would make every genuinely-dead process look like an
// unverifiable one — and an unverifiable process is quarantined rather than
// classified stale, which would leak capacity on every ordinary session end.
func isNoSuchProcess(err error) bool {
	return errors.Is(err, unix.ESRCH) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EIO) ||
		errors.Is(err, unix.ENOENT)
}
