package landing

import "context"

// State is the state of a proposal in the landing serializer.
//
// Ported from the merge-queue adapter status union in
// donmai-libraries merge-queue/types.ts.
type State string

const (
	// StateQueued — the proposal is waiting in the queue.
	StateQueued State = "queued"
	// StateMerging — the proposal is at the head of the queue and landing.
	StateMerging State = "merging"
	// StateMerged — the proposal has landed.
	StateMerged State = "merged"
	// StateFailed — landing failed (e.g. tests failed).
	StateFailed State = "failed"
	// StateBlocked — landing is blocked (e.g. an unresolved conflict).
	StateBlocked State = "blocked"
	// StateNotQueued — the proposal is not in the queue.
	StateNotQueued State = "not-queued"
)

// CheckStatus is the status of a single required check.
type CheckStatus struct {
	Name string
	// Status is one of "pass", "fail", "pending".
	Status string
}

// Status is the landing-serializer status of a proposal.
type Status struct {
	State State
	// Position is the 1-based queue position, or 0 when not queued.
	Position int
	// FailureReason is set when State is StateFailed or StateBlocked.
	FailureReason string
	// Checks is the status of required checks.
	Checks []CheckStatus
}

// Adapter is the provider-agnostic landing-serializer adapter. Each
// implementation wraps a specific provider's queue API (e.g. a self-hosted Redis
// queue, or an external provider-native merge queue) behind this interface. The
// adapter handles queue management only — conflict resolution is handled at the
// git/VCS level.
//
// Ported from the MergeQueueAdapter interface in
// donmai-libraries merge-queue/types.ts.
type Adapter interface {
	// Name is the provider name identifier (e.g. "local").
	Name() string
	// CanEnqueue reports whether a proposal is eligible for queue entry.
	CanEnqueue(ctx context.Context, owner, repo string, proposal int) (bool, error)
	// Enqueue adds a proposal to the queue and returns its status.
	Enqueue(ctx context.Context, owner, repo string, proposal int) (Status, error)
	// GetStatus returns the current queue status for a proposal.
	GetStatus(ctx context.Context, owner, repo string, proposal int) (Status, error)
	// Dequeue removes a proposal from the queue.
	Dequeue(ctx context.Context, owner, repo string, proposal int) error
	// IsEnabled reports whether the serializer is enabled for a repository.
	IsEnabled(ctx context.Context, owner, repo string) (bool, error)
}
