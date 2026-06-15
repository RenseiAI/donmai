package landing

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestKeyString(t *testing.T) {
	tests := []struct {
		name string
		key  Key
		want string
	}{
		{"org+repo", Key{OrgID: "org1", RepoID: "owner/repo"}, "landing:org1:owner/repo"},
		{"empty org still namespaces", Key{OrgID: "", RepoID: "owner/repo"}, "landing::owner/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKeyValid(t *testing.T) {
	tests := []struct {
		name string
		key  Key
		want bool
	}{
		{"both set", Key{OrgID: "o", RepoID: "r"}, true},
		{"no org", Key{OrgID: "", RepoID: "r"}, false},
		{"no repo", Key{OrgID: "o", RepoID: ""}, false},
		{"neither", Key{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEncodeScoreOrdering(t *testing.T) {
	base := fixedTime
	tests := []struct {
		name     string
		aPri     int
		aTime    time.Time
		bPri     int
		bTime    time.Time
		aBeforeB bool // true if entry a should dequeue before b (lower score)
	}{
		{"lower priority wins regardless of time", 1, base.Add(time.Hour), 3, base, true},
		{"same priority FIFO older first", 2, base, 2, base.Add(time.Minute), true},
		{"higher priority value loses", 5, base, 2, base.Add(time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sa := encodeScore(tt.aPri, tt.aTime)
			sb := encodeScore(tt.bPri, tt.bTime)
			if got := sa < sb; got != tt.aBeforeB {
				t.Errorf("encodeScore: a(%v) < b(%v) = %v, want %v", sa, sb, got, tt.aBeforeB)
			}
		})
	}
}

// TestEncodeScoreTieBreakerStaysInBand verifies the fractional FIFO tie-breaker
// never bleeds into the next integer priority band.
func TestEncodeScoreTieBreakerStaysInBand(t *testing.T) {
	// A priority-2 entry enqueued far in the future must still sort ahead of a
	// priority-3 entry enqueued at the epoch.
	high := encodeScore(2, fixedTime.Add(100*365*24*time.Hour))
	low := encodeScore(3, enqueueEpoch)
	if high >= low {
		t.Fatalf("priority-2 score %v should be < priority-3 score %v", high, low)
	}
}

func newTestStorage(t *testing.T) *RedisStorage {
	t.Helper()
	_, rdb := newTestRedis(t)
	return NewRedisStorage(rdb)
}

func mkEntry(orgID, repoID string, proposal, priority int, branch string, at time.Time) Entry {
	return Entry{
		OrgID:        orgID,
		RepoID:       repoID,
		Proposal:     proposal,
		ProposalURL:  "https://example.test/pull/" + branch,
		IssueID:      "ABC-" + branch,
		Priority:     priority,
		SourceBranch: branch,
		TargetBranch: "main",
		EnqueuedAt:   at,
	}
}

func TestRedisStorageEnqueueDequeueOrdering(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}

	// Enqueue out of priority order; expect dequeue in priority order, FIFO ties.
	entries := []Entry{
		mkEntry("org1", "owner/repo", 10, 3, "10", fixedTime),
		mkEntry("org1", "owner/repo", 11, 1, "11", fixedTime.Add(time.Minute)),
		mkEntry("org1", "owner/repo", 12, 1, "12", fixedTime), // same pri, earlier → first
		mkEntry("org1", "owner/repo", 13, 2, "13", fixedTime),
	}
	for _, e := range entries {
		if err := s.Enqueue(ctx, e); err != nil {
			t.Fatalf("Enqueue(%d): %v", e.Proposal, err)
		}
	}

	wantOrder := []int{12, 11, 13, 10} // pri1(older), pri1(newer), pri2, pri3
	for i, want := range wantOrder {
		got, err := s.Dequeue(ctx, key)
		if err != nil {
			t.Fatalf("Dequeue #%d: %v", i, err)
		}
		if got == nil {
			t.Fatalf("Dequeue #%d returned nil", i)
		}
		if got.Proposal != want {
			t.Errorf("Dequeue #%d = %d, want %d", i, got.Proposal, want)
		}
	}
	// Queue now empty.
	got, err := s.Dequeue(ctx, key)
	if err != nil {
		t.Fatalf("Dequeue empty: %v", err)
	}
	if got != nil {
		t.Errorf("Dequeue on empty queue = %+v, want nil", got)
	}
}

func TestRedisStorageDequeuePreservesMetadata(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	in := mkEntry("org1", "owner/repo", 42, 2, "feature/ABC-42", fixedTime)
	if err := s.Enqueue(ctx, in); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	out, err := s.Dequeue(ctx, key)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if out.SourceBranch != in.SourceBranch || out.IssueID != in.IssueID ||
		out.Priority != in.Priority || out.ProposalURL != in.ProposalURL ||
		out.TargetBranch != in.TargetBranch || out.OrgID != "org1" || out.RepoID != "owner/repo" {
		t.Errorf("Dequeue metadata mismatch: got %+v, in %+v", *out, in)
	}
	if !out.EnqueuedAt.Equal(in.EnqueuedAt) {
		t.Errorf("EnqueuedAt = %v, want %v", out.EnqueuedAt, in.EnqueuedAt)
	}
}

func TestRedisStorageEnqueueIdempotent(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	first := mkEntry("org1", "owner/repo", 7, 2, "branch-a", fixedTime)
	if err := s.Enqueue(ctx, first); err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	// Re-enqueue same proposal with different metadata; must be a no-op.
	second := mkEntry("org1", "owner/repo", 7, 9, "branch-b", fixedTime.Add(time.Hour))
	if err := s.Enqueue(ctx, second); err != nil {
		t.Fatalf("Enqueue second: %v", err)
	}
	depth, err := s.QueueDepth(ctx, key)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 1 {
		t.Errorf("QueueDepth = %d, want 1", depth)
	}
	out, err := s.Dequeue(ctx, key)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if out.SourceBranch != "branch-a" {
		t.Errorf("idempotent enqueue should keep original entry; got branch %q", out.SourceBranch)
	}
}

func TestRedisStorageTenantIsolation(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	// Two orgs share the same repoId (the FD-4 collision case).
	keyA := Key{OrgID: "orgA", RepoID: "owner/repo"}
	keyB := Key{OrgID: "orgB", RepoID: "owner/repo"}

	if err := s.Enqueue(ctx, mkEntry("orgA", "owner/repo", 1, 2, "a1", fixedTime)); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	if err := s.Enqueue(ctx, mkEntry("orgB", "owner/repo", 1, 2, "b1", fixedTime)); err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}

	depthA, _ := s.QueueDepth(ctx, keyA)
	depthB, _ := s.QueueDepth(ctx, keyB)
	if depthA != 1 || depthB != 1 {
		t.Fatalf("each org queue should hold 1: A=%d B=%d", depthA, depthB)
	}

	// Dequeuing org A must not touch org B's identically-numbered proposal.
	outA, err := s.Dequeue(ctx, keyA)
	if err != nil {
		t.Fatalf("Dequeue A: %v", err)
	}
	if outA.SourceBranch != "a1" {
		t.Errorf("org A dequeue branch = %q, want a1", outA.SourceBranch)
	}
	depthB, _ = s.QueueDepth(ctx, keyB)
	if depthB != 1 {
		t.Errorf("org B queue must be untouched, depth = %d, want 1", depthB)
	}
	outB, _ := s.Dequeue(ctx, keyB)
	if outB.SourceBranch != "b1" {
		t.Errorf("org B dequeue branch = %q, want b1", outB.SourceBranch)
	}
}

func TestRedisStoragePeekAllOrdered(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 30, 3, "30", fixedTime))
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 31, 1, "31", fixedTime))
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 32, 2, "32", fixedTime))

	got, err := s.PeekAll(ctx, key)
	if err != nil {
		t.Fatalf("PeekAll: %v", err)
	}
	gotProposals := make([]int, len(got))
	for i, e := range got {
		gotProposals[i] = e.Proposal
	}
	want := []int{31, 32, 30}
	if !reflect.DeepEqual(gotProposals, want) {
		t.Errorf("PeekAll order = %v, want %v", gotProposals, want)
	}
	// PeekAll must not remove.
	depth, _ := s.QueueDepth(ctx, key)
	if depth != 3 {
		t.Errorf("PeekAll removed entries; depth = %d, want 3", depth)
	}
}

func TestRedisStorageDequeueBatchAtomic(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	for _, p := range []int{1, 2, 3, 4} {
		_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", p, 2, "b", fixedTime))
	}
	// Request 2,3 plus a non-present 99 → only 2,3 returned.
	out, err := s.DequeueBatch(ctx, key, []int{2, 3, 99})
	if err != nil {
		t.Fatalf("DequeueBatch: %v", err)
	}
	gotProposals := make([]int, len(out))
	for i, e := range out {
		gotProposals[i] = e.Proposal
	}
	sort.Ints(gotProposals)
	if !reflect.DeepEqual(gotProposals, []int{2, 3}) {
		t.Errorf("DequeueBatch returned %v, want [2 3]", gotProposals)
	}
	depth, _ := s.QueueDepth(ctx, key)
	if depth != 2 {
		t.Errorf("DequeueBatch left depth = %d, want 2", depth)
	}
	// A second DequeueBatch for the same proposals returns nothing (already gone).
	again, err := s.DequeueBatch(ctx, key, []int{2, 3})
	if err != nil {
		t.Fatalf("DequeueBatch again: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second DequeueBatch returned %d entries, want 0", len(again))
	}
}

func TestRedisStoragePositionAndIsEnqueued(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 5, 1, "5", fixedTime))
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 6, 2, "6", fixedTime))

	pos, err := s.Position(ctx, key, 5)
	if err != nil || pos != 1 {
		t.Errorf("Position(5) = %d, %v; want 1, nil", pos, err)
	}
	pos, _ = s.Position(ctx, key, 6)
	if pos != 2 {
		t.Errorf("Position(6) = %d, want 2", pos)
	}
	pos, _ = s.Position(ctx, key, 999)
	if pos != 0 {
		t.Errorf("Position(absent) = %d, want 0", pos)
	}

	ok, _ := s.IsEnqueued(ctx, key, 5)
	if !ok {
		t.Error("IsEnqueued(5) = false, want true")
	}
	ok, _ = s.IsEnqueued(ctx, key, 999)
	if ok {
		t.Error("IsEnqueued(absent) = true, want false")
	}
}

func TestRedisStorageMarkAndReasons(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}

	// Failed.
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 1, 2, "1", fixedTime))
	if err := s.MarkFailed(ctx, key, 1, "tests failed"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if r, _ := s.FailedReason(ctx, key, 1); r != "tests failed" {
		t.Errorf("FailedReason = %q, want %q", r, "tests failed")
	}
	if enq, _ := s.IsEnqueued(ctx, key, 1); enq {
		t.Error("MarkFailed should remove from queue")
	}

	// Blocked.
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 2, 2, "2", fixedTime))
	if err := s.MarkBlocked(ctx, key, 2, "conflict"); err != nil {
		t.Fatalf("MarkBlocked: %v", err)
	}
	if r, _ := s.BlockedReason(ctx, key, 2); r != "conflict" {
		t.Errorf("BlockedReason = %q, want %q", r, "conflict")
	}

	// Completed clears the queue and any stale reasons.
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 3, 2, "3", fixedTime))
	_ = s.MarkFailed(ctx, key, 3, "old")
	if err := s.MarkCompleted(ctx, key, 3); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	if r, _ := s.FailedReason(ctx, key, 3); r != "" {
		t.Errorf("MarkCompleted should clear failed reason; got %q", r)
	}

	// Absent reasons return "".
	if r, _ := s.FailedReason(ctx, key, 777); r != "" {
		t.Errorf("FailedReason(absent) = %q, want empty", r)
	}
	if r, _ := s.BlockedReason(ctx, key, 777); r != "" {
		t.Errorf("BlockedReason(absent) = %q, want empty", r)
	}
}

func TestRedisStorageRemove(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	key := Key{OrgID: "org1", RepoID: "owner/repo"}
	_ = s.Enqueue(ctx, mkEntry("org1", "owner/repo", 1, 2, "1", fixedTime))
	if err := s.Remove(ctx, key, 1); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ok, _ := s.IsEnqueued(ctx, key, 1); ok {
		t.Error("Remove should delete the proposal from the queue")
	}
}

func TestRedisStorageRejectsInvalidKey(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	bad := Key{OrgID: "", RepoID: "owner/repo"}

	if err := s.Enqueue(ctx, Entry{RepoID: "owner/repo", Proposal: 1}); err == nil {
		t.Error("Enqueue with empty OrgID should error")
	}
	if _, err := s.Dequeue(ctx, bad); err == nil {
		t.Error("Dequeue with invalid key should error")
	}
	if _, err := s.PeekAll(ctx, bad); err == nil {
		t.Error("PeekAll with invalid key should error")
	}
	if _, err := s.QueueDepth(ctx, bad); err == nil {
		t.Error("QueueDepth with invalid key should error")
	}
}

func TestExtractIssueID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"ABC-1153", "ABC-1153"},
		{"ABC-1153: short title", "ABC-1153"},
		{"feature/ABC-1153-cedar-stuff", "ABC-1153"},
		{"abc-1153", "ABC-1153"},
		{"v1.2.3-rc4", ""},
		{"", ""},
		{"no-identifier-here", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ExtractIssueID(tt.in); got != tt.want {
				t.Errorf("ExtractIssueID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
