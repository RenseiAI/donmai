package landing

import (
	"context"
	"fmt"
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

// ConflictResolverConfig configures a ConflictResolver.
type ConflictResolverConfig struct {
	// MergirafEnabled enables the mergiraf auto-resolution pass.
	MergirafEnabled bool
	// EscalationStrategy is applied to files mergiraf could not resolve.
	EscalationStrategy EscalationStrategy
}

// ConflictResolver attempts automatic conflict resolution via the mergiraf merge
// driver, then escalates remaining conflicts using the configured strategy.
//
// Ported from donmai-libraries merge-queue/conflict-resolver.ts.
type ConflictResolver struct {
	cfg ConflictResolverConfig
}

// NewConflictResolver returns a ConflictResolver.
func NewConflictResolver(cfg ConflictResolverConfig) *ConflictResolver {
	return &ConflictResolver{cfg: cfg}
}

// Resolve runs the mergiraf pass (if enabled) then escalates unresolved files.
//
// Stub: not yet ported.
func (r *ConflictResolver) Resolve(ctx context.Context, c ConflictContext) (ResolutionResult, error) {
	_ = ctx
	_ = c
	return ResolutionResult{}, fmt.Errorf("ConflictResolver.Resolve: %w", ErrNotImplemented)
}
