package codex

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// NOTE_EXIT observes death without reaping. The zombie continues reserving the
// leader's PID until the lifecycle owner has made its last group operation.
func observeOwnedProcessExit(pid int) error {
	if pid <= 0 {
		return errors.New("invalid owned process ID")
	}
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	unix.CloseOnExec(fd)
	changes := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(fd, changes, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		// A child that exited before registration is already waitable. The
		// subsequent group inspection must still prove the unreaped anchor.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		changes = nil
		if n > 0 {
			if events[0].Flags&unix.EV_ERROR != 0 && events[0].Data != 0 {
				if events[0].Data != int64(syscall.ESRCH) {
					return fmt.Errorf("observe owned process exit: errno %d", events[0].Data)
				}
			}
			return nil
		}
	}
}
