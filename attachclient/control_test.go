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

func mustFrame(t *testing.T, m attachwire.ControlMessage) attachwire.Frame {
	t.Helper()
	f, err := attachwire.BuildControlFrame(m)
	if err != nil {
		t.Fatalf("build control frame: %v", err)
	}
	return f
}

func TestKillHookInvokedOnce(t *testing.T) {
	var killed atomic.Int64
	var gotReason atomic.Value
	h := startHost(t, attachtest.Config{}, func(c *HostConfig) {
		c.Kill = func(_ context.Context, reason, _ string) error {
			killed.Add(1)
			gotReason.Store(reason)
			return nil
		}
	})
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	sig := "SIGTERM"
	h.relay.SendToHost(mustFrame(t, attachwire.Kill{Reason: attachwire.KillStopped, Signal: &sig}))
	// Redelivery (at-least-once) must not invoke the hook again.
	h.relay.SendToHost(mustFrame(t, attachwire.Kill{Reason: attachwire.KillStopped, Signal: &sig}))

	if !waitFor(func() bool { return killed.Load() >= 1 }, 2*time.Second) {
		t.Fatal("Kill hook was never invoked")
	}
	time.Sleep(100 * time.Millisecond)
	if killed.Load() != 1 {
		t.Errorf("Kill hook invoked %d times, want exactly 1 (idempotent)", killed.Load())
	}
	if r, _ := gotReason.Load().(string); r != "stopped" {
		t.Errorf("kill reason = %q, want stopped", r)
	}
}

func TestNonRetryableErrorControlStops(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	h.relay.SendToHost(mustFrame(t, attachwire.ControlError{
		Code: attachwire.CodeAuth, Message: "token rejected", Retryable: false,
	}))

	select {
	case err := <-h.done:
		var stop *RelayStopError
		if !errors.As(err, &stop) {
			t.Fatalf("RunHost returned %v, want *RelayStopError", err)
		}
		if stop.Code != attachwire.CodeAuth {
			t.Errorf("RelayStopError.Code = %s, want auth", stop.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not stop on a non-retryable error control")
	}
}

func TestUnknownFrameTypeIsFramingErrorAndReconnects(t *testing.T) {
	h := startHost(t, attachtest.Config{}, nil)
	h.sess.PushOutput([]byte("boot"))
	waitBound(t, h.relay)

	// An unknown frame type byte (0x09 is reserved, § 3) is a framing error: the
	// client closes the leg and reconnects (not terminal).
	h.relay.SendToHost(attachwire.Frame{Type: attachwire.EventType(0x09), Payload: []byte{1, 2, 3}})

	// Prove recovery: after reconnect, new frames still flow to the relay.
	if !waitFor(func() bool {
		h.sess.PushOutput([]byte("more"))
		return h.relay.Head() >= 4
	}, 4*time.Second) {
		t.Fatalf("client did not recover after a framing error (head=%d)", h.relay.Head())
	}

	select {
	case err := <-h.done:
		t.Fatalf("RunHost terminated on a framing error (%v); it should reconnect", err)
	default:
	}
}

func TestDegradedHostSSEEpochStaleTerminal(t *testing.T) {
	// WSS is refused, so both host legs use the degraded lane. Host A binds via
	// the SSE GET; host B (equal epoch, different jti) hits the CAS reject → the
	// SSE GET returns 409 → openHostSSE surfaces ErrEpochStale (terminal).
	relay := attachtest.New(attachtest.Config{RoomID: "room-1", RefuseWSS: true})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	sessA := newFakeSession(5)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go func() {
		_ = RunHost(ctxA, HostConfig{
			AttachURL:            relay.BaseWSURL(),
			TokenSource:          staticToken(mkHostToken(testSessionID, 5, "host-A", true), nil),
			Session:              sessA,
			FallbackAfterN:       1,
			BackoffMin:           5 * time.Millisecond,
			BackoffMax:           30 * time.Millisecond,
			FinalScreenWindow:    100 * time.Millisecond,
			UpgradeProbeInterval: time.Hour,
		})
	}()
	sessA.PushOutput([]byte("A"))
	waitBound(t, relay)

	sessB := newFakeSession(5)
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunHost(context.Background(), HostConfig{
			AttachURL:            relay.BaseWSURL(),
			TokenSource:          staticToken(mkHostToken(testSessionID, 5, "host-B", true), nil),
			Session:              sessB,
			FallbackAfterN:       1,
			BackoffMin:           5 * time.Millisecond,
			BackoffMax:           30 * time.Millisecond,
			FinalScreenWindow:    100 * time.Millisecond,
			UpgradeProbeInterval: time.Hour,
		})
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("degraded host B returned %v, want ErrEpochStale", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("degraded host B did not terminate with epoch-stale")
	}
}
