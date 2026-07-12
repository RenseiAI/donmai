package attachclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
)

// runHostEpoch starts a RunHost host leg with an explicit epoch + jti.
func runHostEpoch(t *testing.T, relay *attachtest.StubRelay, epoch int64, jti string) (*fakeSession, chan error, context.CancelFunc) {
	t.Helper()
	sess := newFakeSession(uint64(epoch)) //nolint:gosec // G115: epoch is a small non-negative test constant
	ctx, cancel := context.WithCancel(context.Background())
	cfg := HostConfig{
		AttachURL:            relay.BaseWSURL(),
		TokenSource:          staticToken(mkHostToken(testSessionID, epoch, jti, true), nil),
		Session:              sess,
		BackoffMin:           5 * time.Millisecond,
		BackoffMax:           30 * time.Millisecond,
		FinalScreenWindow:    100 * time.Millisecond,
		UpgradeProbeInterval: time.Hour,
	}
	done := make(chan error, 1)
	go func() { done <- RunHost(ctx, cfg) }()
	t.Cleanup(cancel)
	return sess, done, cancel
}

func TestEpochStaleDuplicateRejected(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// Host A binds at epoch 5.
	sessA, _, cancelA := runHostEpoch(t, relay, 5, "host-A")
	sessA.PushOutput([]byte("A"))
	waitBound(t, relay)
	defer cancelA()

	// Host B presents epoch 5 with a DIFFERENT jti while A is live → epoch-stale,
	// terminal (RunHost returns ErrEpochStale, no retry).
	_, doneB, _ := runHostEpoch(t, relay, 5, "host-B")
	select {
	case err := <-doneB:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("host B returned %v, want ErrEpochStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host B did not terminate with epoch-stale")
	}

	// A is still the bound leg.
	if !relay.HostBound() {
		t.Error("host A should still be bound after B's rejection")
	}
	if relay.Epoch() != 5 {
		t.Errorf("epoch = %d, want 5", relay.Epoch())
	}
}

func TestEpochHigherSupersedes(t *testing.T) {
	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// Host A binds at epoch 5.
	sessA, doneA, _ := runHostEpoch(t, relay, 5, "host-A")
	sessA.PushOutput([]byte("A"))
	waitBound(t, relay)

	// Host C at epoch 6 supersedes: the relay closes A and begins a new
	// generation.
	sessC, _, cancelC := runHostEpoch(t, relay, 6, "host-C")
	sessC.PushOutput([]byte("C"))
	defer cancelC()

	if !waitFor(func() bool { return relay.Epoch() == 6 }, 3*time.Second) {
		t.Fatalf("epoch = %d, want 6 after supersede", relay.Epoch())
	}

	// A, superseded and now stale on reconnect, terminates with epoch-stale.
	select {
	case err := <-doneA:
		if !errors.Is(err, ErrEpochStale) {
			t.Fatalf("superseded host A returned %v, want ErrEpochStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded host A did not terminate with epoch-stale")
	}
	if !relay.HostBound() {
		t.Error("host C should be bound after superseding A")
	}
}
