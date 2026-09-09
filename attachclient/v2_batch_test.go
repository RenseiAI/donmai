package attachclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

func batchTestCandidate(t *testing.T, serve func(context.Context, *websocket.Conn) error) (*V2HostCandidate, <-chan error) {
	t.Helper()
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck // fixture owns the connection
		if _, _, err := conn.Read(ctx); err != nil {
			result <- err
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			result <- err
			return
		}
		result <- serve(ctx, conn)
	}))
	t.Cleanup(server.Close)
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:         strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:       func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		ResumeDisposition: &V2ResumeDisposition{ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff, State: V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = candidate.Close() })
	if _, err := candidate.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	return candidate, result
}

func batchOutput(seq uint64, payload []byte) []byte {
	return (attachwire.Frame{Type: attachwire.TypeOutput, Seq: seq, Payload: payload}).Encode()
}

func TestV2OutputBatchWritesBeforeAckAndRetriesExactSuffix(t *testing.T) {
	raws := [][]byte{batchOutput(6, []byte{0, 255}), batchOutput(7, []byte("seven")), batchOutput(8, []byte("eight"))}
	firstRead := make(chan struct{})
	candidate, result := batchTestCandidate(t, func(ctx context.Context, conn *websocket.Conn) error {
		for i, expected := range raws {
			_, raw, err := readV2TestFrame(ctx, conn)
			if err != nil || !bytes.Equal(raw, expected) {
				return fmt.Errorf("initial batch frame %d bytes differ: %v", i, err)
			}
		}
		ack, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 6})
		if err := conn.Write(ctx, websocket.MessageBinary, ack.Encode()); err != nil {
			return err
		}
		close(firstRead)
		for i, expected := range raws[1:] {
			_, raw, err := readV2TestFrame(ctx, conn)
			if err != nil || !bytes.Equal(raw, expected) {
				return fmt.Errorf("retry suffix frame %d bytes differ: %v", i, err)
			}
		}
		ack, _ = attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 8})
		return conn.Write(ctx, websocket.MessageBinary, ack.Encode())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	ack, err := candidate.SendRawFramesDurable(ctx, raws)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || ack != 6 {
		t.Fatalf("partial batch = %d, %v; want confirmed6 + timeout", ack, err)
	}
	<-firstRead
	if err := candidate.SendRawFrameDurable(context.Background(), batchOutput(9, []byte("new"))); err == nil {
		t.Fatal("single frame overtook pending batch")
	}
	if err := candidate.DeclareHostGap(context.Background(), 7, 9); err == nil {
		t.Fatal("gap overtook pending batch")
	}
	if _, err := candidate.SendRawFramesDurable(context.Background(), [][]byte{batchOutput(7, []byte("changed")), raws[2]}); err == nil {
		t.Fatal("changed suffix accepted")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if ack, err := candidate.SendRawFramesDurable(ctx, raws[1:]); err != nil || ack != 8 {
		t.Fatalf("exact suffix = %d, %v", ack, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestV2OutputBatchRejectsBoundsAndBarriersBeforeWriting(t *testing.T) {
	candidate := &V2HostCandidate{active: true, ackSeq: 5, highestSent: 5}
	for _, raws := range [][][]byte{
		nil,
		make([][]byte, DurableOutputBatchMaxFrames+1),
		{batchOutput(6, make([]byte, DurableOutputBatchMaxBytes))},
		{batchOutput(6, nil), batchOutput(8, nil)},
		{(attachwire.Frame{Type: attachwire.TypeMarker, Seq: 6}).Encode()},
	} {
		if ack, err := candidate.SendRawFramesDurable(context.Background(), raws); err == nil || ack != 5 {
			t.Fatalf("invalid window accepted: ack=%d err=%v", ack, err)
		}
	}
	if candidate.highestSent != 5 || len(candidate.pendingBatch) != 0 {
		t.Fatal("rejected window changed sent state")
	}
}

func TestV2OutputBatchDropReturnsOnlyConfirmedPrefix(t *testing.T) {
	candidate, result := batchTestCandidate(t, func(ctx context.Context, conn *websocket.Conn) error {
		for range 3 {
			if _, _, err := readV2TestFrame(ctx, conn); err != nil {
				return err
			}
		}
		ack, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 6})
		if err := conn.Write(ctx, websocket.MessageBinary, ack.Encode()); err != nil {
			return err
		}
		return conn.Close(websocket.StatusGoingAway, "test drop")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ack, err := candidate.SendRawFramesDurable(ctx, [][]byte{batchOutput(6, nil), batchOutput(7, nil), batchOutput(8, nil)})
	if err == nil || ack != 6 {
		t.Fatalf("dropped batch = %d, %v; want confirmed6 + error", ack, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
