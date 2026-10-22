//go:build !darwin && !linux

package sessionshim

import "fmt"

// processStartTime has no portable implementation outside darwin and linux.
//
// Returning an error rather than a plausible-looking fallback is the contract:
// §D3 says a platform without a trustworthy primitive keeps adoption DISABLED.
// A fabricated start time would let PID reuse pass verification, which is worse
// than no adoption at all.
func processStartTime(pid int) (int64, error) {
	return 0, fmt.Errorf("sessionshim: process start identity is unavailable on this platform (pid %d)", pid)
}

// processIdentityExtra has no key to report where adoption is disabled.
func processIdentityExtra(int) (int64, string, error) { return 0, "", nil }

// validateStoredProcessIdentity needs no per-platform validation where
// adoption is disabled: Alive fails closed at the read, before any
// comparison runs.
func validateStoredProcessIdentity(ProcessIdentity) error { return nil }

// processIdentityMatches reports whether two identities name the same process
// incarnation. Only exact equality is meaningful where adoption is disabled.
func processIdentityMatches(a, b ProcessIdentity) (bool, error) {
	return a.StartedAt == b.StartedAt, nil
}
