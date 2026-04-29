package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/RenseiAI/agentfactory-tui/afclient/queue"
)

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────

func newTestAdmin(t *testing.T) (*queue.AdminClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := queue.NewAdminClient("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// seedWorkItem writes a work item JSON into the work:items hash and the
// work:queue sorted set so that ListWorkItems / PeekWorkItem can read it.
func seedWorkItem(t *testing.T, mr *miniredis.Miniredis, sessionID string, item map[string]any, score float64) {
	t.Helper()
	data, _ := json.Marshal(item)
	mr.HSet("work:items", sessionID, string(data))
	if _, err := mr.ZAdd("work:queue", score, sessionID); err != nil {
		t.Fatalf("zadd work:queue: %v", err)
	}
}

// seedSession writes a session JSON into agent:session:<id>.
func seedSession(t *testing.T, mr *miniredis.Miniredis, sessionID string, sess map[string]any) {
	t.Helper()
	data, _ := json.Marshal(sess)
	if err := mr.Set("agent:session:"+sessionID, string(data)); err != nil {
		t.Fatalf("mr.Set session: %v", err)
	}
}

// seedMergeEntry writes a merge entry into merge:entry:<repoID> and the
// relevant sorted set (merge:queue:<repoID> by default).
func seedMergeEntry(t *testing.T, mr *miniredis.Miniredis, repoID string, prNumber int, entry map[string]any, zsetSuffix string) {
	t.Helper()
	data, _ := json.Marshal(entry)
	prStr := strconv.Itoa(prNumber)
	mr.HSet("merge:entry:"+repoID, prStr, string(data))
	key := "merge:" + zsetSuffix + ":" + repoID
	if _, err := mr.ZAdd(key, float64(prNumber), prStr); err != nil {
		t.Fatalf("zadd %s: %v", key, err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// NewAdminClient
// ──────────────────────────────────────────────────────────────────────────────

func TestNewAdminClient_EmptyURL(t *testing.T) {
	t.Parallel()
	_, err := queue.NewAdminClient("")
	if !errors.Is(err, queue.ErrRedisURLRequired) {
		t.Fatalf("want ErrRedisURLRequired, got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ListWorkItems
// ──────────────────────────────────────────────────────────────────────────────

func TestListWorkItems_Empty(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	items, err := c.ListWorkItems(context.Background())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("want 0 items, got %d", len(items))
	}
}

func TestListWorkItems_Single(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	seedWorkItem(t, mr, "sess-001", map[string]any{
		"issueIdentifier": "REN-99",
		"workType":        "development",
		"priority":        float64(2),
	}, 100)

	items, err := c.ListWorkItems(context.Background())
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if items[0].IssueIdentifier != "REN-99" {
		t.Errorf("want IssueIdentifier REN-99, got %q", items[0].IssueIdentifier)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// PeekWorkItem
// ──────────────────────────────────────────────────────────────────────────────

func TestPeekWorkItem_Empty(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	_, err := c.PeekWorkItem(context.Background())
	if !errors.Is(err, queue.ErrItemNotFound) {
		t.Fatalf("want ErrItemNotFound, got %v", err)
	}
}

func TestPeekWorkItem_ReturnsFirstByScore(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)

	seedWorkItem(t, mr, "sess-low", map[string]any{
		"issueIdentifier": "REN-200",
	}, 200)
	seedWorkItem(t, mr, "sess-high", map[string]any{
		"issueIdentifier": "REN-100",
	}, 100)

	item, err := c.PeekWorkItem(context.Background())
	if err != nil {
		t.Fatalf("PeekWorkItem: %v", err)
	}
	// score 100 should be first
	if item.IssueIdentifier != "REN-100" {
		t.Errorf("want REN-100 (lowest score), got %q", item.IssueIdentifier)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ListSessions
// ──────────────────────────────────────────────────────────────────────────────

func TestListSessions_Empty(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions, got %d", len(sessions))
	}
}

func TestListSessions_Found(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	seedSession(t, mr, "abc123", map[string]any{
		"status":          "running",
		"issueIdentifier": "REN-42",
		"workType":        "qa",
	})

	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "running" {
		t.Errorf("want status running, got %q", sessions[0].Status)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DropSession
// ──────────────────────────────────────────────────────────────────────────────

func TestDropSession_NotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	_, err := c.DropSession(context.Background(), "nonexistent")
	if !errors.Is(err, queue.ErrItemNotFound) {
		t.Fatalf("want ErrItemNotFound, got %v", err)
	}
}

func TestDropSession_RemovesSession(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	seedSession(t, mr, "sess-drop", map[string]any{"status": "running"})
	seedWorkItem(t, mr, "sess-drop", map[string]any{"issueIdentifier": "REN-55"}, 50)

	n, err := c.DropSession(context.Background(), "sess-drop")
	if err != nil {
		t.Fatalf("DropSession: %v", err)
	}
	if n < 1 {
		t.Errorf("want >= 1 removed, got %d", n)
	}

	// Verify gone
	sessions, _ := c.ListSessions(context.Background())
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after drop, got %d", len(sessions))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// RequeueSession
// ──────────────────────────────────────────────────────────────────────────────

func TestRequeueSession_NotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	_, err := c.RequeueSession(context.Background(), "ghost")
	if !errors.Is(err, queue.ErrItemNotFound) {
		t.Fatalf("want ErrItemNotFound, got %v", err)
	}
}

func TestRequeueSession_SetsStatusPending(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	seedSession(t, mr, "sess-requeue", map[string]any{
		"status":   "running",
		"workerId": "worker-1",
	})

	n, err := c.RequeueSession(context.Background(), "sess-requeue")
	if err != nil {
		t.Fatalf("RequeueSession: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 requeued, got %d", n)
	}

	sessions, _ := c.ListSessions(context.Background())
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "pending" {
		t.Errorf("want status pending after requeue, got %q", sessions[0].Status)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ListMergeQueue
// ──────────────────────────────────────────────────────────────────────────────

func TestListMergeQueue_Empty(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	snap, err := c.ListMergeQueue(context.Background(), "my-org/my-repo")
	if err != nil {
		t.Fatalf("ListMergeQueue: %v", err)
	}
	if snap.Depth != 0 {
		t.Errorf("want depth 0, got %d", snap.Depth)
	}
	if len(snap.Entries) != 0 {
		t.Errorf("want 0 entries, got %d", len(snap.Entries))
	}
}

func TestListMergeQueue_WithEntries(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	repo := "org/repo"

	seedMergeEntry(t, mr, repo, 42, map[string]any{
		"repoId":       repo,
		"prNumber":     float64(42),
		"sourceBranch": "feature/foo",
		"priority":     float64(1),
		"enqueuedAt":   float64(1000000),
	}, "queue")

	seedMergeEntry(t, mr, repo, 99, map[string]any{
		"repoId":        repo,
		"prNumber":      float64(99),
		"sourceBranch":  "feature/bar",
		"failureReason": "merge conflict",
	}, "failed")

	snap, err := c.ListMergeQueue(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListMergeQueue: %v", err)
	}
	if snap.Depth != 1 {
		t.Errorf("want depth 1 (queued), got %d", snap.Depth)
	}
	if len(snap.Entries) != 2 {
		t.Errorf("want 2 total entries (queued+failed), got %d", len(snap.Entries))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DequeueEntry
// ──────────────────────────────────────────────────────────────────────────────

func TestDequeueEntry_NotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	err := c.DequeueEntry(context.Background(), "my-repo", 1)
	if !errors.Is(err, queue.ErrMergeEntryNotFound) {
		t.Fatalf("want ErrMergeEntryNotFound, got %v", err)
	}
}

func TestDequeueEntry_RemovesEntry(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	repo := "test/dequeue"
	seedMergeEntry(t, mr, repo, 7, map[string]any{
		"prNumber": float64(7),
	}, "queue")

	if err := c.DequeueEntry(context.Background(), repo, 7); err != nil {
		t.Fatalf("DequeueEntry: %v", err)
	}

	snap, _ := c.ListMergeQueue(context.Background(), repo)
	if len(snap.Entries) != 0 {
		t.Errorf("expected 0 entries after dequeue, got %d", len(snap.Entries))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ForceRetry
// ──────────────────────────────────────────────────────────────────────────────

func TestForceRetry_NotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestAdmin(t)
	err := c.ForceRetry(context.Background(), "no-repo", 99)
	if !errors.Is(err, queue.ErrMergeEntryNotFound) {
		t.Fatalf("want ErrMergeEntryNotFound, got %v", err)
	}
}

func TestForceRetry_MovesFromFailedToQueue(t *testing.T) {
	t.Parallel()
	c, mr := newTestAdmin(t)
	repo := "test/retry"
	seedMergeEntry(t, mr, repo, 3, map[string]any{
		"prNumber":      float64(3),
		"sourceBranch":  "feature/retry",
		"failureReason": "ci failed",
		"enqueuedAt":    float64(999),
	}, "failed")

	if err := c.ForceRetry(context.Background(), repo, 3); err != nil {
		t.Fatalf("ForceRetry: %v", err)
	}

	snap, err := c.ListMergeQueue(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListMergeQueue: %v", err)
	}
	if snap.Depth != 1 {
		t.Errorf("want depth 1 after retry, got %d", snap.Depth)
	}
	if len(snap.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(snap.Entries))
	}
	if snap.Entries[0].Status != "queued" {
		t.Errorf("want status queued after retry, got %q", snap.Entries[0].Status)
	}
}
