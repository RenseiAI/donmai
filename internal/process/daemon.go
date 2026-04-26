//go:build !windows

package process

import (
	"fmt"
	"os"
	"syscall"
)

// Daemonize re-execs the current process as a background daemon.
//
// If the environment variable AF_DAEMON=1 is set, Daemonize returns
// (true, 0, nil) — the current process is already the daemon child and the
// caller should continue its work.
//
// Otherwise Daemonize re-execs os.Args[0] with os.Args[1:], appending
// AF_DAEMON=1 to the environment, with Setsid set and stdin/stdout/stderr
// redirected to /dev/null. It returns (false, childPID, nil) — the caller
// should print a "started PID <childPID>" message and then call os.Exit(0).
func Daemonize() (isChild bool, childPID int, err error) {
	if os.Getenv("AF_DAEMON") == "1" {
		return true, 0, nil
	}

	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return false, 0, fmt.Errorf("process: open /dev/null: %w", err)
	}
	defer devNull.Close()

	attr := &os.ProcAttr{
		Env:   append(os.Environ(), "AF_DAEMON=1"),
		Files: []*os.File{devNull, devNull, devNull},
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	proc, err := os.StartProcess(os.Args[0], os.Args, attr)
	if err != nil {
		return false, 0, fmt.Errorf("process: start daemon: %w", err)
	}

	return false, proc.Pid, nil
}
