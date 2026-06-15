package landing

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key is the composite tenant-isolation key for every landing-serializer Redis
// structure. FD-4: the legacy TS keyspace was repoId-only, which collides across
// organizations that share (or fork) the same owner/repo. Keying by (orgId,
// repoId) gives one serialized landing stream per repository per organization.
//
// String centralizes prefix construction so the isolation property has a single
// audit point — no call site should hand-build a key.
type Key struct {
	OrgID  string
	RepoID string
}

// String returns the Redis key prefix for this (orgId, repoId), without a
// trailing separator. Concrete keys append ":<suffix>" (see the *Key helpers).
//
//	landing:<orgId>:<repoId>
//
// OrgID is required; an empty OrgID still namespaces away from any orgId-bearing
// key but indicates a caller bug.
func (k Key) String() string {
	return "landing:" + k.OrgID + ":" + k.RepoID
}

// Valid reports whether both components are non-empty. Callers should reject
// invalid keys before touching Redis.
func (k Key) Valid() bool {
	return k.OrgID != "" && k.RepoID != ""
}

// queueKey is the sorted-set key holding queued proposal numbers (score =
// priority + fractional enqueue time).
func (k Key) queueKey() string { return k.String() + ":queue" }

// entryKey is the hash key holding a single proposal's metadata.
func (k Key) entryKey(proposal int) string {
	return fmt.Sprintf("%s:entry:%d", k.String(), proposal)
}

// lockKey is the coordinator single-instance lock key.
func (k Key) lockKey() string { return k.String() + ":lock" }

// pausedKey is the pause-flag key.
func (k Key) pausedKey() string { return k.String() + ":paused" }

// completedKey is the short-lived "recently landed" marker key for a proposal.
func (k Key) completedKey(proposal int) string {
	return fmt.Sprintf("%s:completed:%d", k.String(), proposal)
}

// failedKey is the key holding the failure reason for a proposal.
func (k Key) failedKey(proposal int) string {
	return fmt.Sprintf("%s:failed:%d", k.String(), proposal)
}

// blockedKey is the key holding the blocked reason for a proposal.
func (k Key) blockedKey(proposal int) string {
	return fmt.Sprintf("%s:blocked:%d", k.String(), proposal)
}

// Entry is a queued proposal plus the metadata the worker needs to land it and
// bubble results back to the originating issue.
//
// Ported from the LocalMergeQueueStorage entry shape in
// donmai-libraries merge-queue/adapters/local.ts, with OrgID added (FD-4).
type Entry struct {
	OrgID        string
	RepoID       string
	Proposal     int
	ProposalURL  string
	IssueID      string
	Priority     int
	SourceBranch string
	TargetBranch string
	EnqueuedAt   time.Time
}

// Key returns the (orgId, repoId) key for this entry.
func (e Entry) Key() Key { return Key{OrgID: e.OrgID, RepoID: e.RepoID} }

// Storage is the landing-serializer queue backing store. All operations are
// scoped by Key so tenants never share queue state (FD-4).
//
// Ported from LocalMergeQueueStorage + MergeWorkerDeps.storage in
// donmai-libraries merge-queue/.
type Storage interface {
	// Enqueue adds an entry to the queue. A no-op if the proposal is already
	// queued.
	Enqueue(ctx context.Context, e Entry) error
	// Dequeue atomically removes and returns the highest-priority entry, or nil
	// when the queue is empty.
	Dequeue(ctx context.Context, key Key) (*Entry, error)
	// PeekAll returns all queued entries without removing them (used by Pool to
	// build the conflict graph).
	PeekAll(ctx context.Context, key Key) ([]Entry, error)
	// DequeueBatch atomically removes and returns the given proposals (used by
	// Pool for parallel landing). Proposals not present are skipped.
	DequeueBatch(ctx context.Context, key Key, proposals []int) ([]Entry, error)
	// QueueDepth returns the number of queued proposals.
	QueueDepth(ctx context.Context, key Key) (int, error)
	// IsEnqueued reports whether a proposal is in the queue.
	IsEnqueued(ctx context.Context, key Key, proposal int) (bool, error)
	// Position returns the 1-based queue position of a proposal, or 0 when not
	// queued.
	Position(ctx context.Context, key Key, proposal int) (int, error)
	// Remove deletes a specific proposal from the queue.
	Remove(ctx context.Context, key Key, proposal int) error
	// MarkCompleted records that a proposal landed (clears queue + sets the
	// short-lived recently-landed marker).
	MarkCompleted(ctx context.Context, key Key, proposal int) error
	// MarkFailed records a terminal failure reason for a proposal.
	MarkFailed(ctx context.Context, key Key, proposal int, reason string) error
	// MarkBlocked records a blocked reason for a proposal.
	MarkBlocked(ctx context.Context, key Key, proposal int, reason string) error
	// FailedReason returns the failure reason for a proposal, or "" if none.
	FailedReason(ctx context.Context, key Key, proposal int) (string, error)
	// BlockedReason returns the blocked reason for a proposal, or "" if none.
	BlockedReason(ctx context.Context, key Key, proposal int) (string, error)
}

// RedisStorage is the Redis sorted-set implementation of Storage. The queue is a
// sorted set per (orgId, repoId); per-proposal metadata lives in sibling hashes.
//
// Ports the server-side storage that backed LocalMergeQueueStorage in the TS
// stack, now keyed by (orgId, repoId).
type RedisStorage struct {
	rdb *redis.Client
}

// compile-time assertion that RedisStorage satisfies Storage.
var _ Storage = (*RedisStorage)(nil)

// NewRedisStorage returns a RedisStorage backed by the given client.
func NewRedisStorage(rdb *redis.Client) *RedisStorage {
	return &RedisStorage{rdb: rdb}
}

// Enqueue — stub: not yet ported.
func (s *RedisStorage) Enqueue(ctx context.Context, e Entry) error {
	_ = ctx
	_ = e
	return fmt.Errorf("RedisStorage.Enqueue: %w", ErrNotImplemented)
}

// Dequeue — stub: not yet ported.
func (s *RedisStorage) Dequeue(ctx context.Context, key Key) (*Entry, error) {
	_ = ctx
	_ = key
	return nil, fmt.Errorf("RedisStorage.Dequeue: %w", ErrNotImplemented)
}

// PeekAll — stub: not yet ported.
func (s *RedisStorage) PeekAll(ctx context.Context, key Key) ([]Entry, error) {
	_ = ctx
	_ = key
	return nil, fmt.Errorf("RedisStorage.PeekAll: %w", ErrNotImplemented)
}

// DequeueBatch — stub: not yet ported.
func (s *RedisStorage) DequeueBatch(ctx context.Context, key Key, proposals []int) ([]Entry, error) {
	_ = ctx
	_ = key
	_ = proposals
	return nil, fmt.Errorf("RedisStorage.DequeueBatch: %w", ErrNotImplemented)
}

// QueueDepth — stub: not yet ported.
func (s *RedisStorage) QueueDepth(ctx context.Context, key Key) (int, error) {
	_ = ctx
	_ = key
	return 0, fmt.Errorf("RedisStorage.QueueDepth: %w", ErrNotImplemented)
}

// IsEnqueued — stub: not yet ported.
func (s *RedisStorage) IsEnqueued(ctx context.Context, key Key, proposal int) (bool, error) {
	_ = ctx
	_ = key
	_ = proposal
	return false, fmt.Errorf("RedisStorage.IsEnqueued: %w", ErrNotImplemented)
}

// Position — stub: not yet ported.
func (s *RedisStorage) Position(ctx context.Context, key Key, proposal int) (int, error) {
	_ = ctx
	_ = key
	_ = proposal
	return 0, fmt.Errorf("RedisStorage.Position: %w", ErrNotImplemented)
}

// Remove — stub: not yet ported.
func (s *RedisStorage) Remove(ctx context.Context, key Key, proposal int) error {
	_ = ctx
	_ = key
	_ = proposal
	return fmt.Errorf("RedisStorage.Remove: %w", ErrNotImplemented)
}

// MarkCompleted — stub: not yet ported.
func (s *RedisStorage) MarkCompleted(ctx context.Context, key Key, proposal int) error {
	_ = ctx
	_ = key
	_ = proposal
	return fmt.Errorf("RedisStorage.MarkCompleted: %w", ErrNotImplemented)
}

// MarkFailed — stub: not yet ported.
func (s *RedisStorage) MarkFailed(ctx context.Context, key Key, proposal int, reason string) error {
	_ = ctx
	_ = key
	_ = proposal
	_ = reason
	return fmt.Errorf("RedisStorage.MarkFailed: %w", ErrNotImplemented)
}

// MarkBlocked — stub: not yet ported.
func (s *RedisStorage) MarkBlocked(ctx context.Context, key Key, proposal int, reason string) error {
	_ = ctx
	_ = key
	_ = proposal
	_ = reason
	return fmt.Errorf("RedisStorage.MarkBlocked: %w", ErrNotImplemented)
}

// FailedReason — stub: not yet ported.
func (s *RedisStorage) FailedReason(ctx context.Context, key Key, proposal int) (string, error) {
	_ = ctx
	_ = key
	_ = proposal
	return "", fmt.Errorf("RedisStorage.FailedReason: %w", ErrNotImplemented)
}

// BlockedReason — stub: not yet ported.
func (s *RedisStorage) BlockedReason(ctx context.Context, key Key, proposal int) (string, error) {
	_ = ctx
	_ = key
	_ = proposal
	return "", fmt.Errorf("RedisStorage.BlockedReason: %w", ErrNotImplemented)
}

// issueIDPattern matches a Linear-style identifier (ALPHA-DIGITS) anchored to a
// word boundary so version-like strings (v1.2.3-rc4) do not match.
var issueIDPattern = regexp.MustCompile(`\b([A-Za-z]{2,10})-(\d+)\b`)

// ExtractIssueID pulls an issue identifier (e.g. "ABC-1153") out of a branch
// name or proposal title, normalizing the prefix to upper case. Returns "" if
// there is no match.
//
// Matches:
//   - "ABC-1153"                          (bare branch)
//   - "ABC-1153: short title"             (title prefix)
//   - "feature/ABC-1153-cedar-stuff"      (branch with prefix)
//   - "abc-1153"                          (lower case, normalized to upper)
//
// Ported from extractIssueIdentifier in
// donmai-libraries merge-queue/adapters/local.ts.
func ExtractIssueID(input string) string {
	if input == "" {
		return ""
	}
	m := issueIDPattern.FindStringSubmatch(input)
	if len(m) < 3 {
		return ""
	}
	return strings.ToUpper(m[1]) + "-" + m[2]
}
