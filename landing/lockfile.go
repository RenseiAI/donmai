package landing

import (
	"context"
	"fmt"
)

// PackageManager identifies the package manager whose lock file may need
// regenerating after a landing.
type PackageManager string

const (
	// PMNone — no package manager; lock-file regeneration is skipped.
	PMNone PackageManager = "none"
	// PMNpm / PMPnpm / PMYarn / PMBun — supported Node package managers.
	PMNpm  PackageManager = "npm"
	PMPnpm PackageManager = "pnpm"
	PMYarn PackageManager = "yarn"
	PMBun  PackageManager = "bun"
)

// RegenerationResult is the outcome of a lock-file regeneration.
type RegenerationResult struct {
	Success        bool
	LockFile       string
	PackageManager PackageManager
	Error          string
}

// LockFileRegeneration deletes the conflicted lock file and regenerates it by
// running the package manager's install, then stages the result. Avoids
// hand-merging machine-generated lock files, which almost never merges cleanly.
//
// Ported from donmai-libraries merge-queue/lock-file-regeneration.ts.
type LockFileRegeneration struct{}

// NewLockFileRegeneration returns a LockFileRegeneration.
func NewLockFileRegeneration() *LockFileRegeneration {
	return &LockFileRegeneration{}
}

// ShouldRegenerate reports whether regeneration applies for the given package
// manager and config flag.
func (LockFileRegeneration) ShouldRegenerate(pm PackageManager, lockFileRegenerate bool) bool {
	return lockFileRegenerate && pm != PMNone
}

// LockFileName returns the lock file name for a package manager, or "" if none.
//
// Stub: not yet ported.
func (LockFileRegeneration) LockFileName(pm PackageManager) string {
	_ = pm
	return ""
}

// Regenerate deletes + regenerates the lock file in worktreePath.
//
// Stub: not yet ported.
func (LockFileRegeneration) Regenerate(ctx context.Context, worktreePath string, pm PackageManager) (RegenerationResult, error) {
	_ = ctx
	_ = worktreePath
	return RegenerationResult{PackageManager: pm}, fmt.Errorf("LockFileRegeneration.Regenerate: %w", ErrNotImplemented)
}

// EnsureGitAttributes ensures the .gitattributes entry that marks the lock file
// as a merge-driver-handled file exists in repoPath.
//
// Stub: not yet ported.
func (LockFileRegeneration) EnsureGitAttributes(ctx context.Context, repoPath string, pm PackageManager) error {
	_ = ctx
	_ = repoPath
	_ = pm
	return fmt.Errorf("LockFileRegeneration.EnsureGitAttributes: %w", ErrNotImplemented)
}
