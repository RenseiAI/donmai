// Package queue provides Redis-backed admin clients for the AgentFactory work
// queue and merge queue. It mirrors the data structures used by the TypeScript
// packages/cli/src/lib/queue-admin-runner.ts and merge-queue-runner.ts so the
// Go CLI can inspect and manipulate the same Redis state.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ──────────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ──────────────────────────────────────────────────────────────────────────────

// ErrRedisURLRequired is returned when REDIS_URL is not set.
var ErrRedisURLRequired = errors.New("REDIS_URL environment variable is required")

// ErrItemNotFound is returned when a queue item cannot be found by the given selector.
var ErrItemNotFound = errors.New("item not found in queue")

// ErrMergeEntryNotFound is returned when a merge queue entry cannot be located.
var ErrMergeEntryNotFound = errors.New("merge queue entry not found")

// ──────────────────────────────────────────────────────────────────────────────
// Work-queue Redis key constants (mirrors TS queue-admin-runner.ts)
// ──────────────────────────────────────────────────────────────────────────────

const (
	workQueueKey     = "work:queue"
	workItemsKey     = "work:items"
	workClaimPrefix  = "work:claim:"
	sessionKeyPrefix = "agent:session:"
	workerPrefix     = "work:worker:"
)

// ──────────────────────────────────────────────────────────────────────────────
// Types — work queue
// ──────────────────────────────────────────────────────────────────────────────

// WorkItem represents a single entry in the work queue hash.
type WorkItem struct {
	SessionID       string  `json:"sessionId"`
	IssueIdentifier string  `json:"issueIdentifier,omitempty"`
	WorkType        string  `json:"workType,omitempty"`
	Priority        float64 `json:"priority,omitempty"`
	QueuedAt        int64   `json:"queuedAt,omitempty"`
	ProviderSession string  `json:"providerSessionId,omitempty"`
	Prompt          string  `json:"prompt,omitempty"`
}

// SessionEntry represents one agent session stored in Redis.
type SessionEntry struct {
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	IssueIdentifier string `json:"issueIdentifier,omitempty"`
	IssueID         string `json:"issueId,omitempty"`
	LinearSessionID string `json:"linearSessionId,omitempty"`
	WorkerID        string `json:"workerId,omitempty"`
	WorkType        string `json:"workType,omitempty"`
	UpdatedAt       int64  `json:"updatedAt,omitempty"`
}

// WorkerEntry represents a registered worker.
type WorkerEntry struct {
	ID            string `json:"id"`
	Hostname      string `json:"hostname,omitempty"`
	Status        string `json:"status"`
	Capacity      int    `json:"capacity,omitempty"`
	ActiveCount   int    `json:"activeCount,omitempty"`
	LastHeartbeat int64  `json:"lastHeartbeat,omitempty"`
}

// Snapshot is the JSON output for `af admin queue list`.
type Snapshot struct {
	Items    []WorkItem     `json:"items"`
	Sessions []SessionEntry `json:"sessions"`
	Workers  []WorkerEntry  `json:"workers"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Types — merge queue (mirrors TS merge-queue-runner.ts / MergeQueueEntry)
// ──────────────────────────────────────────────────────────────────────────────

// MergeEntry represents one PR in the merge queue.
type MergeEntry struct {
	RepoID        string  `json:"repoId"`
	PRNumber      int     `json:"prNumber"`
	SourceBranch  string  `json:"sourceBranch"`
	Priority      float64 `json:"priority"`
	EnqueuedAt    int64   `json:"enqueuedAt"`
	Status        string  `json:"status"` // queued|processing|failed|blocked
	FailureReason string  `json:"failureReason,omitempty"`
	BlockReason   string  `json:"blockReason,omitempty"`
}

// MergeQueueSnapshot is the JSON output for `af admin merge-queue list`.
type MergeQueueSnapshot struct {
	RepoID  string       `json:"repoId"`
	Depth   int          `json:"depth"`
	Entries []MergeEntry `json:"entries"`
}

// ──────────────────────────────────────────────────────────────────────────────
// AdminClient
// ──────────────────────────────────────────────────────────────────────────────

// AdminClient wraps a Redis connection and provides admin operations for both
// the work queue and the merge queue.
type AdminClient struct {
	rdb *redis.Client
}

// NewAdminClient parses redisURL and returns a connected AdminClient.
func NewAdminClient(redisURL string) (*AdminClient, error) {
	if redisURL == "" {
		return nil, ErrRedisURLRequired
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	return &AdminClient{rdb: redis.NewClient(opts)}, nil
}

// Close releases the underlying connection pool.
func (c *AdminClient) Close() error {
	return c.rdb.Close()
}

// Ping checks connectivity.
func (c *AdminClient) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Work-queue reads
// ──────────────────────────────────────────────────────────────────────────────

// ListWorkItems returns all items currently in the work queue hash.
func (c *AdminClient) ListWorkItems(ctx context.Context) ([]WorkItem, error) {
	raw, err := c.rdb.HGetAll(ctx, workItemsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("hgetall %s: %w", workItemsKey, err)
	}

	items := make([]WorkItem, 0, len(raw))
	for sessionID, jsonStr := range raw {
		var item WorkItem
		if err := json.Unmarshal([]byte(jsonStr), &item); err != nil {
			// Return partial data for un-parseable items (same as TS runner)
			item = WorkItem{SessionID: sessionID, IssueIdentifier: "[invalid JSON]"}
		} else {
			item.SessionID = sessionID
		}
		items = append(items, item)
	}
	return items, nil
}

// PeekWorkItem returns the first entry in the work queue without removing it.
func (c *AdminClient) PeekWorkItem(ctx context.Context) (*WorkItem, error) {
	// The work queue is a sorted set (ZSet); fetch the lowest-score member.
	result, err := c.rdb.ZRangeByScoreWithScores(ctx, workQueueKey, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    "+inf",
		Offset: 0,
		Count:  1,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("zrangebyscore %s: %w", workQueueKey, err)
	}
	if len(result) == 0 {
		return nil, ErrItemNotFound
	}
	sessionID, _ := result[0].Member.(string)
	return c.workItemBySession(ctx, sessionID)
}

func (c *AdminClient) workItemBySession(ctx context.Context, sessionID string) (*WorkItem, error) {
	jsonStr, err := c.rdb.HGet(ctx, workItemsKey, sessionID).Result()
	if errors.Is(err, redis.Nil) {
		return &WorkItem{SessionID: sessionID}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hget %s %s: %w", workItemsKey, sessionID, err)
	}
	var item WorkItem
	if jsonErr := json.Unmarshal([]byte(jsonStr), &item); jsonErr != nil {
		item = WorkItem{SessionID: sessionID, IssueIdentifier: "[invalid JSON]"}
	} else {
		item.SessionID = sessionID
	}
	return &item, nil
}

// ListSessions returns all agent sessions.
func (c *AdminClient) ListSessions(ctx context.Context) ([]SessionEntry, error) {
	keys, err := c.rdb.Keys(ctx, sessionKeyPrefix+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("keys %s*: %w", sessionKeyPrefix, err)
	}

	sessions := make([]SessionEntry, 0, len(keys))
	for _, key := range keys {
		jsonStr, err := c.rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &entry); err != nil {
			continue
		}
		sess := sessionFromMap(entry)
		sess.SessionID = strings.TrimPrefix(key, sessionKeyPrefix)
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

func sessionFromMap(m map[string]any) SessionEntry {
	s := SessionEntry{}
	if v, ok := m["status"].(string); ok {
		s.Status = v
	}
	if v, ok := m["issueIdentifier"].(string); ok {
		s.IssueIdentifier = v
	}
	if v, ok := m["issueId"].(string); ok {
		s.IssueID = v
	}
	if v, ok := m["linearSessionId"].(string); ok {
		s.LinearSessionID = v
	}
	if v, ok := m["workerId"].(string); ok {
		s.WorkerID = v
	}
	if v, ok := m["workType"].(string); ok {
		s.WorkType = v
	}
	switch v := m["updatedAt"].(type) {
	case float64:
		s.UpdatedAt = int64(v)
	case int64:
		s.UpdatedAt = v
	}
	return s
}

// ListWorkers returns all registered workers.
func (c *AdminClient) ListWorkers(ctx context.Context) ([]WorkerEntry, error) {
	keys, err := c.rdb.Keys(ctx, workerPrefix+"*").Result()
	if err != nil {
		return nil, fmt.Errorf("keys %s*: %w", workerPrefix, err)
	}

	workers := make([]WorkerEntry, 0, len(keys))
	for _, key := range keys {
		jsonStr, err := c.rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &entry); err != nil {
			continue
		}
		workers = append(workers, workerFromMap(entry))
	}
	return workers, nil
}

func workerFromMap(m map[string]any) WorkerEntry {
	w := WorkerEntry{}
	if v, ok := m["id"].(string); ok {
		w.ID = v
	}
	if v, ok := m["hostname"].(string); ok {
		w.Hostname = v
	}
	if v, ok := m["status"].(string); ok {
		w.Status = v
	}
	if v, ok := m["capacity"].(float64); ok {
		w.Capacity = int(v)
	}
	if v, ok := m["activeCount"].(float64); ok {
		w.ActiveCount = int(v)
	}
	if v, ok := m["lastHeartbeat"].(float64); ok {
		w.LastHeartbeat = int64(v)
	}
	return w
}

// ──────────────────────────────────────────────────────────────────────────────
// Work-queue destructive ops
// ──────────────────────────────────────────────────────────────────────────────

// DropSession removes a session (and its queue/claim entries) by partial ID match.
// Returns the number of entries removed.
func (c *AdminClient) DropSession(ctx context.Context, partialID string) (int, error) {
	removed := 0

	// Sessions
	sessionKeys, err := c.rdb.Keys(ctx, sessionKeyPrefix+"*").Result()
	if err != nil {
		return 0, fmt.Errorf("keys sessions: %w", err)
	}
	for _, key := range sessionKeys {
		if strings.Contains(key, partialID) {
			if err := c.rdb.Del(ctx, key).Err(); err != nil {
				return removed, fmt.Errorf("del session %s: %w", key, err)
			}
			removed++
		}
	}

	// Work items hash + sorted set
	allItems, err := c.rdb.HGetAll(ctx, workItemsKey).Result()
	if err == nil {
		for sessionID := range allItems {
			if strings.Contains(sessionID, partialID) {
				_ = c.rdb.HDel(ctx, workItemsKey, sessionID)
				_ = c.rdb.ZRem(ctx, workQueueKey, sessionID)
				removed++
			}
		}
	}

	// Claims
	claimKeys, err := c.rdb.Keys(ctx, workClaimPrefix+"*").Result()
	if err == nil {
		for _, key := range claimKeys {
			if strings.Contains(key, partialID) {
				_ = c.rdb.Del(ctx, key)
				removed++
			}
		}
	}

	if removed == 0 {
		return 0, ErrItemNotFound
	}
	return removed, nil
}

// RequeueSession resets a "running" or "claimed" session back to "pending".
func (c *AdminClient) RequeueSession(ctx context.Context, partialID string) (int, error) {
	requeued := 0
	keys, err := c.rdb.Keys(ctx, sessionKeyPrefix+"*").Result()
	if err != nil {
		return 0, fmt.Errorf("keys sessions: %w", err)
	}
	for _, key := range keys {
		if !strings.Contains(key, partialID) {
			continue
		}
		jsonStr, err := c.rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return requeued, fmt.Errorf("get %s: %w", key, err)
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &entry); err != nil {
			continue
		}
		entry["status"] = "pending"
		delete(entry, "workerId")
		delete(entry, "claimedAt")
		delete(entry, "providerSessionId")
		entry["updatedAt"] = time.Now().Unix()

		updated, err := json.Marshal(entry)
		if err != nil {
			return requeued, fmt.Errorf("marshal updated session: %w", err)
		}
		if err := c.rdb.Set(ctx, key, updated, 24*time.Hour).Err(); err != nil {
			return requeued, fmt.Errorf("set %s: %w", key, err)
		}
		requeued++
	}
	if requeued == 0 {
		return 0, ErrItemNotFound
	}
	return requeued, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Merge-queue Redis key constants (mirrors TS MergeQueueStorage)
// ──────────────────────────────────────────────────────────────────────────────

const (
	mergeQueuePrefix   = "merge:queue:"   // ZSET per repo
	mergeEntryPrefix   = "merge:entry:"   // hash per repo
	mergeFailedPrefix  = "merge:failed:"  // ZSET per repo
	mergeBlockedPrefix = "merge:blocked:" // ZSET per repo
)

// ──────────────────────────────────────────────────────────────────────────────
// Merge-queue reads
// ──────────────────────────────────────────────────────────────────────────────

// ListMergeQueue returns all queued, failed, and blocked merge entries for repoID.
func (c *AdminClient) ListMergeQueue(ctx context.Context, repoID string) (MergeQueueSnapshot, error) {
	snapshot := MergeQueueSnapshot{RepoID: repoID}

	queued, err := c.mergeFetchAll(ctx, mergeQueuePrefix+repoID, "queued")
	if err != nil {
		return snapshot, err
	}
	failed, err := c.mergeFetchAll(ctx, mergeFailedPrefix+repoID, "failed")
	if err != nil {
		return snapshot, err
	}
	blocked, err := c.mergeFetchAll(ctx, mergeBlockedPrefix+repoID, "blocked")
	if err != nil {
		return snapshot, err
	}

	queuedDepth := len(queued)
	failed = append(failed, blocked...)
	queued = append(queued, failed...)
	snapshot.Entries = queued
	snapshot.Depth = queuedDepth
	return snapshot, nil
}

func (c *AdminClient) mergeFetchAll(ctx context.Context, zsetKey, status string) ([]MergeEntry, error) {
	members, err := c.rdb.ZRangeByScoreWithScores(ctx, zsetKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: "+inf",
	}).Result()
	if errors.Is(err, redis.Nil) || len(members) == 0 {
		return nil, nil //nolint:nilerr // empty is ok
	}
	if err != nil {
		return nil, fmt.Errorf("zrangebyscore %s: %w", zsetKey, err)
	}

	repoID := strings.TrimPrefix(zsetKey, mergeQueuePrefix)
	repoID = strings.TrimPrefix(repoID, mergeFailedPrefix)
	repoID = strings.TrimPrefix(repoID, mergeBlockedPrefix)

	entries := make([]MergeEntry, 0, len(members))
	for _, m := range members {
		prNum, _ := m.Member.(string)
		raw, err := c.rdb.HGet(ctx, mergeEntryPrefix+repoID, prNum).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			continue
		}
		var entry MergeEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			continue
		}
		entry.Status = status
		entries = append(entries, entry)
	}
	return entries, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Merge-queue destructive ops
// ──────────────────────────────────────────────────────────────────────────────

// DequeueEntry removes a PR from the merge queue entirely (skip/dequeue).
func (c *AdminClient) DequeueEntry(ctx context.Context, repoID string, prNumber int) error {
	prStr := fmt.Sprintf("%d", prNumber)
	removed := int64(0)
	for _, key := range []string{
		mergeQueuePrefix + repoID,
		mergeFailedPrefix + repoID,
		mergeBlockedPrefix + repoID,
	} {
		n, err := c.rdb.ZRem(ctx, key, prStr).Result()
		if err == nil {
			removed += n
		}
	}
	if removed == 0 {
		return fmt.Errorf("%w: PR #%d in repo %q", ErrMergeEntryNotFound, prNumber, repoID)
	}
	_ = c.rdb.HDel(ctx, mergeEntryPrefix+repoID, prStr)
	return nil
}

// ForceRetry moves a failed/blocked PR back to the queued set.
func (c *AdminClient) ForceRetry(ctx context.Context, repoID string, prNumber int) error {
	prStr := fmt.Sprintf("%d", prNumber)

	// Find the entry to preserve its enqueuedAt score
	raw, err := c.rdb.HGet(ctx, mergeEntryPrefix+repoID, prStr).Result()
	if errors.Is(err, redis.Nil) {
		return fmt.Errorf("%w: PR #%d in repo %q", ErrMergeEntryNotFound, prNumber, repoID)
	}
	if err != nil {
		return fmt.Errorf("hget merge entry: %w", err)
	}

	var entry MergeEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return fmt.Errorf("unmarshal merge entry: %w", err)
	}

	// Remove from failed and blocked sets
	_ = c.rdb.ZRem(ctx, mergeFailedPrefix+repoID, prStr)
	_ = c.rdb.ZRem(ctx, mergeBlockedPrefix+repoID, prStr)

	// Re-add to queue with original (or current) score
	score := float64(entry.EnqueuedAt)
	if score == 0 {
		score = float64(time.Now().UnixMilli())
	}
	if err := c.rdb.ZAdd(ctx, mergeQueuePrefix+repoID, redis.Z{
		Score:  score,
		Member: prStr,
	}).Err(); err != nil {
		return fmt.Errorf("zadd merge queue: %w", err)
	}

	// Update entry status
	entry.Status = "queued"
	entry.FailureReason = ""
	entry.BlockReason = ""
	updated, _ := json.Marshal(entry)
	_ = c.rdb.HSet(ctx, mergeEntryPrefix+repoID, prStr, updated)
	return nil
}
