package landing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
// priority + fractional enqueue sequence).
func (k Key) queueKey() string { return k.String() + ":queue" }

// seqKey is the monotonic enqueue-sequence counter. Each Enqueue does an INCR on
// it to obtain a strictly increasing tiebreaker, so two proposals enqueued at the
// same priority within the same nanosecond still order FIFO — a guarantee the
// previous wall-clock-fraction score could not make once a float64 mantissa ran
// out of precision for sub-second timestamps.
func (k Key) seqKey() string { return k.String() + ":seq" }

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
	OrgID       string
	RepoID      string
	Proposal    int
	ProposalURL string
	IssueID     string
	// LinearSessionID identifies the agent session that produced this proposal.
	// Carried so a ResultPoster can promote+close that session by id after the
	// proposal lands, without the coordinator re-resolving it from IssueID.
	// Optional; empty when the producer does not supply it.
	LinearSessionID string
	Priority        int
	SourceBranch    string
	TargetBranch    string
	EnqueuedAt      time.Time
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

// DefaultReasonTTL is the lifetime of a failed / blocked reason marker. The
// reason is a transient signal consumed by `Status` lookups and the bubble-up;
// it does not need to live forever and an unbounded key set is a leak.
const DefaultReasonTTL = 24 * time.Hour

// encodeScore packs (priority, seq) into a single sorted-set score so the queue
// orders by priority ascending (lower priority value = higher precedence),
// breaking ties by enqueue order (FIFO). The tiebreaker is a strictly increasing
// per-key sequence number (a Redis INCR, not a timestamp), mapped through
// seq/(1+seq) into [0,1) so it never bleeds into the next integer priority band:
// two entries with the same priority order by enqueue sequence, but an entry with
// a lower priority always sorts ahead of a higher one regardless of sequence.
//
// Using a monotonic integer sequence instead of a wall-clock fraction fixes the
// sub-second FIFO defect: float64 cannot resolve fine-grained nanosecond
// timestamps within a priority band, so several proposals enqueued in the same
// second could collide on one score and lose FIFO ordering. A small, strictly
// increasing seq (0, 1, 2, …) is exactly representable and is strictly monotonic
// under seq/(1+seq) for every distinct seq, so same-priority entries always
// dequeue in enqueue order.
//
// This mirrors the legacy "lower = higher priority; ties by enqueue order"
// intent of the server-side queue while keeping a single Redis ZSET score.
func encodeScore(priority int, seq int64) float64 {
	if seq < 0 {
		seq = 0
	}
	frac := float64(seq) / (1 + float64(seq)) // in [0,1); strictly increasing in seq
	// Guard the float64 edge: once seq is large enough that 1+seq rounds back to
	// seq, frac would evaluate to exactly 1.0 and bleed into the next priority
	// band. Clamp strictly below 1 so the priority integer part is always
	// authoritative. The clamp ceiling is far above any realistic queue depth, so
	// it never affects FIFO ordering in practice.
	if frac >= maxScoreFrac {
		frac = maxScoreFrac
	}
	// Earlier (smaller seq) → smaller frac → smaller score → dequeued first. FIFO.
	return float64(priority) + frac
}

// maxScoreFrac is the largest fractional tiebreaker, kept strictly below 1 so an
// entry can never sort into the next integer priority band regardless of seq.
const maxScoreFrac = 0.9999999999

// RedisStorage is the Redis sorted-set implementation of Storage. The queue is a
// sorted set per (orgId, repoId); per-proposal metadata lives in sibling hashes.
//
// Ports the server-side storage that backed LocalMergeQueueStorage in the TS
// stack, now keyed by (orgId, repoId).
type RedisStorage struct {
	rdb *redis.Client
	// reasonTTL bounds failed/blocked reason markers; 0 ⇒ DefaultReasonTTL.
	reasonTTL time.Duration
}

// compile-time assertion that RedisStorage satisfies Storage.
var _ Storage = (*RedisStorage)(nil)

// NewRedisStorage returns a RedisStorage backed by the given client.
func NewRedisStorage(rdb *redis.Client) *RedisStorage {
	return &RedisStorage{rdb: rdb}
}

func (s *RedisStorage) reasonExpiry() time.Duration {
	if s.reasonTTL > 0 {
		return s.reasonTTL
	}
	return DefaultReasonTTL
}

// entryFields serializes an Entry into a Redis hash field map. Proposal/OrgID/
// RepoID are derivable from the key + member, so only the bubble-up metadata is
// stored.
func entryFields(e Entry) map[string]any {
	return map[string]any{
		"proposalUrl":     e.ProposalURL,
		"issueId":         e.IssueID,
		"linearSessionId": e.LinearSessionID,
		"priority":        e.Priority,
		"sourceBranch":    e.SourceBranch,
		"targetBranch":    e.TargetBranch,
		"enqueuedAt":      e.EnqueuedAt.UnixNano(),
	}
}

// entryFromHash rebuilds an Entry from its key, proposal number, and stored hash.
func entryFromHash(key Key, proposal int, h map[string]string) Entry {
	priority, _ := strconv.Atoi(h["priority"])
	var enqueuedAt time.Time
	if ns, err := strconv.ParseInt(h["enqueuedAt"], 10, 64); err == nil {
		enqueuedAt = time.Unix(0, ns).UTC()
	}
	return Entry{
		OrgID:           key.OrgID,
		RepoID:          key.RepoID,
		Proposal:        proposal,
		ProposalURL:     h["proposalUrl"],
		IssueID:         h["issueId"],
		LinearSessionID: h["linearSessionId"],
		Priority:        priority,
		SourceBranch:    h["sourceBranch"],
		TargetBranch:    h["targetBranch"],
		EnqueuedAt:      enqueuedAt,
	}
}

// validateKey rejects an unscoped key before any Redis op so a caller bug never
// silently shares queue state across tenants (FD-4).
func validateKey(key Key) error {
	if !key.Valid() {
		return fmt.Errorf("landing: invalid key (orgId=%q repoId=%q): both required", key.OrgID, key.RepoID)
	}
	return nil
}

// Enqueue adds an entry to the queue. A no-op if the proposal is already queued.
// The sorted-set member is the proposal number; metadata lives in a sibling hash.
func (s *RedisStorage) Enqueue(ctx context.Context, e Entry) error {
	key := e.Key()
	if err := validateKey(key); err != nil {
		return err
	}
	if e.EnqueuedAt.IsZero() {
		e.EnqueuedAt = time.Now().UTC()
	}
	member := strconv.Itoa(e.Proposal)

	// A strictly increasing per-key sequence is the FIFO tiebreaker. INCR is
	// atomic, so concurrent enqueues at the same priority still get distinct,
	// strictly ordered sequence numbers — no sub-second float collision.
	seq, err := s.rdb.Incr(ctx, key.seqKey()).Result()
	if err != nil {
		return fmt.Errorf("RedisStorage.Enqueue incr seq %s: %w", key.seqKey(), err)
	}

	// Skip if already queued (NX on the ZADD), then write metadata. ZADD NX
	// returns the number of NEW members added (0 when it already existed).
	added, err := s.rdb.ZAddNX(ctx, key.queueKey(), redis.Z{
		Score:  encodeScore(e.Priority, seq),
		Member: member,
	}).Result()
	if err != nil {
		return fmt.Errorf("RedisStorage.Enqueue zadd %s: %w", key.queueKey(), err)
	}
	if added == 0 {
		// Already queued — leave the existing entry untouched. The INCR above
		// consumed a sequence number for a no-op enqueue; that is harmless (the
		// counter only needs to be strictly increasing, not gap-free).
		return nil
	}
	if err := s.rdb.HSet(ctx, key.entryKey(e.Proposal), entryFields(e)).Err(); err != nil {
		return fmt.Errorf("RedisStorage.Enqueue hset %s: %w", key.entryKey(e.Proposal), err)
	}
	return nil
}

// Dequeue atomically removes and returns the highest-priority entry, or nil when
// the queue is empty. ZPOPMIN pops the lowest score, which encodeScore maps to
// the highest-precedence proposal.
func (s *RedisStorage) Dequeue(ctx context.Context, key Key) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	popped, err := s.rdb.ZPopMin(ctx, key.queueKey(), 1).Result()
	if err != nil {
		return nil, fmt.Errorf("RedisStorage.Dequeue zpopmin %s: %w", key.queueKey(), err)
	}
	if len(popped) == 0 {
		return nil, nil
	}
	proposal, convErr := strconv.Atoi(memberString(popped[0].Member))
	if convErr != nil {
		return nil, fmt.Errorf("RedisStorage.Dequeue parse member %v: %w", popped[0].Member, convErr)
	}
	entry, err := s.loadEntry(ctx, key, proposal)
	if err != nil {
		return nil, err
	}
	// Clear the metadata hash now that the entry left the queue.
	if err := s.rdb.Del(ctx, key.entryKey(proposal)).Err(); err != nil {
		return nil, fmt.Errorf("RedisStorage.Dequeue del entry %s: %w", key.entryKey(proposal), err)
	}
	return &entry, nil
}

// PeekAll returns all queued entries in dequeue order without removing them
// (used by Pool to build the conflict graph).
func (s *RedisStorage) PeekAll(ctx context.Context, key Key) ([]Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	members, err := s.rdb.ZRange(ctx, key.queueKey(), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("RedisStorage.PeekAll zrange %s: %w", key.queueKey(), err)
	}
	entries := make([]Entry, 0, len(members))
	for _, m := range members {
		proposal, convErr := strconv.Atoi(m)
		if convErr != nil {
			return nil, fmt.Errorf("RedisStorage.PeekAll parse member %q: %w", m, convErr)
		}
		entry, loadErr := s.loadEntry(ctx, key, proposal)
		if loadErr != nil {
			return nil, loadErr
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// DequeueBatch atomically removes and returns the given proposals (used by Pool
// for parallel landing). Proposals not present are skipped. The ZREM + HGETALL
// reads run in a MULTI/EXEC transaction so the Pool never double-dispatches a
// proposal across concurrent coordinators: only the coordinator whose ZREM
// reports the member as removed gets the entry.
func (s *RedisStorage) DequeueBatch(ctx context.Context, key Key, proposals []int) ([]Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if len(proposals) == 0 {
		return nil, nil
	}

	// Load metadata first (outside the transaction) so we can return full
	// entries; the transaction then claims membership atomically via per-member
	// ZREM whose integer reply tells us who actually owned the member.
	loaded := make(map[int]Entry, len(proposals))
	for _, p := range proposals {
		entry, err := s.loadEntry(ctx, key, p)
		if err != nil {
			return nil, err
		}
		loaded[p] = entry
	}

	remCmds := make(map[int]*redis.IntCmd, len(proposals))
	pipe := s.rdb.TxPipeline()
	for _, p := range proposals {
		remCmds[p] = pipe.ZRem(ctx, key.queueKey(), strconv.Itoa(p))
		pipe.Del(ctx, key.entryKey(p))
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("RedisStorage.DequeueBatch exec: %w", err)
	}

	out := make([]Entry, 0, len(proposals))
	for _, p := range proposals {
		removed, err := remCmds[p].Result()
		if err != nil {
			return nil, fmt.Errorf("RedisStorage.DequeueBatch zrem %d: %w", p, err)
		}
		if removed == 1 {
			out = append(out, loaded[p])
		}
	}
	return out, nil
}

// QueueDepth returns the number of queued proposals.
func (s *RedisStorage) QueueDepth(ctx context.Context, key Key) (int, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	n, err := s.rdb.ZCard(ctx, key.queueKey()).Result()
	if err != nil {
		return 0, fmt.Errorf("RedisStorage.QueueDepth zcard %s: %w", key.queueKey(), err)
	}
	return int(n), nil
}

// IsEnqueued reports whether a proposal is in the queue.
func (s *RedisStorage) IsEnqueued(ctx context.Context, key Key, proposal int) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	_, err := s.rdb.ZScore(ctx, key.queueKey(), strconv.Itoa(proposal)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("RedisStorage.IsEnqueued zscore %s: %w", key.queueKey(), err)
	}
	return true, nil
}

// Position returns the 1-based queue position of a proposal, or 0 when not
// queued.
func (s *RedisStorage) Position(ctx context.Context, key Key, proposal int) (int, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	rank, err := s.rdb.ZRank(ctx, key.queueKey(), strconv.Itoa(proposal)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("RedisStorage.Position zrank %s: %w", key.queueKey(), err)
	}
	return int(rank) + 1, nil
}

// Remove deletes a specific proposal from the queue and its metadata hash.
func (s *RedisStorage) Remove(ctx context.Context, key Key, proposal int) error {
	if err := validateKey(key); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.ZRem(ctx, key.queueKey(), strconv.Itoa(proposal))
	pipe.Del(ctx, key.entryKey(proposal))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("RedisStorage.Remove exec: %w", err)
	}
	return nil
}

// MarkCompleted records that a proposal landed: clears it from the queue and
// metadata. The short-lived recently-landed marker is written separately by the
// worker (see markRecentlyMerged) which owns its TTL.
func (s *RedisStorage) MarkCompleted(ctx context.Context, key Key, proposal int) error {
	if err := validateKey(key); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.ZRem(ctx, key.queueKey(), strconv.Itoa(proposal))
	pipe.Del(ctx, key.entryKey(proposal))
	// Completing a proposal clears any stale failed/blocked reason.
	pipe.Del(ctx, key.failedKey(proposal))
	pipe.Del(ctx, key.blockedKey(proposal))
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("RedisStorage.MarkCompleted exec: %w", err)
	}
	return nil
}

// MarkFailed records a terminal failure reason for a proposal and clears it from
// the queue.
func (s *RedisStorage) MarkFailed(ctx context.Context, key Key, proposal int, reason string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.ZRem(ctx, key.queueKey(), strconv.Itoa(proposal))
	pipe.Del(ctx, key.entryKey(proposal))
	pipe.Set(ctx, key.failedKey(proposal), reason, s.reasonExpiry())
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("RedisStorage.MarkFailed exec: %w", err)
	}
	return nil
}

// MarkBlocked records a blocked reason for a proposal and clears it from the
// queue.
func (s *RedisStorage) MarkBlocked(ctx context.Context, key Key, proposal int, reason string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.ZRem(ctx, key.queueKey(), strconv.Itoa(proposal))
	pipe.Del(ctx, key.entryKey(proposal))
	pipe.Set(ctx, key.blockedKey(proposal), reason, s.reasonExpiry())
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("RedisStorage.MarkBlocked exec: %w", err)
	}
	return nil
}

// FailedReason returns the failure reason for a proposal, or "" if none.
func (s *RedisStorage) FailedReason(ctx context.Context, key Key, proposal int) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	reason, err := s.rdb.Get(ctx, key.failedKey(proposal)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("RedisStorage.FailedReason get %s: %w", key.failedKey(proposal), err)
	}
	return reason, nil
}

// BlockedReason returns the blocked reason for a proposal, or "" if none.
func (s *RedisStorage) BlockedReason(ctx context.Context, key Key, proposal int) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	reason, err := s.rdb.Get(ctx, key.blockedKey(proposal)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("RedisStorage.BlockedReason get %s: %w", key.blockedKey(proposal), err)
	}
	return reason, nil
}

// loadEntry rebuilds an Entry from its metadata hash. A missing hash (e.g. a
// member that lost its sibling) yields an entry with only the derivable fields.
func (s *RedisStorage) loadEntry(ctx context.Context, key Key, proposal int) (Entry, error) {
	h, err := s.rdb.HGetAll(ctx, key.entryKey(proposal)).Result()
	if err != nil {
		return Entry{}, fmt.Errorf("RedisStorage.loadEntry hgetall %s: %w", key.entryKey(proposal), err)
	}
	return entryFromHash(key, proposal, h), nil
}

// memberString coerces a ZSet member (string in practice) to its string form.
func memberString(member any) string {
	switch v := member.(type) {
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
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
