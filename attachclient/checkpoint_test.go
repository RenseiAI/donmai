package attachclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

type checkpointSourceFixture struct {
	*fakeSession
	checkpoint attachwire.ContinuationCheckpoint
}

func (s *checkpointSourceFixture) InspectContinuation(_ context.Context, selected string) (attachwire.ContinuationCheckpoint, error) {
	if selected != attachwire.ContinuationSchema {
		return attachwire.ContinuationCheckpoint{}, errors.New("schema mismatch")
	}
	return s.checkpoint, nil
}

func TestContinuationOpenFailsClosedOnEvictedTail(t *testing.T) {
	fs := newFakeSession(3)
	fs.PushOutput([]byte("old"))
	picture, _, err := fs.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	source := &checkpointSourceFixture{fakeSession: fs, checkpoint: attachwire.ContinuationCheckpoint{Schema: attachwire.ContinuationSchema, Epoch: 3, AtSeq: 1, Picture: picture, State: []byte("opaque fixture state")}}
	fs.evictBelow = 2
	c, sub, err := OpenContinuation(context.Background(), source, attachwire.ContinuationSchema)
	if !errors.Is(err, errFakeSessionEvicted) || sub != nil || c.Schema != "" {
		t.Fatalf("evicted handoff=%+v %v %v", c, sub, err)
	}
	seqs := fs.SubscribeSeqs()
	if len(seqs) != 1 || seqs[0] != 1 {
		t.Fatal("tail did not use exact checkpoint anchor")
	}
	if _, _, err := OpenContinuation(context.Background(), fs, attachwire.ContinuationSchema); err == nil {
		t.Fatal("legacy source was silently accepted")
	}
	if _, _, err := OpenContinuation(context.Background(), source, ""); err == nil {
		t.Fatal("unselected schema accepted")
	}
	if len(fs.SubscribeSeqs()) != 1 {
		t.Fatal("unsupported capability sent a subscribe operation")
	}
}

type cancellableCheckpointSource struct {
	*fakeSession
	entered chan context.Context
	release chan struct{}
}

func (s *cancellableCheckpointSource) InspectContinuation(ctx context.Context, _ string) (attachwire.ContinuationCheckpoint, error) {
	s.entered <- ctx
	select {
	case <-ctx.Done():
		return attachwire.ContinuationCheckpoint{}, ctx.Err()
	case <-s.release:
		return attachwire.ContinuationCheckpoint{}, errors.New("fixture stopped")
	}
}

func TestContinuationOpenPropagatesCancellation(t *testing.T) {
	source := &cancellableCheckpointSource{fakeSession: newFakeSession(1), entered: make(chan context.Context, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(source.release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, _, err := OpenContinuation(ctx, source, attachwire.ContinuationSchema); result <- err }()
	select {
	case received := <-source.entered:
		if received != ctx {
			t.Fatal("caller context was replaced")
		}
	case <-time.After(time.Second):
		t.Fatal("source was not invoked")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source cancellation did not return")
	}
	if len(source.SubscribeSeqs()) != 0 {
		t.Fatal("cancelled capture advanced to tail subscription")
	}
}

func (*checkpointSourceFixture) SupportsContinuation() bool     { return true }
func (*cancellableCheckpointSource) SupportsContinuation() bool { return true }
