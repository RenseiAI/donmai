package landing

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis spins up an in-process miniredis and returns a connected client.
// The server and client are torn down via t.Cleanup so each test gets an
// isolated keyspace.
func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = rdb.Close()
		mr.Close()
	})
	return mr, rdb
}

// fixedTime is a deterministic clock anchor for enqueue-ordering tests.
var fixedTime = time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC)

// TestRedisClientDelIfMatches verifies the atomic compare-and-delete used to
// release the coordinator lock against a real (miniredis) Redis: a non-matching
// token is a no-op (it must NOT free another worker's re-acquired lock), and only
// the matching token deletes.
func TestRedisClientDelIfMatches(t *testing.T) {
	_, rdb := newTestRedis(t)
	rc := NewRedisClient(rdb)
	ctx := context.Background()
	const key = "landing:org1:owner/repo:lock"

	// Worker A holds the lock.
	if ok, err := rc.SetNX(ctx, key, "tokenA", time.Minute); err != nil || !ok {
		t.Fatalf("SetNX tokenA = (%v, %v), want (true, nil)", ok, err)
	}

	// A different worker's release (mismatched token) must be a no-op.
	deleted, err := rc.DelIfMatches(ctx, key, "tokenB")
	if err != nil {
		t.Fatalf("DelIfMatches(tokenB): %v", err)
	}
	if deleted {
		t.Error("DelIfMatches deleted with a NON-matching token; it must be a no-op")
	}
	if v, _ := rc.Get(ctx, key); v != "tokenA" {
		t.Errorf("lock value = %q after mismatched release, want tokenA (untouched)", v)
	}

	// The owner's release (matching token) deletes.
	deleted, err = rc.DelIfMatches(ctx, key, "tokenA")
	if err != nil {
		t.Fatalf("DelIfMatches(tokenA): %v", err)
	}
	if !deleted {
		t.Error("DelIfMatches with the matching token should delete")
	}
	if v, _ := rc.Get(ctx, key); v != "" {
		t.Errorf("lock value = %q after owner release, want empty", v)
	}

	// Releasing an absent key is a harmless no-op.
	if deleted, err := rc.DelIfMatches(ctx, key, "tokenA"); err != nil || deleted {
		t.Errorf("DelIfMatches(absent) = (%v, %v), want (false, nil)", deleted, err)
	}
}

// TestWorkerReleaseDoesNotFreeReacquiredLock proves the end-to-end safety
// property: after a Worker runs, its release only frees ITS OWN lock value. If
// another worker re-acquired the lock (its token now stored), the first worker's
// shutdown must not delete it.
func TestWorkerReleaseDoesNotFreeReacquiredLock(t *testing.T) {
	_, rdb := newTestRedis(t)
	rc := NewRedisClient(rdb)
	ctx := context.Background()
	const key = "landing:org1:owner/repo:lock"

	// Simulate worker A acquiring, then losing the lock (TTL expiry) and worker B
	// re-acquiring it with a different token.
	_, _ = rc.SetNX(ctx, key, "workerA-token", time.Minute)
	_ = rdb.Set(ctx, key, "workerB-token", time.Minute).Err() // B re-acquired

	// Worker A's shutdown release with its OWN (now stale) token must not delete
	// workerB's lock.
	deleted, err := rc.DelIfMatches(ctx, key, "workerA-token")
	if err != nil {
		t.Fatalf("DelIfMatches: %v", err)
	}
	if deleted {
		t.Error("stale worker freed a re-acquired lock; compare-and-delete failed")
	}
	if v, _ := rc.Get(ctx, key); v != "workerB-token" {
		t.Errorf("lock value = %q, want workerB-token (B still holds it)", v)
	}
}
