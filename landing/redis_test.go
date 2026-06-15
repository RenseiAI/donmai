package landing

import (
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
