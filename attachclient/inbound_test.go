package attachclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

func newTestHost(sess Session, kill KillFunc) *host {
	return &host{cfg: HostConfig{Session: sess, Kill: kill}, log: discardLogger()}
}

func TestApplyInputStampedAndUnstamped(t *testing.T) {
	t.Parallel()
	sess := newFakeSession(1)
	h := newTestHost(sess, nil)

	// Unstamped → dropped, never written.
	if _, err := h.applyInput(attachwire.EncodeViewerInput(1, 0, []byte("x"))); err != nil {
		t.Fatalf("applyInput(unstamped): %v", err)
	}
	if len(sess.Inputs()) != 0 {
		t.Fatalf("unstamped Input reached WriteInput")
	}
	// Stamped → written verbatim.
	stamped := attachwire.InputPayload{InputSeq: 2, UserID: []byte("u"), Data: []byte("hi")}
	if _, err := h.applyInput(stamped.Encode()); err != nil {
		t.Fatalf("applyInput(stamped): %v", err)
	}
	got := sess.Inputs()
	if len(got) != 1 || string(got[0]) != "hi" {
		t.Fatalf("WriteInput = %q, want [hi]", got)
	}
	// Malformed payload → framing error.
	if _, err := h.applyInput([]byte{0xFF}); !attachwire.IsFramingErr(err) {
		t.Errorf("applyInput(malformed) err = %v, want framing", err)
	}
}

func TestApplyResizeValidAndZeroDim(t *testing.T) {
	t.Parallel()
	sess := newFakeSession(1)
	h := newTestHost(sess, nil)

	valid, _ := attachwire.ResizePayload{Cols: 100, Rows: 30}.Encode()
	if _, err := h.applyResize(valid); err != nil {
		t.Fatalf("applyResize(valid): %v", err)
	}
	if rs := sess.Resizes(); len(rs) != 1 || rs[0].Cols != 100 {
		t.Fatalf("Resize not applied: %+v", rs)
	}
	// 0-dim geometry is a framing error (§ 3.1/§ 8).
	zero := attachwire.InputPayload{}.Encode() // decodes to cols=0,rows=0 shape for DecodeResize? no
	_ = zero
	// Encode a raw 0×0 resize payload directly.
	raw := append(attachwire.AppendUvarint(nil, 0), attachwire.AppendUvarint(nil, 0)...)
	raw = append(raw, 0, 0)
	if _, err := h.applyResize(raw); !attachwire.IsFramingErr(err) {
		t.Errorf("applyResize(0x0) err = %v, want framing", err)
	}
}

func TestHandleControlDispositions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// SnapshotRequest pre-Exit → inStream, no back frame.
	sess := newFakeSession(1)
	sess.PushOutput([]byte("x"))
	h := newTestHost(sess, nil)
	back, err := h.handleControl(ctx, attachwire.SnapshotRequest{Reason: attachwire.ReasonJoin})
	if err != nil || len(back) != 0 {
		t.Errorf("pre-Exit snapshot_request: back=%v err=%v, want nil,nil", back, err)
	}

	// SnapshotRequest post-Exit → a directly-transmitted post-Exit snapshot.
	sess.PushExit(0)
	back, err = h.handleControl(ctx, attachwire.SnapshotRequest{Reason: attachwire.ReasonJoin})
	if err != nil || len(back) != 1 || back[0].Type != attachwire.TypeSnapshot || back[0].Seq != 0 {
		t.Errorf("post-Exit snapshot_request: back=%v err=%v, want one seq-0 Snapshot", back, err)
	}

	// Kill → hook invoked with reason+signal.
	var reason, signal atomic.Value
	hk := newTestHost(newFakeSession(1), func(_ context.Context, r, s string) error {
		reason.Store(r)
		signal.Store(s)
		return nil
	})
	sig := "SIGKILL"
	if _, err := hk.handleControl(ctx, attachwire.Kill{Reason: attachwire.KillRevoked, Signal: &sig}); err != nil {
		t.Fatalf("kill control: %v", err)
	}
	if r, _ := reason.Load().(string); r != "revoked" {
		t.Errorf("kill reason = %q, want revoked", r)
	}
	if s, _ := signal.Load().(string); s != "SIGKILL" {
		t.Errorf("kill signal = %q, want SIGKILL", s)
	}

	// error control: epoch-stale → ErrEpochStale.
	if _, err := h.handleControl(ctx, attachwire.ControlError{Code: attachwire.CodeEpochStale}); !errors.Is(err, ErrEpochStale) {
		t.Errorf("epoch-stale control err = %v, want ErrEpochStale", err)
	}
	// non-retryable → *RelayStopError (and Error()/isRelayStop cover).
	_, err = h.handleControl(ctx, attachwire.ControlError{Code: attachwire.CodeAuth, Message: "no", Retryable: false})
	if !isRelayStop(err) {
		t.Fatalf("non-retryable control err = %v, want *RelayStopError", err)
	}
	if err.Error() == "" {
		t.Error("RelayStopError.Error() is empty")
	}
	// retryable → ignored (nil).
	if _, err := h.handleControl(ctx, attachwire.ControlError{Code: attachwire.CodeInternal, Retryable: true}); err != nil {
		t.Errorf("retryable control err = %v, want nil", err)
	}
	// error control: ring-miss → *RelayRingMissError, RESET-AND-RETRY, NEVER
	// *RelayStopError — §13 makes this the designed relay-restart repair path,
	// regardless of what the wire's retryable bit says.
	for _, retryable := range []bool{false, true} {
		_, err := h.handleControl(ctx, attachwire.ControlError{Code: attachwire.CodeRingMiss, Message: "ring evicted", Retryable: retryable})
		if !isRelayRingMiss(err) {
			t.Fatalf("ring-miss control (retryable=%v) err = %v, want *RelayRingMissError", retryable, err)
		}
		if isRelayStop(err) {
			t.Errorf("ring-miss control (retryable=%v) classified as *RelayStopError — must never be terminal", retryable)
		}
		if err.Error() == "" {
			t.Error("RelayRingMissError.Error() is empty")
		}
	}
	// A known control not addressed to the host (§ 6.3) → ignored.
	if _, err := h.handleControl(ctx, attachwire.Presence{Op: attachwire.PresenceList}); err != nil {
		t.Errorf("presence control err = %v, want nil (ignored)", err)
	}
}

func TestApplyControlUnknownAndMalformed(t *testing.T) {
	t.Parallel()
	h := newTestHost(newFakeSession(1), nil)
	ctx := context.Background()

	// Unknown control type → soft ignore.
	unknown := attachwire.EncodeControlPayload([]byte(`{"type":"bogus-future-thing"}`))
	if back, err := h.applyControl(ctx, unknown); err != nil || back != nil {
		t.Errorf("unknown control: back=%v err=%v, want nil,nil", back, err)
	}
	// Malformed control JSON → tolerated (ignored).
	bad := attachwire.EncodeControlPayload([]byte(`}{not json`))
	if back, err := h.applyControl(ctx, bad); err != nil || back != nil {
		t.Errorf("malformed control: back=%v err=%v, want nil,nil", back, err)
	}
	// Malformed control payload envelope → framing error.
	if _, err := h.applyControl(ctx, []byte{0xFF}); !attachwire.IsFramingErr(err) {
		t.Errorf("malformed control payload err = %v, want framing", err)
	}
}

func TestInvokeKillIdempotentAndNilHook(t *testing.T) {
	t.Parallel()
	// nil hook → no panic, no-op.
	h := newTestHost(newFakeSession(1), nil)
	h.invokeKill(context.Background(), "stopped", "")

	var n atomic.Int64
	h2 := newTestHost(newFakeSession(1), func(context.Context, string, string) error {
		n.Add(1)
		return nil
	})
	h2.invokeKill(context.Background(), "stopped", "")
	h2.invokeKill(context.Background(), "stopped", "") // second call is a no-op
	if n.Load() != 1 {
		t.Errorf("kill hook invoked %d times, want 1", n.Load())
	}
}
