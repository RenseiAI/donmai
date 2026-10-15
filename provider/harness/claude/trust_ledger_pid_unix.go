//go:build !windows

package claude

import (
	"os"
	"syscall"
)

// trustLedgerPidAlive reports whether pid is a currently running process.
// A signal-0 probe delivers no signal but still fails for a pid that does
// not exist; os.FindProcess always succeeds on unix regardless of pid
// validity, so the probe — not the lookup — is the verdict. Any probe
// failure reads as not-alive EXCEPT a lookup error (returned for the
// caller to fail closed on): ESRCH, EPERM-masked absence, and the Go
// "process already finished" verdict for an unallocatable pid all mean
// the row's owner is gone.
func trustLedgerPidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return true, nil
}
