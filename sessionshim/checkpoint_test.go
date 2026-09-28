package sessionshim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestV5ContinuationRoundTrip(t *testing.T) {
	f := startShimHelper(t, Identity{OrgID: "org-checkpoint", SessionID: "session-checkpoint"}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := Adopt(ctx, AdoptOptions{Registry: f.registry, ControllerID: "checkpoint-controller", ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V5, RequireFullHostFrames: true, ExpectedWorkarea: func(Identity) string { return f.workarea }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(result.Close)
	if len(result.Adopted) != 1 {
		t.Fatalf("adopted %d", len(result.Adopted))
	}
	c := result.Adopted[0]
	if c.SelectedVersion() != shimwire.V5 || !c.SupportsContinuation() {
		t.Fatal("continuation not negotiated")
	}
	seq := exchange(t, c, "checkpoint-authority")
	checkpoint, err := c.InspectContinuation(ctx, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatalf("%v; stream cause=%v", err, c.StreamEndCause())
	}
	if checkpoint.Epoch != c.Hello().Continuation.HostEpoch || uint64(checkpoint.AtSeq) < seq || !strings.Contains(screenText(checkpoint.Picture), "ack:checkpoint-authority") {
		t.Fatal("checkpoint lost authoritative state or boundary")
	}
	t.Run("wrong_pty_epoch", func(t *testing.T) {
		altered := checkpoint
		altered.Epoch++
		altered.Picture.Epoch = altered.Epoch
		encoded, err := altered.Encode(attachwire.ContinuationSchema)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > shimwire.CheckpointChunkBytes {
			t.Fatal("fixture exceeds one chunk")
		}
		req := shimwire.CheckpointRequest{RequestID: 1, Generation: c.gen, Schema: attachwire.ContinuationSchema}
		receiver := &Controller{hello: c.hello, checkpointCall: &continuationCall{request: req, done: make(chan struct{})}}
		chunk, err := shimwire.EncodeCheckpointResult(shimwire.CheckpointResult{RequestID: 1, Generation: c.gen, Total: uint32(len(encoded) & (shimwire.CheckpointChunkBytes - 1)), Digest: sha256.Sum256(encoded), Bytes: encoded})
		if err != nil {
			t.Fatal(err)
		}
		if err := receiver.acceptContinuationChunk(chunk); err == nil {
			t.Fatal("checkpoint from another PTY epoch was accepted")
		}
		if receiver.checkpointCall.completed || receiver.checkpointCall.result != nil {
			t.Fatal("wrong epoch advanced checkpoint state")
		}
	})
	again, err := c.InspectContinuation(ctx, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if again.AtSeq != checkpoint.AtSeq {
		t.Fatal("out-of-band inspect allocated canonical sequence")
	}
	legacy, err := c.InspectSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.AtSeq != uint64(checkpoint.AtSeq) {
		t.Fatal("legacy picture and complete checkpoint disagree on anchor")
	}
	if _, err := attachwire.DecodeScreen(legacy.Bytes); err != nil {
		t.Fatal("legacy inspect no longer encoded0x01")
	}
	if next := exchange(t, c, "after-checkpoint"); next <= uint64(checkpoint.AtSeq) {
		t.Fatal("raw tail failed to advance")
	}
}

func TestV5ContinuationChunkRejectsWrongCorrelation(t *testing.T) {
	req := shimwire.CheckpointRequest{RequestID: 1, Generation: 3, Schema: attachwire.ContinuationSchema}
	for _, tc := range []struct {
		name   string
		result shimwire.CheckpointResult
	}{
		{"request", shimwire.CheckpointResult{RequestID: 2, Generation: 3, Total: 1, Bytes: []byte{1}}},
		{"generation", shimwire.CheckpointResult{RequestID: 1, Generation: 4, Total: 1, Bytes: []byte{1}}},
		{"offset", shimwire.CheckpointResult{RequestID: 1, Generation: 3, Total: shimwire.CheckpointChunkBytes + 1, Offset: shimwire.CheckpointChunkBytes, Bytes: []byte{1}}},
		{"digest", shimwire.CheckpointResult{RequestID: 1, Generation: 3, Total: 1, Bytes: []byte{1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Controller{checkpointCall: &continuationCall{request: req, done: make(chan struct{})}}
			body, err := shimwire.EncodeCheckpointResult(tc.result)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.acceptContinuationChunk(body); err == nil {
				t.Fatal("invalid result accepted")
			}
			if c.checkpointCall.completed || c.checkpointCall.result != nil {
				t.Fatal("invalid result committed")
			}
		})
	}
}

func TestV5ContinuationPostExitAndLegacyRefusal(t *testing.T) {
	t.Run("post-exit", func(t *testing.T) {
		sh, c, _ := finalScreenFixture(t, shimwire.V5, defaultFinalScreenWindow)
		exchange(t, c, "final-continuation")
		if err := c.Stop(shimwire.StopOperator); err != nil {
			t.Fatal(err)
		}
		exit := awaitFinalScreenExit(t, c)
		awaitFinalScreenSignal(t, sh.finalScreenWindowStarted, "final screen window not ready")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		checkpoint, err := c.InspectContinuation(ctx, attachwire.ContinuationSchema)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.Exit == nil || uint64(checkpoint.AtSeq) != exit.Seq {
			t.Fatal("final complete checkpoint lost Exit boundary")
		}
		terminal, err := ptyhost.RestoreContinuation(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = terminal.Close() })
		if _, ended := terminal.Exit(); !ended {
			t.Fatal("restored final checkpoint is not ended")
		}
		if err := terminal.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: uint64(terminal.AtSeq()) + 1, Payload: attachwire.EncodeOutput([]byte("after exit"))}); err == nil {
			t.Fatal("ended mirror accepted output")
		}
	})
	for _, version := range []uint32{shimwire.V1, shimwire.V2, shimwire.V3, shimwire.V4} {
		t.Run(fmt.Sprintf("legacy-v%d", version), func(t *testing.T) {
			_, old, registry := finalScreenFixture(t, version, defaultFinalScreenWindow)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			newer, err := Adopt(context.Background(), AdoptOptions{Registry: registry, ControllerID: "new-controller-old-shim", ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V5, RequireFullHostFrames: true})
			if err != nil || len(newer.Adopted) != 1 {
				t.Fatalf("new controller failed older shim: %v", err)
			}
			t.Cleanup(newer.Close)
			c := newer.Adopted[0]
			if c.SelectedVersion() != version {
				t.Fatal("legacy shim version changed during overlap")
			}

			if c.SupportsContinuation() {
				t.Fatal("legacy peer advertised continuation")
			}
			if _, err := c.InspectContinuation(context.Background(), attachwire.ContinuationSchema); err == nil {
				t.Fatal("legacy peer accepted checkpoint operation")
			}
			if c.checkpointCall != nil {
				t.Fatal("unsupported request allocated or sent a checkpoint call")
			}
			exchange(t, c, "legacy-still-live")
		})
	}
}

type blockedContinuationWriter struct {
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
	calls    atomic.Int32
}

func (w *blockedContinuationWriter) Write(p []byte) (int, error) {
	n := w.calls.Add(1)
	if n == 1 {
		close(w.entered)
	}
	<-w.release
	if n == 2 {
		close(w.finished)
	}
	return len(p), nil
}

func TestV5ContinuationDeadlineWhileWriterBackpressured(t *testing.T) {
	writer := &blockedContinuationWriter{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	t.Cleanup(func() {
		close(writer.release)
		select {
		case <-writer.finished:
		case <-time.After(time.Second):
			t.Error("checkpoint sender did not release")
		}
	})
	c := &Controller{selected: shimwire.V5, gen: 1, hello: shimwire.Hello{Continuation: &shimwire.CheckpointCapability{Schema: attachwire.ContinuationSchema}}, w: shimwire.NewWriter(writer), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.InspectContinuation(ctx, attachwire.ContinuationSchema); result <- err }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint request was not attempted")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("backpressured cancellation=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline waited for network writer")
	}
	c.checkpointMu.Lock()
	completed := c.checkpointCall.completed
	c.checkpointMu.Unlock()
	if completed {
		t.Fatal("cancelled in-flight request was falsely committed")
	}
}

func TestContinuationSupportRequiresDecodedOptionalAdvertisement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selected   uint32
		capability *shimwire.CheckpointCapability
		want       bool
	}{
		{"max5_without_capability", shimwire.V5, nil, false},
		{"unknown_schema", shimwire.V5, &shimwire.CheckpointCapability{Schema: "unknown", HostEpoch: 19}, false},
		{"selected4", shimwire.V4, &shimwire.CheckpointCapability{Schema: attachwire.ContinuationSchema, HostEpoch: 19}, false},
		{"selected5_explicit_zero_epoch", shimwire.V5, &shimwire.CheckpointCapability{Schema: attachwire.ContinuationSchema, HostEpoch: 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := shimwire.EncodeHello(shimwire.Hello{Min: shimwire.V1, Max: shimwire.V5, Continuation: tc.capability})
			if err != nil {
				t.Fatal(err)
			}
			hello, err := shimwire.DecodeHello(raw)
			if err != nil {
				t.Fatal(err)
			}
			c := &Controller{selected: tc.selected, hello: hello}
			if got := c.SupportsContinuation(); got != tc.want {
				t.Fatalf("support=%t want=%t", got, tc.want)
			}
		})
	}
}
