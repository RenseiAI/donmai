package attachclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
)

const testSessionID = "sess-1"

type harness struct {
	relay   *attachtest.StubRelay
	sess    *fakeSession
	done    chan error
	cancel  context.CancelFunc
	tokUsed *atomic.Int64
}

func startHost(t *testing.T, relayCfg attachtest.Config, mut func(*HostConfig)) *harness {
	t.Helper()
	sess := newFakeSession(1)
	if relayCfg.RoomID == "" {
		relayCfg.RoomID = "room-1"
	}
	relay := attachtest.New(relayCfg)
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	calls := &atomic.Int64{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := HostConfig{
		AttachURL:            relay.BaseWSURL(),
		TokenSource:          staticToken(mkHostToken(testSessionID, 1, "host-jti-1", true), calls),
		Session:              sess,
		BackoffMin:           5 * time.Millisecond,
		BackoffMax:           50 * time.Millisecond,
		FinalScreenWindow:    300 * time.Millisecond,
		UpgradeProbeInterval: time.Hour,
	}
	if mut != nil {
		mut(&cfg)
	}
	done := make(chan error, 1)
	go func() { done <- RunHost(ctx, cfg) }()
	return &harness{relay: relay, sess: sess, done: done, cancel: cancel, tokUsed: calls}
}

func waitBound(t *testing.T, relay *attachtest.StubRelay) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if relay.HostBound() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for host leg to bind")
}

func attachViewer(t *testing.T, relay *attachtest.StubRelay, role attachwire.Role, resumeFrom *int64) *attachtest.Viewer {
	t.Helper()
	tok := mkViewerToken(testSessionID, "user-"+string(role), "vjti-"+string(role), string(role))
	v, err := attachtest.AttachViewer(context.Background(), relay.BaseWSURL(), tok, role, resumeFrom)
	if err != nil {
		t.Fatalf("attach viewer: %v", err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

// collect reads up to want frames (or until timeout / channel close).
func collect(v *attachtest.Viewer, want int, timeout time.Duration) []attachwire.Frame {
	var out []attachwire.Frame
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case f, ok := <-v.Frames():
			if !ok {
				return out
			}
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
	return out
}

// waitFor polls until cond is true or the deadline; returns success.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func assertSeqMonotonic(t *testing.T, frames []attachwire.Frame) {
	t.Helper()
	var last uint64
	for _, f := range frames {
		if f.Seq == 0 {
			continue // out-of-namespace (Control, post-Exit Snapshot)
		}
		if f.Seq <= last {
			t.Errorf("host seq regression: %d after %d (type %v)", f.Seq, last, f.Type)
		}
		last = f.Seq
	}
}

func TestHappyPathStreamSnapshotAndOrdering(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("line-1\r\n"))
	h.sess.PushOutput([]byte("line-2\r\n"))
	waitBound(t, h.relay)

	// A fresh viewer (resumeFrom null) → snapshot + tail (§ 13).
	v := attachViewer(t, h.relay, attachwire.RoleViewer, nil)

	// Wait for the join snapshot to round-trip, then stream more output.
	if !waitFor(func() bool { return h.relay.Head() >= 3 }, 3*time.Second) {
		t.Fatalf("snapshot did not advance the head (head=%d)", h.relay.Head())
	}
	h.sess.PushOutput([]byte("line-3\r\n"))
	h.sess.PushOutput([]byte("line-4\r\n"))

	frames := collect(v, 40, 2*time.Second)
	assertSeqMonotonic(t, frames)

	var sawSnapshot, sawOutput bool
	for _, f := range frames {
		switch f.Type {
		case attachwire.TypeSnapshot:
			sawSnapshot = true
		case attachwire.TypeOutput:
			sawOutput = true
		}
	}
	if !sawSnapshot {
		t.Error("viewer never received a Snapshot (join snapshot+tail path)")
	}
	if !sawOutput {
		t.Error("viewer never received Output frames")
	}
}

func TestViewerResumeRingHitReplays(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	for i := 0; i < 5; i++ {
		h.sess.PushOutput([]byte("frame"))
	}
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 5 }, 3*time.Second) {
		t.Fatalf("relay head did not reach 5 (head=%d)", h.relay.Head())
	}

	// Resume from seq 3 (still in the ring) → ring hit → replay from 4 onward, no
	// snapshot.
	from := int64(3)
	v := attachViewer(t, h.relay, attachwire.RoleViewer, &from)

	frames := collect(v, 20, 2*time.Second)
	assertSeqMonotonic(t, frames)

	var firstDataSeq uint64
	for _, f := range frames {
		if f.Type == attachwire.TypeOutput {
			firstDataSeq = f.Seq
			break
		}
	}
	if firstDataSeq != 4 {
		t.Errorf("ring-hit resume: first Output seq = %d, want 4 (replay from resumeFrom+1)", firstDataSeq)
	}
	for _, f := range frames {
		if f.Type == attachwire.TypeSnapshot {
			t.Error("ring-hit resume should not deliver a Snapshot")
		}
	}
}

func TestExitReturnsNilAfterFinalWindow(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.FinalScreenWindow = 150 * time.Millisecond
	})
	h.sess.PushOutput([]byte("hello"))
	waitBound(t, h.relay)
	h.sess.PushExit(0)

	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("RunHost returned %v, want nil on clean session end", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return within the final-screen window")
	}
}

func TestPostExitSnapshotAnswered(t *testing.T) {
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.FinalScreenWindow = 2 * time.Second // keep the leg alive to answer joins
	})
	h.sess.PushOutput([]byte("x"))
	h.sess.PushOutput([]byte("y"))
	waitBound(t, h.relay)
	if !waitFor(func() bool { return h.relay.Head() >= 2 }, 2*time.Second) {
		t.Fatalf("head did not reach 2")
	}
	exitAtHead := h.relay.Head() + 1
	h.sess.PushExit(0)
	if !waitFor(func() bool { return h.relay.Head() >= exitAtHead }, 2*time.Second) {
		t.Fatalf("Exit frame not observed by relay (head=%d want>=%d)", h.relay.Head(), exitAtHead)
	}

	// A viewer joining post-Exit gets the post-Exit final Snapshot.
	v := attachViewer(t, h.relay, attachwire.RoleViewer, nil)
	frames := collect(v, 40, 2*time.Second)
	var sawSnapshot bool
	for _, f := range frames {
		if f.Type == attachwire.TypeSnapshot {
			sawSnapshot = true
			env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
			if err != nil {
				t.Fatalf("decode post-Exit snapshot: %v", err)
			}
			if env.AtSeq != exitAtHead {
				t.Errorf("post-Exit snapshot atSeq = %d, want %d (Exit seq)", env.AtSeq, exitAtHead)
			}
		}
	}
	if !sawSnapshot {
		t.Error("viewer joining post-Exit never received a Snapshot")
	}
}

func TestInputTrustUnstampedNeverReachesSession(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	// Hostile relay: an UNSTAMPED Input (userIdLen 0) must never reach WriteInput.
	unstamped := attachwire.Frame{
		Type:    attachwire.TypeInput,
		Payload: attachwire.EncodeViewerInput(1, 0, []byte("rm -rf /\n")),
	}
	h.relay.SendToHost(unstamped)

	// A properly stamped Input DOES reach WriteInput.
	stamped := attachwire.InputPayload{InputSeq: 2, PenGeneration: 0, UserID: []byte("user-A"), Data: []byte("ok\n")}
	h.relay.SendToHost(attachwire.Frame{Type: attachwire.TypeInput, Payload: stamped.Encode()})

	if !waitFor(func() bool { return len(h.sess.Inputs()) >= 1 }, 2*time.Second) {
		t.Fatal("stamped Input never reached WriteInput")
	}
	// Give any (wrongly-accepted) unstamped input a chance to land, then assert
	// exactly the stamped payload was written.
	time.Sleep(100 * time.Millisecond)
	inputs := h.sess.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("WriteInput called %d times, want exactly 1 (unstamped dropped)", len(inputs))
	}
	if string(inputs[0]) != "ok\n" {
		t.Errorf("WriteInput data = %q, want %q", inputs[0], "ok\n")
	}
}

func TestAuthoritativeResizeApplied(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	rz, _ := attachwire.ResizePayload{Cols: 120, Rows: 40}.Encode()
	h.relay.SendToHost(attachwire.NewViewportResizeFrame(rz))

	if !waitFor(func() bool { return len(h.sess.Resizes()) >= 1 }, 2*time.Second) {
		t.Fatal("authoritative Resize never applied")
	}
	got := h.sess.Resizes()[0]
	if got.Cols != 120 || got.Rows != 40 {
		t.Errorf("Resize applied = %dx%d, want 120x40", got.Cols, got.Rows)
	}
}

func TestCtxCancelReturnsPromptly(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("hi"))
	waitBound(t, h.relay)

	start := time.Now()
	h.cancel()
	select {
	case err := <-h.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunHost returned %v, want context.Canceled", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("ctx cancel took %v to return, want prompt", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return promptly on ctx cancel")
	}
}

func TestReconnectResolvesTokenPerAttemptAndRecovers(t *testing.T) {
	// The relay refuses WSS and the degraded lane is disabled, so every dial
	// fails: RunHost keeps retrying, re-resolving the token on each attempt, with
	// bounded backoff. When WSS is re-enabled the client recovers (backoff was
	// reset-on-success / never diverged).
	h := startHost(t, attachtest.Config{RefuseWSS: true}, func(c *HostConfig) {
		c.DisableDegraded = true
	})
	h.sess.PushOutput([]byte("hi"))

	if !waitFor(func() bool { return h.tokUsed.Load() >= 3 }, 2*time.Second) {
		t.Fatalf("TokenSource called %d times across failed attempts, want >= 3 (per-attempt re-resolution)", h.tokUsed.Load())
	}

	h.relay.SetRefuseWSS(false)
	waitBound(t, h.relay) // recovered → backoff stayed bounded
}
