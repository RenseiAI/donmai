package landing

import (
	"regexp"
	"strings"
)

// Branch-conflict detection: shared classifiers for the two git error phrasings
// that mean a branch is held by another worktree, plus the missing-remote-ref
// phrasing that (in landing context) means the source branch was already deleted
// by a prior successful landing.
//
// Ported from donmai-libraries merge-queue/branch-conflict.ts. Pure string
// classifiers, no I/O.

// worktreePathPattern extracts the conflicting worktree path from a
// branch-conflict error. Matches both git phrasings.
var worktreePathPattern = regexp.MustCompile(`(?:already checked out at|already used by worktree at)\s+'([^']+)'`)

// IsBranchConflictError reports whether a git error message indicates the branch
// is already associated with another worktree. Matches both "is already checked
// out at" and "is already used by worktree at" phrasings.
func IsBranchConflictError(errMsg string) bool {
	return strings.Contains(errMsg, "is already checked out at") ||
		strings.Contains(errMsg, "is already used by worktree at")
}

// ParseConflictingWorktreePath extracts the conflicting worktree path from a git
// branch-conflict error, or "" when the message does not match.
func ParseConflictingWorktreePath(errMsg string) string {
	m := worktreePathPattern.FindStringSubmatch(errMsg)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// IsMissingRemoteRefError reports whether a git error indicates the requested
// remote ref is missing for the given source branch. In landing context this
// only happens after the source branch was deleted on the remote by a prior
// successful landing; the caller treats it as already-landed (noop) rather than
// a hard failure. The expected source branch is required so a genuine
// missing-target-branch error is not silently swallowed.
func IsMissingRemoteRefError(errMsg, sourceBranch string) bool {
	if !strings.Contains(errMsg, "couldn't find remote ref") {
		return false
	}
	return strings.Contains(errMsg, "couldn't find remote ref "+sourceBranch)
}
