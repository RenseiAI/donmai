// Package strategies provides pluggable landing strategies (rebase, merge,
// squash). Each strategy encapsulates how a proposal's source branch is
// integrated into the target branch via a three-step lifecycle: Prepare (fetch
// + detached checkout), Execute (rebase/merge/squash), Finalize (push).
//
// Ported from donmai-libraries merge-queue/strategies/.
package strategies

import (
	"context"
	"fmt"
	"strings"
)

// Strategy names.
const (
	NameRebase = "rebase"
	NameMerge  = "merge"
	NameSquash = "squash"
)

// Context is passed to every strategy operation.
type Context struct {
	// RepoPath is the path to the (bare) repository.
	RepoPath string
	// WorktreePath is the worktree used for landing operations.
	WorktreePath string
	// SourceBranch is the proposal branch being landed.
	SourceBranch string
	// TargetBranch is the branch being landed into (e.g. "main").
	TargetBranch string
	// Proposal is the proposal number.
	Proposal int
	// Remote is the git remote name (e.g. "origin").
	Remote string
}

// PrepareResult is the outcome of the Prepare step.
type PrepareResult struct {
	// Success reports whether preparation succeeded.
	Success bool
	// Error is the error message when preparation failed.
	Error string
	// HeadSHA is the HEAD SHA after preparation.
	HeadSHA string
	// Retryable is set when the failure is transient (e.g. the branch is held
	// by another worktree) and the worker should requeue with backoff.
	Retryable bool
	// AlreadyMerged is set when the source branch no longer exists on the
	// remote — almost always because a prior successful landing deleted it.
	// The worker treats this as a noop.
	AlreadyMerged bool
}

// MergeResult is the outcome of the Execute step.
type MergeResult struct {
	// Status is one of "success", "conflict", "error".
	Status string
	// MergedSHA is the SHA of the landed commit (when Status == "success").
	MergedSHA string
	// ConflictFiles lists files with conflicts (when Status == "conflict").
	ConflictFiles []string
	// ConflictDetails is a human-readable conflict summary.
	ConflictDetails string
	// Error is the error message (when Status == "error").
	Error string
}

// Merge result statuses.
const (
	StatusSuccess  = "success"
	StatusConflict = "conflict"
	StatusError    = "error"
)

// Strategy is the pluggable landing-strategy interface.
//
// Lifecycle:
//  1. Prepare — fetch latest refs and check out the working branch (detached).
//  2. Execute — perform the rebase/merge/squash.
//  3. Finalize — push the result to the remote.
type Strategy interface {
	// Name is the strategy identifier ("rebase", "merge", or "squash").
	Name() string
	// Prepare fetches latest refs and checks out the working branch.
	Prepare(ctx context.Context, c Context) (PrepareResult, error)
	// Execute performs the landing operation.
	Execute(ctx context.Context, c Context) (MergeResult, error)
	// Finalize pushes the result to the remote.
	Finalize(ctx context.Context, c Context) error
}

// New returns a landing strategy by name.
//
// Ported from createMergeStrategy in
// donmai-libraries merge-queue/strategies/index.ts.
func New(name string) (Strategy, error) {
	switch name {
	case NameRebase:
		return NewRebaseStrategy(), nil
	case NameMerge:
		return NewMergeCommitStrategy(), nil
	case NameSquash:
		return NewSquashStrategy(), nil
	default:
		return nil, fmt.Errorf("unknown landing strategy: %q", name)
	}
}

// collectConflictFiles returns the unmerged (diff-filter=U) files in worktreePath
// using a runner, or nil when the command fails (e.g. no merge/rebase in
// progress). Shared by every strategy's Execute conflict-detection branch.
func collectConflictFiles(ctx context.Context, r commandRunner, worktreePath string) []string {
	out, err := r.run(ctx, worktreePath, nil, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	return splitLines(out)
}

// splitLines splits trimmed stdout into non-empty lines, mirroring the legacy
// `stdout.trim().split('\n').filter(Boolean)`.
func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// errMessage returns the message used by the branch-conflict classifiers,
// matching the legacy `err instanceof Error ? err.message : String(err)`.
func errMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
