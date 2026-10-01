package heartbeat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

// TestOnHeartbeatAckFiresWithExactTickOnSuccess covers the heartbeat
// observation contract: a successful lock-refresh acknowledgement fires
// Config.OnHeartbeatAck exactly once with the exact unix-ms tick snapshot
// the pulser stored in LastTick. The runner persists that snapshot into
// state.State.LastHeartbeat — the field host-watch reads.
func TestOnHeartbeatAckFiresWithExactTickOnSuccess(t *testing.T) {
	t.Parallel()

	fixed := time.UnixMilli(1_786_000_000_123).UTC()
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": true})
	})

	acked := make(chan int64, 8)
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:  "s1",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Interval:   24 * time.Hour, // suppress further ticks — first tick must suffice
		Now:        func() time.Time { return fixed },
		OnHeartbeatAck: func(tick int64) {
			acked <- tick
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case tick := <-acked:
		if tick != fixed.UnixMilli() {
			t.Fatalf("OnHeartbeatAck tick = %d; want exact snapshot %d", tick, fixed.UnixMilli())
		}
		if got := p.LastTick(); got != tick {
			t.Fatalf("LastTick = %d; want the exact snapshot delivered to OnHeartbeatAck %d", got, tick)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnHeartbeatAck did not fire on a successful acknowledgement")
	}

	// Exactly one ack per successful tick — no replay duplication.
	select {
	case extra := <-acked:
		t.Fatalf("OnHeartbeatAck fired twice for one tick (extra=%d); replay must not duplicate", extra)
	default:
	}
}

// TestOnHeartbeatAckSilentOnFailedAck covers the negative contract: a
// failed lock-refresh attempt (HTTP 500 here) must not fire
// OnHeartbeatAck. Failed attempts must never advance heartbeat freshness.
func TestOnHeartbeatAckSilentOnFailedAck(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})

	fired := make(chan int64, 8)
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour, // one synchronous tick only
		MaxAttemptsPerTick: 1,              // no retry backoff to wait out
		Sleep:              func(time.Duration) {},
		OnHeartbeatAck: func(tick int64) {
			fired <- tick
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The first tick fires synchronously inside Start, so by the time it
	// returns the failed attempt has already run — a non-blocking check is
	// deterministic, not timing-dependent.
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case tick := <-fired:
		t.Fatalf("OnHeartbeatAck fired (%d) on a failed attempt; only successful acks may advance freshness", tick)
	default:
	}
	if got := p.LastTick(); got != 0 {
		t.Fatalf("LastTick = %d; want 0 after a failed attempt", got)
	}
}

// TestOnHeartbeatAckSilentOnRefusedRefresh covers the ownership-refusal
// contract: a platform refusal (refreshed=false) is a strike-eligible
// failure, not an acknowledgement, so OnHeartbeatAck must not fire.
func TestOnHeartbeatAckSilentOnRefusedRefresh(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"refreshed": false})
	})

	fired := make(chan int64, 8)
	p, err := heartbeat.New(heartbeat.Config{
		SessionID:          "s1",
		BaseURL:            srv.URL,
		HTTPClient:         srv.Client(),
		Interval:           24 * time.Hour, // one synchronous tick only
		MaxAttemptsPerTick: 1,
		StrikesUntilLost:   5, // stay clear of the fuse; only the ack path is under test
		Sleep:              func(time.Duration) {},
		OnHeartbeatAck: func(tick int64) {
			fired <- tick
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	select {
	case tick := <-fired:
		t.Fatalf("OnHeartbeatAck fired (%d) on a refused refresh; refusals must not advance freshness", tick)
	default:
	}
}
