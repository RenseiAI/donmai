//go:build windows

package codex

import (
	"errors"
	"os"
)

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

// readOwnedManifestBytes has no ACL-based ownership implementation on
// windows today, so this package refuses to read an owner manifest there at
// all. Unverified is not verified-and-passing: windows' per-user %TEMP%
// layout differs fundamentally from a shared, world-writable unix /tmp (the
// concrete threat readVerifiedDonmaiOwnerManifest exists to close — see its
// doc comment), but that difference has not been audited here, and an
// unverified manifest drives directory reclamation, not just termination.
//
// The practical effect is the same fail-safe direction as the liveness
// caveat above: on windows every artifact directory is treated as having no
// manifest at all, so the sweep can only ever remove one that is already
// empty. Directories accumulate exactly as they did before this package
// existed, and nothing is ever deleted or signalled on the word of a
// manifest whose provenance this platform cannot check.
func readOwnedManifestBytes(string) ([]byte, error) {
	return nil, errWindowsOwnershipUnverifiable
}

// verifyOwnedDirectory has no ACL-based implementation on windows either, so
// no directory is ever provenanced there and the sweep neither reads from
// nor deletes inside any of them. Same fail-safe direction as above.
func verifyOwnedDirectory(string) error { return errWindowsOwnershipUnverifiable }

// verifyOwnedDirectoryNotWritableByOthers is likewise unimplemented, so
// plugin-cache seeding is skipped on windows rather than done unverified.
func verifyOwnedDirectoryNotWritableByOthers(string) error { return errWindowsOwnershipUnverifiable }

var errWindowsOwnershipUnverifiable = errors.New("directory ownership verification is unimplemented on windows")
