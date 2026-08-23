package sessionshim

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestV2AuthoritativeSnapshotProxyExactRetryOrderingAndPostExit(t *testing.T) {
	id := Identity{OrgID: "org-snapshot-v2", SessionID: "session-snapshot-v2"}
	f := startShimHelper(t, id, 0)
	result := f.adoptAsMax(t, "controller-snapshot-v2", shimwire.V2)
	if len(result.Adopted) != 1 {
		t.Fatalf("adopted = %d, want 1", len(result.Adopted))
	}
	c := result.Adopted[0]
	if c.SelectedVersion() != shimwire.V2 || !c.SupportsAuthoritativeSnapshot() {
		t.Fatalf("selected/capability = v%d/%v, want v2/true", c.SelectedVersion(), c.SupportsAuthoritativeSnapshot())
	}

	// Produce a value only the real PTY-owned VT can know, and wait until its raw
	// output has crossed the shim before inspecting that same authoritative VT.
	exchange(t, c, "snapshot-authority")
	inspect, err := c.InspectSnapshot(context.Background())
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}
	screen, err := attachwire.DecodeScreen(inspect.Bytes)
	if err != nil {
		t.Fatalf("inspect screen: %v", err)
	}
	if got := screenText(screen); !strings.Contains(got, "ack:snapshot-authority") {
		t.Fatalf("authoritative screen %q lacks harness output", got)
	}
	if inspect.InStream {
		t.Fatal("inspect unexpectedly allocated/delivered a host frame")
	}
	inspectAgain, err := c.InspectSnapshot(context.Background())
	if err != nil || inspectAgain.AtSeq != inspect.AtSeq {
		t.Fatalf("read-only inspect advanced host sequence: first=%d second=%d err=%v", inspect.AtSeq, inspectAgain.AtSeq, err)
	}

	const requestID = 77
	if err := c.WriteInput([]byte("snapshot-concurrent-order\r")); err != nil {
		t.Fatal(err)
	}
	emitted, err := c.SnapshotWithID(context.Background(), requestID, shimwire.SnapshotEmit)
	if err != nil {
		t.Fatalf("EmitSnapshot: %v", err)
	}
	frame, err := attachwire.DecodeFrame(emitted.Bytes)
	if err != nil {
		t.Fatalf("decode exact emitted frame: %v", err)
	}
	if frame.Type != attachwire.TypeSnapshot || !emitted.InStream || frame.Seq != emitted.AtSeq+1 {
		t.Fatalf("live emit disposition = frame(type=%s seq=%d) result(atSeq=%d inStream=%v)", frame.Type, frame.Seq, emitted.AtSeq, emitted.InStream)
	}

	var delivered []byte
	var lastStreamSeq uint64
	deadline := time.After(10 * time.Second)
	for delivered == nil {
		select {
		case ev := <-c.Events():
			if ev.Kind == EventOutput {
				if ev.Seq <= lastStreamSeq {
					t.Fatalf("ordinary output sequence reordered: %d after %d", ev.Seq, lastStreamSeq)
				}
				lastStreamSeq = ev.Seq
			}
			if ev.Kind == EventSnapshotFrame {
				if ev.Seq <= lastStreamSeq {
					t.Fatalf("snapshot sequence reordered: %d after %d", ev.Seq, lastStreamSeq)
				}
				lastStreamSeq = ev.Seq
				delivered = ev.FrameBytes
			}
		case <-deadline:
			t.Fatal("timed out waiting for emitted snapshot in controller stream")
		}
	}
	if !bytes.Equal(delivered, emitted.Bytes) {
		t.Fatal("controller stream did not preserve exact emitted frame bytes")
	}

	retry, err := c.SnapshotWithID(context.Background(), requestID, shimwire.SnapshotEmit)
	if err != nil || !snapshotResultsEqual(retry, emitted) {
		t.Fatalf("exact retry = (%+v,%v), want first immutable result", retry, err)
	}
	if _, err := c.SnapshotWithID(context.Background(), requestID, shimwire.SnapshotInspect); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("changed replay = %v, want ErrSnapshotMismatch", err)
	}

	// Drive the wire-level changed-replay and stale-generation refusals too. The
	// public controller rejects changed replay before writing; these deliberate
	// package-level calls prove the shim's independent closed ledger/fence.
	if _, err := c.SnapshotWithID(context.Background(), 88, shimwire.SnapshotInspect); err != nil {
		t.Fatalf("seed request 88: %v", err)
	}
	changed := installRawSnapshotCall(c, shimwire.SnapshotRequest{RequestID: 88, Generation: c.gen, Mode: shimwire.SnapshotEmit})
	if err := writeRawSnapshotRequest(c, changed.request); err != nil {
		t.Fatal(err)
	}
	<-changed.done
	if changed.result == nil || changed.result.Code != shimwire.CodeDuplicateChanged || !errors.Is(changed.err, shimwire.ErrSnapshotRefused) {
		t.Fatalf("wire changed replay = result %+v err %v", changed.result, changed.err)
	}
	staleGen := c.gen - 1
	if staleGen == 0 {
		staleGen = c.gen + 1
	}
	stale := installRawSnapshotCall(c, shimwire.SnapshotRequest{RequestID: 89, Generation: staleGen, Mode: shimwire.SnapshotInspect})
	if err := writeRawSnapshotRequest(c, stale.request); err != nil {
		t.Fatal(err)
	}
	<-stale.done
	if stale.result == nil || stale.result.Code != shimwire.CodeStaleGeneration || !errors.Is(stale.err, shimwire.ErrSnapshotRefused) {
		t.Fatalf("stale generation = result %+v err %v", stale.result, stale.err)
	}
	unknownReq := shimwire.SnapshotRequest{RequestID: 91, Generation: c.gen, Mode: shimwire.SnapshotMode(99)}
	unknown := installRawSnapshotCall(c, unknownReq)
	unknownBody := make([]byte, 17)
	binary.BigEndian.PutUint64(unknownBody[0:8], unknownReq.RequestID)
	binary.BigEndian.PutUint64(unknownBody[8:16], uint64(unknownReq.Generation))
	unknownBody[16] = byte(unknownReq.Mode)
	if err := c.w.WriteVersion(c.selected, shimwire.TypeSnapshotRequest, unknownBody); err != nil {
		t.Fatal(err)
	}
	<-unknown.done
	if unknown.result == nil || unknown.result.Code != shimwire.CodeMalformed || !errors.Is(unknown.err, shimwire.ErrSnapshotRefused) {
		t.Fatalf("unknown mode = result %+v err %v", unknown.result, unknown.err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := c.SnapshotWithID(expired, 90, shimwire.SnapshotInspect); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired request = %v, want DeadlineExceeded", err)
	}
	if late, err := c.SnapshotWithID(context.Background(), 90, shimwire.SnapshotInspect); err != nil || late.Code != "" {
		t.Fatalf("exact retry after caller timeout = (%+v,%v), want first immutable success", late, err)
	}
	noDouble := time.NewTimer(250 * time.Millisecond)
	defer noDouble.Stop()
	for {
		select {
		case ev := <-c.Events():
			if ev.Kind == EventSnapshotFrame {
				t.Fatal("exact retry delivered the emitted frame twice")
			}
		case <-noDouble.C:
			goto postExit
		}
	}

postExit:
	if err := c.Stop(shimwire.StopOperator); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var exitSeq uint64
	exitDeadline := time.After(10 * time.Second)
	for exitSeq == 0 {
		select {
		case ev := <-c.Events():
			if ev.Kind == EventExit {
				exitSeq = ev.Exit.Seq
			}
		case <-exitDeadline:
			t.Fatal("timed out waiting for Exit")
		}
	}
	final, err := c.EmitSnapshot(context.Background())
	if err != nil {
		t.Fatalf("post-Exit EmitSnapshot: %v", err)
	}
	finalFrame, err := attachwire.DecodeFrame(final.Bytes)
	if err != nil {
		t.Fatalf("decode post-Exit frame: %v", err)
	}
	if final.InStream || final.AtSeq != exitSeq || finalFrame.Seq != attachwire.PostExitSnapshotSeq {
		t.Fatalf("post-Exit disposition = result(atSeq=%d inStream=%v) frameSeq=%d, want atSeq=%d false seq0", final.AtSeq, final.InStream, finalFrame.Seq, exitSeq)
	}
}

func installRawSnapshotCall(c *Controller, req shimwire.SnapshotRequest) *snapshotCall {
	call := &snapshotCall{request: req, sent: true, done: make(chan struct{})}
	c.snapshotMu.Lock()
	c.snapshotCalls[req.RequestID] = call
	c.snapshotMu.Unlock()
	return call
}

func writeRawSnapshotRequest(c *Controller, req shimwire.SnapshotRequest) error {
	body, err := shimwire.EncodeSnapshotRequest(req)
	if err != nil {
		return err
	}
	return c.w.WriteVersion(c.selected, shimwire.TypeSnapshotRequest, body)
}

func screenText(screen attachwire.Screen) string {
	var b strings.Builder
	for _, cell := range screen.Primary {
		b.Write(cell.RuneBytes)
	}
	return b.String()
}

func TestControllerSnapshotRetryLedgerIsBounded(t *testing.T) {
	t.Parallel()
	c := &Controller{
		selected:      shimwire.V2,
		gen:           1,
		snapshotCalls: make(map[uint64]*snapshotCall, controllerSnapshotRetryLedgerLimit),
	}
	for i := uint64(1); i <= controllerSnapshotRetryLedgerLimit; i++ {
		c.snapshotCalls[i] = &snapshotCall{
			request: shimwire.SnapshotRequest{RequestID: i, Generation: 1, Mode: shimwire.SnapshotInspect},
			done:    make(chan struct{}),
		}
	}
	if _, err := c.SnapshotWithID(context.Background(), controllerSnapshotRetryLedgerLimit+1, shimwire.SnapshotInspect); !errors.Is(err, shimwire.ErrSnapshotRefused) {
		t.Fatalf("overflow = %v, want ErrSnapshotRefused", err)
	}
}
