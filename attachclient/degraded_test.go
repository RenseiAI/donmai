package attachclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
)

// startDegradedHost starts a RunHost that must fall back to the degraded lane
// (the relay refuses WSS). FallbackAfterN=1 makes the fallback immediate.
func startDegradedHost(t *testing.T, relayCfg attachtest.Config, mut func(*HostConfig)) *harness {
	relayCfg.RefuseWSS = true
	return startHost(t, relayCfg, func(c *HostConfig) {
		c.FallbackAfterN = 1
		c.FinalScreenWindow = 150 * time.Millisecond
		if mut != nil {
			mut(c)
		}
	})
}

func assertContiguousUnique(t *testing.T, seqs []uint64) {
	t.Helper()
	seen := make(map[uint64]bool)
	for i, s := range seqs {
		if seen[s] {
			t.Errorf("duplicate host seq %d in ring", s)
		}
		seen[s] = true
		if i > 0 && s != seqs[i-1]+1 {
			t.Errorf("non-contiguous ring at index %d: %d after %d", i, s, seqs[i-1])
		}
	}
}

func TestDegradedFallbackStreamsViaPOST(t *testing.T) {
	h := startDegradedHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("a"))
	h.sess.PushOutput([]byte("b"))
	h.sess.PushOutput([]byte("c"))

	// The degraded SSE-down GET binds the host leg; POST-up delivers the frames.
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 3 }, 3*time.Second) {
		t.Fatalf("degraded POST-up did not deliver frames (head=%d)", h.relay.Head())
	}
	if got := h.relay.HostAckSeq(); got != 3 {
		t.Errorf("hostAckSeq = %d, want 3", got)
	}
	assertContiguousUnique(t, h.relay.RingSeqs())

	h.sess.PushExit(0)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost (degraded) returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost (degraded) did not return after Exit")
	}
}

func TestDegradedPersistentSSE401BacksOffRecoversAndCancels(t *testing.T) {
	const basePath = "/v1/rooms/room-1"
	stale := mkHostToken(testSessionID, 1, "host-stale", true)
	replacement := mkHostToken(testSessionID, 1, "host-replacement", true)

	var sourceToken atomic.Value
	sourceToken.Store(stale)
	var allowedToken atomic.Value
	allowedToken.Store(replacement)
	var sseRequests atomic.Int64
	admitted := make(chan struct{}, 1)
	postAccepted := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case basePath:
			http.Error(w, "upgrade unavailable", http.StatusNotFound)
		case basePath + "/host/sse":
			sseRequests.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+allowedToken.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case admitted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		case basePath + "/host/output":
			if r.Header.Get("Authorization") != "Bearer "+allowedToken.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeAccepted(w)
			select {
			case postAccepted <- struct{}{}:
			default:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// The local PTY epoch and every refreshed grant must describe the same
	// process generation; this test varies jti/auth freshness, not ownership.
	sess := newFakeSession(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunHost(ctx, HostConfig{
			AttachURL: strings.Replace(srv.URL, "http://", "ws://", 1) + basePath,
			TokenSource: func(context.Context) (string, error) {
				return sourceToken.Load().(string), nil
			},
			Session:              sess,
			FallbackAfterN:       1,
			BackoffMin:           40 * time.Millisecond,
			BackoffMax:           40 * time.Millisecond,
			UpgradeProbeInterval: time.Hour,
		})
	}()

	// A missing/empty runner token file resolves to the same rejected static
	// bearer. The degraded SSE leg must hand that failure back to RunHost's
	// reconnect backoff instead of spinning inside openHostSSE.
	time.Sleep(220 * time.Millisecond)
	if got := sseRequests.Load(); got < 2 || got > 12 {
		cancel()
		t.Fatalf("SSE request count=%d during persistent 401; want bounded [2,12]", got)
	}

	// Once the provisioner makes a replacement available, the next top-level
	// attempt is admitted and the POST-up half uses the same refreshed bearer.
	sourceToken.Store(replacement)
	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("degraded SSE did not recover after token replacement")
	}
	select {
	case <-postAccepted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("degraded POST-up did not use the replacement token")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunHost returned %v after cancellation; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunHost did not cancel promptly after degraded recovery")
	}
}

func TestDegradedPostRewind409(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	sess := newFakeSession(1)
	for i := 0; i < 5; i++ {
		sess.PushOutput([]byte("f")) // head = 5
	}

	tok := mkHostToken(testSessionID, 1, "host-jti", true)
	cfg := HostConfig{
		AttachURL:            relay.BaseWSURL(),
		TokenSource:          staticToken(tok, nil),
		Session:              sess,
		UpgradeProbeInterval: time.Hour,
		FinalScreenWindow:    100 * time.Millisecond,
	}
	if err := cfg.withDefaults(); err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	cl, err := parseHostClaims(tok)
	if err != nil {
		t.Fatalf("parseHostClaims: %v", err)
	}
	// hasStreamed=true → reconnect discipline: subscribe from the current head (5)
	// → firstSeq 6, which the relay 409s (its ack is 0) → rewind to Subscribe(0)
	// and resend from seq 1.
	h := &host{cfg: cfg, log: cfg.Logger, hasStreamed: true}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.runDegraded(ctx, tok, cl, time.Time{})
		done <- err
	}()

	waitBound(t, relay)
	// Wait until the client has subscribed at the current head (5) before
	// producing frame 6, so the firstSeq=6 batch (vs the relay's ack 0)
	// deterministically triggers the 409 rewind rather than racing the head read.
	if !waitFor(func() bool { return sess.SubscriberCount() >= 1 }, 3*time.Second) {
		t.Fatal("client never subscribed to the session")
	}
	sess.PushOutput([]byte("f6")) // head = 6 → triggers the firstSeq=6 batch → 409

	if !waitFor(func() bool { return relay.Head() >= 6 && relay.HostAckSeq() >= 6 }, 3*time.Second) {
		t.Fatalf("degraded rewind did not deliver frames 1..6 (head=%d ack=%d)", relay.Head(), relay.HostAckSeq())
	}
	assertContiguousUnique(t, relay.RingSeqs())

	sess.PushExit(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDegraded returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runDegraded did not return after Exit")
	}
}

func TestDegradedBatchIDIdempotentOnDroppedResponse(t *testing.T) {
	h := startDegradedHost(t, attachtest.Config{DropHostPOSTOnce: true}, nil)
	h.sess.PushOutput([]byte("a"))
	h.sess.PushOutput([]byte("b"))
	h.sess.PushOutput([]byte("c"))
	waitBound(t, h.relay)

	// The first applied POST's 200 is dropped; the client retries the SAME
	// batchId; the relay de-duplicates → the frames are applied exactly once.
	if !waitFor(func() bool { return h.relay.Head() >= 3 }, 3*time.Second) {
		t.Fatalf("frames not delivered after dropped-response retry (head=%d)", h.relay.Head())
	}
	assertContiguousUnique(t, h.relay.RingSeqs()) // no double-apply

	h.sess.PushExit(0)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return after Exit")
	}
}

func TestDegradedSSEInputDedup(t *testing.T) {
	h := startDegradedHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	// Same stamped Input delivered twice on the at-least-once SSE lane → applied
	// once (dedup by (userId, penGeneration, inputSeq)).
	dup := attachwire.InputPayload{InputSeq: 1, PenGeneration: 0, UserID: []byte("user-A"), Data: []byte("dup")}
	dupFrame := attachwire.Frame{Type: attachwire.TypeInput, Payload: dup.Encode()}
	h.relay.SendToHost(dupFrame)
	h.relay.SendToHost(dupFrame)

	if !waitFor(func() bool { return len(h.sess.Inputs()) >= 1 }, 3*time.Second) {
		t.Fatal("first SSE Input never reached WriteInput")
	}
	// A genuinely new keystroke still lands.
	next := attachwire.InputPayload{InputSeq: 2, PenGeneration: 0, UserID: []byte("user-A"), Data: []byte("next")}
	h.relay.SendToHost(attachwire.Frame{Type: attachwire.TypeInput, Payload: next.Encode()})

	if !waitFor(func() bool { return len(h.sess.Inputs()) >= 2 }, 3*time.Second) {
		t.Fatalf("second (distinct) SSE Input never reached WriteInput (got %d)", len(h.sess.Inputs()))
	}
	time.Sleep(100 * time.Millisecond)
	inputs := h.sess.Inputs()
	if len(inputs) != 2 {
		t.Fatalf("WriteInput called %d times, want 2 (replay deduped)", len(inputs))
	}
	if string(inputs[0]) != "dup" || string(inputs[1]) != "next" {
		t.Errorf("inputs = %q,%q, want dup,next", inputs[0], inputs[1])
	}
}

func TestDegradedUpgradeBackToWSS(t *testing.T) {
	h := startDegradedHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.UpgradeProbeInterval = 30 * time.Millisecond
		c.FinalScreenWindow = 150 * time.Millisecond
	})
	h.sess.PushOutput([]byte("1"))
	h.sess.PushOutput([]byte("2"))
	h.sess.PushOutput([]byte("3"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 3 }, 3*time.Second) {
		t.Fatalf("degraded stream did not reach head 3 (head=%d)", h.relay.Head())
	}

	// WSS becomes reachable again → the client upgrades back.
	h.relay.SetRefuseWSS(false)
	time.Sleep(500 * time.Millisecond) // let the probe fire and the WSS leg rebind

	// Post-upgrade frames flow over WSS with no seq reset (carrier-invisible).
	h.sess.PushOutput([]byte("4"))
	h.sess.PushOutput([]byte("5"))
	if !waitFor(func() bool { return h.relay.Head() >= 5 }, 3*time.Second) {
		t.Fatalf("post-upgrade frames not delivered over WSS (head=%d)", h.relay.Head())
	}
	assertContiguousUnique(t, h.relay.RingSeqs()) // no host-seq regression across the switch

	h.sess.PushExit(0)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost returned %v, want nil after upgrade-back + Exit", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return after upgrade-back + Exit")
	}
}
