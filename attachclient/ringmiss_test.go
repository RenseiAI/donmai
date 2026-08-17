package attachclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
)

// This file pins the §13 ring-miss fix: a relay-state-loss stop (most commonly
// a relay restart wiping its in-memory ring, § 13 "Relay restart ... every
// viewer resume is a ring miss ... This is the designed repair path") must
// RESET-AND-RETRY — a fresh re-attach with fromSeq 0 / no resume position —
// and must NEVER surface as a terminal RunHost return. Before this fix, a
// relay-sent error.code=ring-miss control (any carrier) and the degraded
// lane's own rewind hitting an evicted local seq both produced a terminal
// *RelayStopError, permanently blinding the session's viewers.

// TestDegradedPostRewindPastLocalRingIsResetNotTerminal pins the literal
// production repro: the degraded lane's 409 rewind asks the host to resend
// from a seq the host's OWN retained local ring no longer holds (e.g. a
// long-lived session whose bounded ring has since rotated past it, or a relay
// that kept a durable ack/generation counter across a restart while its ring
// bytes were lost). Session.Subscribe(ack) then fails with a ring-miss-shaped
// error — before the fix this produced a terminal *RelayStopError
// (code=ring-miss); it must instead be a *RelayRingMissError.
func TestDegradedPostRewindPastLocalRingIsResetNotTerminal(t *testing.T) {
	t.Parallel()
	const relayAck = 2 // the relay's rewind target the host can no longer serve

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/host/sse"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/host/output"):
			writeJSONStatus(w, http.StatusConflict, attachwire.HostBatchRejected{BatchID: "irrelevant", AckSeq: relayAck})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sess := newFakeSession(1)
	for i := 0; i < 10; i++ {
		sess.PushOutput([]byte("f")) // head = 10
	}
	// The host's own local ring has rotated past seq 2 — it can no longer
	// serve the relay's rewind target (ack=2 → Subscribe(2)).
	sess.SetEvictBelow(3)

	tok := mkHostToken(testSessionID, 1, "host-jti", true)
	cfg := HostConfig{
		AttachURL:            strings.Replace(srv.URL, "http://", "ws://", 1) + "/" + attachwire.VersionPathSegment + "/rooms/room-1",
		TokenSource:          staticToken(tok, nil),
		Session:              sess,
		UpgradeProbeInterval: time.Hour,
	}
	if err := cfg.withDefaults(); err != nil {
		t.Fatalf("withDefaults: %v", err)
	}
	cl, err := parseHostClaims(tok)
	if err != nil {
		t.Fatalf("parseHostClaims: %v", err)
	}
	h := &host{cfg: cfg, log: cfg.Logger}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, rerr := h.runDegraded(ctx, tok, cl, time.Time{})

	if !isRelayRingMiss(rerr) {
		t.Fatalf("runDegraded err = %v, want *RelayRingMissError (§13 RESET-AND-RETRY)", rerr)
	}
	if isRelayStop(rerr) {
		t.Fatalf("runDegraded err = %v classified as *RelayStopError — a ring miss must never be terminal", rerr)
	}
}

// TestRingMissOnWSSResetsAndReattachesFresh proves a relay-sent
// error.code=ring-miss control on the WSS lane never terminates RunHost: the
// client resets its local resume position (fromSeq back to 0, i.e. no resume
// position on the next attach) and recovers.
func TestRingMissOnWSSResetsAndReattachesFresh(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.RingMissRetryCeiling = 20 * time.Millisecond
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 1 }, 3*time.Second) {
		t.Fatalf("initial WSS frame never delivered (head=%d)", h.relay.Head())
	}

	h.relay.SendToHost(mustFrame(t, attachwire.ControlError{
		Code: attachwire.CodeRingMiss, Message: "relay restarted, ring lost", Retryable: false,
	}))

	// Must NOT terminate — §13 makes this RESET-AND-RETRY regardless of the
	// wire's retryable bit.
	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated on a WSS ring-miss control (%v); it must reset and retry", err)
	case <-time.After(250 * time.Millisecond):
	}

	// Recovers: rebinds and streams fresh output again.
	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-reset"))
		return h.relay.Head() >= 2
	}, 4*time.Second) {
		t.Fatalf("client did not recover after WSS ring-miss reset (head=%d)", h.relay.Head())
	}

	// The reset re-attached with fromSeq 0 (no resume position) — a second
	// fromSeq=0 subscribe call beyond the initial attach's own.
	seqs := h.sess.SubscribeSeqs()
	var zeros int
	for _, s := range seqs {
		if s == 0 {
			zeros++
		}
	}
	if zeros < 2 {
		t.Errorf("subscribeSeqs = %v, want >= 2 fromSeq=0 calls (initial attach + fresh reset re-attach)", seqs)
	}
}

// TestRingMissOnDegradedSSEResetsWithoutResumeFrom is the degraded-lane
// counterpart: a relay-sent ring-miss control on the host SSE-down leg must
// reset-and-retry, re-attaching with NO resume position, never terminating.
func TestRingMissOnDegradedSSEResetsWithoutResumeFrom(t *testing.T) {
	h := startDegradedHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.RingMissRetryCeiling = 20 * time.Millisecond
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 1 }, 3*time.Second) {
		t.Fatalf("initial degraded frame never delivered (head=%d)", h.relay.Head())
	}

	h.relay.SendToHost(mustFrame(t, attachwire.ControlError{
		Code: attachwire.CodeRingMiss, Message: "relay restarted, ring lost", Retryable: false,
	}))

	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated on a degraded-lane ring-miss control (%v); it must reset and retry", err)
	case <-time.After(250 * time.Millisecond):
	}

	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-reset"))
		return h.relay.Head() >= 2
	}, 4*time.Second) {
		t.Fatalf("degraded client did not recover after ring-miss reset (head=%d)", h.relay.Head())
	}

	seqs := h.sess.SubscribeSeqs()
	var zeros int
	for _, s := range seqs {
		if s == 0 {
			zeros++
		}
	}
	if zeros < 2 {
		t.Errorf("subscribeSeqs = %v, want >= 2 fromSeq=0 calls (initial attach + fresh reset re-attach — NO resume_from)", seqs)
	}
}

// TestRepeatedRingMissNeverTerminatesAndResubscribesFresh drives several
// ring-miss bounces in a row (the production incident this fix targets saw
// four relay restarts in 30 minutes) and proves RunHost survives every one of
// them — resubscribing fresh (fromSeq 0) each time, never returning — and
// still reaches a clean nil return on a normal session end afterward.
func TestRepeatedRingMissNeverTerminatesAndResubscribesFresh(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.RingMissRetryCeiling = 20 * time.Millisecond
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	const bounces = 3
	prevSubs := len(h.sess.SubscribeSeqs())
	for i := 0; i < bounces; i++ {
		h.relay.SendToHost(mustFrame(t, attachwire.ControlError{
			Code: attachwire.CodeRingMiss, Message: "relay restarted, ring lost", Retryable: false,
		}))
		if !waitFor(func() bool { return len(h.sess.SubscribeSeqs()) > prevSubs }, 3*time.Second) {
			t.Fatalf("bounce %d: host never re-subscribed after ring-miss reset (subscribeSeqs=%v)", i, h.sess.SubscribeSeqs())
		}
		prevSubs = len(h.sess.SubscribeSeqs())
		// Wait for the relay to finish processing the re-attach's subscribe
		// control before firing the next bounce — SendToHost is a no-op while
		// unbound, and Subscribe() is called client-side slightly before the
		// relay-side bind completes.
		waitBound(t, h.relay)

		select {
		case err := <-h.done:
			t.Fatalf("bounce %d: RunHost terminated on ring-miss (%v); it must never be terminal", i, err)
		default:
		}
	}

	// Every subscribe in this test — the initial attach and every reset
	// re-attach — used fromSeq 0: no output was pushed between bounces, so any
	// call that resumed at the (nonzero, already-streamed) head instead of
	// resetting to 0 would show up here.
	for i, s := range h.sess.SubscribeSeqs() {
		if s != 0 {
			t.Errorf("subscribeSeqs[%d] = %d, want 0 (every attach here is the initial one or a §13 reset)", i, s)
		}
	}

	h.sess.PushExit(0)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost returned %v after repeated ring-miss + Exit, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return after Exit")
	}
}

// TestRelayRestartDuringSessionSurvivesAndReattaches is a broader end-to-end
// regression: an actual simulated relay-process restart (StubRelay.
// SimulateRestart — ring/epoch/ack all wiped, current host leg forcibly
// dropped) must never take a live session down. The host observes the drop as
// an ordinary connection failure, reconnects, and rebinds into the now-blank
// room, and the session keeps streaming afterward.
func TestRelayRestartDuringSessionSurvivesAndReattaches(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.RingMissRetryCeiling = 20 * time.Millisecond
	})
	h.sess.PushOutput([]byte("before-restart"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 1 }, 3*time.Second) {
		t.Fatalf("initial frame never delivered (head=%d)", h.relay.Head())
	}

	h.relay.SimulateRestart()

	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated across a relay restart (%v); it must reconnect", err)
	case <-time.After(250 * time.Millisecond):
	}

	if !waitFor(func() bool { return h.relay.HostBound() }, 4*time.Second) {
		t.Fatal("host leg never rebound after the simulated relay restart")
	}
	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("after-restart"))
		return h.relay.Head() >= 1
	}, 4*time.Second) {
		t.Fatalf("session did not resume streaming after the relay restart (head=%d)", h.relay.Head())
	}

	h.sess.PushExit(0)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost returned %v after a relay restart + Exit, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return after Exit")
	}
}
