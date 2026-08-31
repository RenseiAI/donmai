//go:build windows

package codex

import "os"

// processAliveOS reports whether pid is a currently running process.
//
// CAVEAT, untested on real windows: this assumes os.FindProcess opens a real
// process handle and fails for a pid that does not exist (unlike unix, where
// FindProcess always succeeds and liveness needs a separate signal-0 probe).
// If that assumption is ever wrong in the "reports alive when it is not"
// direction, the practical effect is fail-safe, not fail-open: SweepOrphans's
// dead-owner reclaim path (see sweepOne) would simply never fire on windows —
// directories would accumulate exactly as they did before this package
// existed, never be wrongly reclaimed out from under a live owner.
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// processLooksLikeCodexOS has no portable windows implementation in this
// package today (no `ps`, and named interactive sessions — the only shape
// that tracks a live child identity at all — are unix-only per
// validateNamedInteractiveTransport). Always false: SweepOrphans's
// termination path (see sweepOne) treats that as "cannot prove binary
// identity" and never signals anything on windows. Directory reclamation for
// a confirmed-dead owner is unaffected — this only gates the termination
// path.
func processLooksLikeCodexOS(int, string) bool { return false }

// verifyManifestDirectoryOwnershipOS has no ACL-based implementation on
// windows today. Unverified, not verified-and-passing: windows' per-user
// %TEMP% layout differs fundamentally from a shared, world-writable unix
// /tmp (the concrete threat verifyManifestDirectoryOwnership exists to
// close — see its doc comment), but that difference has not been audited
// here, so treat this the same way as F10's liveness caveat above: a known,
// documented gap, not a proven-safe default.
func verifyManifestDirectoryOwnershipOS(os.FileInfo) error { return nil }
