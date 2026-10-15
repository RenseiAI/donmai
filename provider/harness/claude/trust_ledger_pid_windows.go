//go:build windows

package claude

import (
	"os"
)

// trustLedgerPidAlive reports whether pid is a currently running process.
//
// Fail-closed caveat: on Windows os.FindProcess succeeds for handles it
// can open, and there is no signal-0 probe — a pid the lookup can open
// reads as live. If that ever over-reports (a dead pid that still opens),
// the effect is fail-safe, not fail-open: the sweep keeps rows it should
// have drained (trust accumulates exactly as before this ledger existed)
// rather than undoing a live session's trust.
func trustLedgerPidAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	_, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	return true, nil
}
