package strategies

import "strings"

// Branch-conflict classifiers used by every strategy's Prepare error handling.
//
// These mirror the exported classifiers in the parent landing package
// (branchconflict.go). They are duplicated here as unexported helpers rather
// than imported because the landing package depends on this strategies package
// (the worker selects a strategy), so importing landing here would create an
// import cycle. Both copies port the same pure logic from
// donmai-libraries merge-queue/branch-conflict.ts.

// isBranchConflictError reports whether a git error message indicates the branch
// is already associated with another worktree (both git phrasings).
func isBranchConflictError(errMsg string) bool {
	return strings.Contains(errMsg, "is already checked out at") ||
		strings.Contains(errMsg, "is already used by worktree at")
}

// isMissingRemoteRefError reports whether a git error indicates the requested
// remote ref is missing for the given source branch. In landing context this
// only happens after a prior successful landing deleted the source branch on the
// remote; the caller treats it as already-landed (noop). The expected source
// branch is required so a genuine missing-target-branch error is not swallowed.
func isMissingRemoteRefError(errMsg, sourceBranch string) bool {
	if !strings.Contains(errMsg, "couldn't find remote ref") {
		return false
	}
	return strings.Contains(errMsg, "couldn't find remote ref "+sourceBranch)
}
