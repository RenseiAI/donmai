package landing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// EscalationStrategy is how unresolved conflicts are escalated.
type EscalationStrategy string

const (
	// EscalationReassign — send the proposal back to an agent to resolve.
	EscalationReassign EscalationStrategy = "reassign"
	// EscalationNotify — notify a human; leave the proposal as-is.
	EscalationNotify EscalationStrategy = "notify"
	// EscalationPark — park the proposal for auto-retry after other landings.
	EscalationPark EscalationStrategy = "park"
)

// ConflictContext is the input to conflict resolution.
type ConflictContext struct {
	RepoPath        string
	WorktreePath    string
	SourceBranch    string
	TargetBranch    string
	Proposal        int
	IssueID         string
	ConflictFiles   []string
	ConflictDetails string
}

// ResolutionResult is the outcome of conflict resolution.
type ResolutionResult struct {
	// Status is one of "resolved", "escalated", "parked".
	Status string
	// Method is one of "mergiraf", "escalation".
	Method           string
	ResolvedFiles    []string
	UnresolvedFiles  []string
	EscalationAction EscalationStrategy
	Message          string
}

// Resolution result statuses.
const (
	ResolutionResolved  = "resolved"
	ResolutionEscalated = "escalated"
	ResolutionParked    = "parked"
)

// Resolution methods.
const (
	methodMergiraf   = "mergiraf"
	methodEscalation = "escalation"
)

// diffTruncateLimit caps the conflict diff embedded in a reassign escalation
// message, matching the TS source's 5000-char slice.
const diffTruncateLimit = 5000

// ConflictResolverConfig configures a ConflictResolver.
type ConflictResolverConfig struct {
	// MergirafEnabled enables the mergiraf auto-resolution pass.
	MergirafEnabled bool
	// EscalationStrategy is applied to files mergiraf could not resolve.
	EscalationStrategy EscalationStrategy
}

// ConflictResolver attempts automatic conflict resolution via the mergiraf merge
// driver (which runs as a git merge driver during rebase), then escalates
// remaining conflicts using the configured strategy.
//
// Ported from donmai-libraries merge-queue/conflict-resolver.ts.
type ConflictResolver struct {
	cfg    ConflictResolverConfig
	runner commandRunner
}

// NewConflictResolver returns a ConflictResolver backed by the production runner.
func NewConflictResolver(cfg ConflictResolverConfig) *ConflictResolver {
	return &ConflictResolver{cfg: cfg, runner: defaultRunner}
}

func (r *ConflictResolver) run() commandRunner {
	if r.runner == nil {
		return defaultRunner
	}
	return r.runner
}

// Resolve runs the mergiraf pass (if enabled) then escalates unresolved files
// using the configured strategy. When mergiraf resolves everything, the resolved
// result is returned directly; otherwise the unresolved files are forwarded to
// escalation.
func (r *ConflictResolver) Resolve(ctx context.Context, c ConflictContext) (ResolutionResult, error) {
	if r.cfg.MergirafEnabled {
		mergiraf, err := r.attemptMergiraf(ctx, c)
		if err != nil {
			return ResolutionResult{}, fmt.Errorf("ConflictResolver.Resolve mergiraf: %w", err)
		}
		if mergiraf.Status == ResolutionResolved {
			return mergiraf, nil
		}
		// Partial resolution — forward only the remaining files to escalation.
		if mergiraf.UnresolvedFiles != nil {
			c.ConflictFiles = mergiraf.UnresolvedFiles
		}
	}
	return r.escalate(ctx, c), nil
}

// attemptMergiraf inspects each conflict file for remaining markers. Files with
// no markers are staged (mergiraf resolved them). If all files are clean it runs
// `git rebase --continue` (with GIT_EDITOR=true) and reports resolved; otherwise
// it reports escalated with the unresolved set.
func (r *ConflictResolver) attemptMergiraf(ctx context.Context, c ConflictContext) (ResolutionResult, error) {
	resolvedFiles := make([]string, 0, len(c.ConflictFiles))
	unresolvedFiles := make([]string, 0, len(c.ConflictFiles))

	for _, file := range c.ConflictFiles {
		hasConflict := r.fileHasConflictMarkers(ctx, c.WorktreePath, file)
		if !hasConflict {
			if _, err := r.run().run(ctx, c.WorktreePath, nil, "git", "add", file); err != nil {
				return ResolutionResult{}, fmt.Errorf("stage resolved file %q: %w", file, err)
			}
			resolvedFiles = append(resolvedFiles, file)
		} else {
			unresolvedFiles = append(unresolvedFiles, file)
		}
	}

	if len(unresolvedFiles) == 0 {
		// All resolved — continue the rebase.
		if _, err := r.run().run(ctx, c.WorktreePath, []string{"GIT_EDITOR=true"},
			"git", "rebase", "--continue"); err != nil {
			// rebase --continue failed — escalate with the full original set.
			return ResolutionResult{
				Status:          ResolutionEscalated,
				Method:          methodMergiraf,
				ResolvedFiles:   resolvedFiles,
				UnresolvedFiles: c.ConflictFiles,
				Message:         "Mergiraf resolved conflict files but rebase --continue failed",
			}, nil
		}
		return ResolutionResult{
			Status:        ResolutionResolved,
			Method:        methodMergiraf,
			ResolvedFiles: resolvedFiles,
		}, nil
	}

	return ResolutionResult{
		Status:          ResolutionEscalated,
		Method:          methodMergiraf,
		ResolvedFiles:   resolvedFiles,
		UnresolvedFiles: unresolvedFiles,
		Message:         fmt.Sprintf("Mergiraf resolved %d/%d files", len(resolvedFiles), len(c.ConflictFiles)),
	}, nil
}

// fileHasConflictMarkers reports whether a file still contains git conflict
// markers. grep exits non-zero when there are no matches, which is treated as
// "no markers" (resolved). A positive count means markers remain.
func (r *ConflictResolver) fileHasConflictMarkers(ctx context.Context, worktreePath, file string) bool {
	out, err := r.run().run(ctx, worktreePath, nil,
		"grep", "-c", `^<<<<<<<\|^=======\|^>>>>>>>`, file)
	if err != nil {
		// grep returns exit code 1 when no matches — no conflict markers.
		return false
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		return false
	}
	return n > 0
}

// escalate routes to the configured escalation strategy, defaulting to notify
// for an unknown strategy.
func (r *ConflictResolver) escalate(ctx context.Context, c ConflictContext) ResolutionResult {
	switch r.cfg.EscalationStrategy {
	case EscalationReassign:
		return r.escalateReassign(ctx, c)
	case EscalationNotify:
		return r.escalateNotify(c)
	case EscalationPark:
		return r.escalatePark(c)
	default:
		return r.escalateNotify(c)
	}
}

func (r *ConflictResolver) escalateReassign(ctx context.Context, c ConflictContext) ResolutionResult {
	diffOutput := r.conflictDiff(ctx, c)
	return ResolutionResult{
		Status:           ResolutionEscalated,
		Method:           methodEscalation,
		UnresolvedFiles:  c.ConflictFiles,
		EscalationAction: EscalationReassign,
		Message: fmt.Sprintf(
			"Conflict on %s PR #%d. Files: %s. Agent should resolve and re-submit.\n\nDiff:\n%s",
			c.IssueID, c.Proposal, strings.Join(c.ConflictFiles, ", "), diffOutput),
	}
}

func (r *ConflictResolver) escalateNotify(c ConflictContext) ResolutionResult {
	return ResolutionResult{
		Status:           ResolutionEscalated,
		Method:           methodEscalation,
		UnresolvedFiles:  c.ConflictFiles,
		EscalationAction: EscalationNotify,
		Message: fmt.Sprintf(
			"Merge conflict on %s PR #%d requires resolution. Files: %s",
			c.IssueID, c.Proposal, strings.Join(c.ConflictFiles, ", ")),
	}
}

func (r *ConflictResolver) escalatePark(c ConflictContext) ResolutionResult {
	return ResolutionResult{
		Status:           ResolutionParked,
		Method:           methodEscalation,
		UnresolvedFiles:  c.ConflictFiles,
		EscalationAction: EscalationPark,
		Message: fmt.Sprintf(
			"PR #%d parked due to conflicts in: %s. Will auto-retry after other merges complete.",
			c.Proposal, strings.Join(c.ConflictFiles, ", ")),
	}
}

// conflictDiff returns the (truncated) git diff of the conflict files for the
// reassign message, or a placeholder when the diff cannot be produced.
func (r *ConflictResolver) conflictDiff(ctx context.Context, c ConflictContext) string {
	args := append([]string{"diff"}, c.ConflictFiles...)
	out, err := r.run().run(ctx, c.WorktreePath, nil, "git", args...)
	if err != nil {
		return "(unable to generate diff)"
	}
	if len(out) > diffTruncateLimit {
		return out[:diffTruncateLimit]
	}
	return out
}
