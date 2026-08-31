//go:build windows

package codex

import "os"

// processAliveOS reports whether pid is a currently running process. Unlike
// unix, os.FindProcess on windows opens a real process handle and fails
// immediately for a pid that does not exist, so existence alone is proof.
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// processLooksLikeCodexOS has no portable windows implementation in this
// package today (no `ps`, and named interactive sessions — the only shape
// that tracks a live child PID at all — are unix-only per
// validateNamedInteractiveTransport). Always false: the sweep's ownership
// gate (see SweepOrphans) treats that as "cannot prove identity" and skips
// termination, never as license to terminate. Directory reclamation by age
// and dead-owner-PID is unaffected — this only gates the termination path.
func processLooksLikeCodexOS(int, string) bool { return false }
