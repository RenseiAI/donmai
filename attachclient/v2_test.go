package attachclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

func v2TestToken(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"sessionId": "session-v2", "roomId": "session-v2", "role": "host",
		"epoch": 3, "carrier_epoch": 9,
		"handoff_nonce":               base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"prepared_correlation_digest": strings.Repeat("a", 64),
		"protocol":                    attachwirev2.ProtocolVersion,
		"orgId":                       "org-v2",
		"iat":                         time.Now().Add(-time.Minute).Unix(),
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"aud":                         "relay",
		"jti":                         "123e4567-e89b-42d3-a456-426614174000",
	}
	if mutate != nil {
		mutate(claims)
	}
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
}

func readV2TestFrame(ctx context.Context, conn *websocket.Conn) (attachwire.Frame, []byte, error) {
	kind, raw, err := conn.Read(ctx)
	if err != nil {
		return attachwire.Frame{}, nil, err
	}
	if kind != websocket.MessageBinary {
		return attachwire.Frame{}, nil, fmt.Errorf("message kind %v is not binary", kind)
	}
	frame, err := attachwire.DecodeFrame(raw)
	return frame, raw, err
}

func v2ControlFromFrame(frame attachwire.Frame) (attachwire.ControlMessage, error) {
	payload, err := attachwire.DecodeControlPayload(frame.Payload)
	if err != nil {
		return nil, err
	}
	return attachwirev2.DecodeControl(payload)
}

func TestV2CandidateActivationAndDurableAckOrdering(t *testing.T) {
	outputSeen := make(chan struct{})
	allowOutputAck := make(chan struct{})
	retryFirstSeen := make(chan struct{})
	serverErr := make(chan error, 1)
	candidateSnapshot := attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: 5, Payload: []byte{0, 1, 2, 0xff}}
	output := attachwire.Frame{Type: attachwire.TypeOutput, Seq: 6, Payload: []byte{0, 0xff, '\r', '\n'}}
	recoverySnapshot := attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: 9, Payload: []byte{9, 0, 0xff}}
	retryOutput := attachwire.Frame{Type: attachwire.TypeOutput, Seq: 10, Payload: []byte("retry-exact")}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		frame, _, err := readV2TestFrame(ctx, conn)
		if err != nil {
			serverErr <- err
			return
		}
		message, err := v2ControlFromFrame(frame)
		if err != nil || message.ControlType() != attachwire.CtrlSubscribe {
			serverErr <- fmt.Errorf("first frame = %T/%v", message, err)
			return
		}
		request, _ := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonResync})
		if err := conn.Write(ctx, websocket.MessageBinary, request.Encode()); err != nil {
			serverErr <- err
			return
		}
		_, raw, err := readV2TestFrame(ctx, conn)
		if err != nil || string(raw) != string(candidateSnapshot.Encode()) {
			serverErr <- fmt.Errorf("candidate Snapshot raw mismatch: %v", err)
			return
		}
		frame, _, err = readV2TestFrame(ctx, conn)
		message, controlErr := v2ControlFromFrame(frame)
		if err != nil || controlErr != nil || message.ControlType() != attachwirev2.CtrlCarrierActivate {
			serverErr <- fmt.Errorf("activation frame = %T/%v/%v", message, err, controlErr)
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			serverErr <- err
			return
		}
		_, raw, err = readV2TestFrame(ctx, conn)
		if err != nil || string(raw) != string(output.Encode()) {
			serverErr <- fmt.Errorf("output raw mismatch: %v", err)
			return
		}
		close(outputSeen)
		<-allowOutputAck
		ack, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 6})
		if err := conn.Write(ctx, websocket.MessageBinary, ack.Encode()); err != nil {
			serverErr <- err
			return
		}
		frame, _, err = readV2TestFrame(ctx, conn)
		message, controlErr = v2ControlFromFrame(frame)
		gap, gapOK := message.(attachwirev2.HostGap)
		if err != nil || controlErr != nil || !gapOK || gap.FromSeq != 7 || gap.ToSeq != 8 {
			serverErr <- fmt.Errorf("gap frame = %#v/%v/%v", message, err, controlErr)
			return
		}
		_, raw, err = readV2TestFrame(ctx, conn)
		if err != nil || string(raw) != string(recoverySnapshot.Encode()) {
			serverErr <- fmt.Errorf("recovery Snapshot raw mismatch: %v", err)
			return
		}
		gapAck, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 9})
		if err := conn.Write(ctx, websocket.MessageBinary, gapAck.Encode()); err != nil {
			serverErr <- err
			return
		}
		_, raw, err = readV2TestFrame(ctx, conn)
		if err != nil || !bytes.Equal(raw, retryOutput.Encode()) {
			serverErr <- fmt.Errorf("first retry output mismatch: %v", err)
			return
		}
		close(retryFirstSeen)
		_, raw, err = readV2TestFrame(ctx, conn)
		if err != nil || !bytes.Equal(raw, retryOutput.Encode()) {
			serverErr <- fmt.Errorf("exact replay output mismatch: %v", err)
			return
		}
		retryAck, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10})
		if err := conn.Write(ctx, websocket.MessageBinary, retryAck.Encode()); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		DurableHighWater: 4,
	})
	if err != nil {
		t.Fatalf("DialV2HostCandidate: %v", err)
	}
	defer candidate.Close() //nolint:errcheck
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err != nil {
		t.Fatalf("WaitMandatorySnapshotRequest: %v", err)
	}
	if err := candidate.SendCandidateSnapshot(ctx, candidateSnapshot.Encode()); err != nil {
		t.Fatalf("SendCandidateSnapshot: %v", err)
	}
	if err := candidate.SendRawFrameDurable(ctx, output.Encode()); err == nil {
		t.Fatal("durable event succeeded before carrier_active")
	}
	if ack, err := candidate.Activate(ctx); err != nil || ack != 5 {
		t.Fatalf("Activate = %d, %v; want ack 5", ack, err)
	}
	durableDone := make(chan error, 1)
	go func() { durableDone <- candidate.OnSessionEventDurable(ctx, output.Encode()) }()
	select {
	case <-outputSeen:
	case <-ctx.Done():
		t.Fatal("relay did not receive output")
	}
	select {
	case err := <-durableDone:
		t.Fatalf("durable callback returned before host_ack: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowOutputAck)
	if err := <-durableDone; err != nil {
		t.Fatalf("durable callback after host_ack: %v", err)
	}
	if err := candidate.DeclareHostGap(ctx, 8, 9); err == nil {
		t.Fatal("host gap not contiguous with ackSeq+1 was accepted")
	}
	if err := candidate.DeclareHostGap(ctx, 7, 8); err != nil {
		t.Fatalf("DeclareHostGap: %v", err)
	}
	if err := candidate.SendRawFrameDurable(ctx, recoverySnapshot.Encode()); err != nil {
		t.Fatalf("durable recovery Snapshot: %v", err)
	}
	shortCtx, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := candidate.SendRawFrameDurable(shortCtx, retryOutput.Encode()); !errors.Is(err, context.DeadlineExceeded) {
		shortCancel()
		t.Fatalf("unacknowledged send = %v, want deadline", err)
	}
	shortCancel()
	select {
	case <-retryFirstSeen:
	case <-ctx.Done():
		t.Fatal("relay did not receive first unacknowledged frame")
	}
	changedRetry := retryOutput
	changedRetry.Payload = []byte("changed")
	if err := candidate.SendRawFrameDurable(ctx, changedRetry.Encode()); err == nil {
		t.Fatal("changed raw bytes for a pending sequence were accepted")
	}
	if err := candidate.SendRawFrameDurable(ctx, retryOutput.Encode()); err != nil {
		t.Fatalf("exact pending replay: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestV2HostAckCannotAdvanceBeyondSentContiguousFrames(t *testing.T) {
	candidate := &V2HostCandidate{
		claims: v2HostClaims{Epoch: 3, CarrierEpoch: 9},
		active: true, ackSeq: 6, highestSent: 6, notify: make(chan struct{}), closedCh: make(chan struct{}),
	}
	if err := candidate.acceptHostAck(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 7}); err == nil {
		t.Fatal("future host_ack advanced beyond the exact sent stream")
	}
	if candidate.ackSeq != 6 {
		t.Fatalf("future host_ack mutated cursor to %d", candidate.ackSeq)
	}
}

func TestV2HostClaimsAreExactAndIndependentFromV1(t *testing.T) {
	now := time.Now()
	if _, err := parseV2HostClaims(v2TestToken(t, nil), now); err != nil {
		t.Fatalf("parse exact v2 claims: %v", err)
	}
	cases := []func(map[string]any){
		func(claims map[string]any) { claims["protocol"] = attachwire.ProtocolVersion },
		func(claims map[string]any) { claims["userId"] = "not-host-only" },
		func(claims map[string]any) { delete(claims, "carrier_epoch") },
		func(claims map[string]any) { claims["carrier_epoch"] = 0 },
	}
	for _, mutate := range cases {
		if _, err := parseV2HostClaims(v2TestToken(t, mutate), now); err == nil {
			t.Fatal("invalid v2 claim set was accepted")
		}
	}
}

func TestV2RefusalsDoNotExposeCredentialsOrRawFrames(t *testing.T) {
	const correlationSecret = "fixture-correlation-secret-never-report"
	token := v2TestToken(t, func(claims map[string]any) {
		claims["handoff_nonce"] = correlationSecret
		claims["jti"] = correlationSecret
	})
	_, err := parseV2HostClaims(token, time.Now())
	if err == nil {
		t.Fatal("malformed correlation claims were accepted")
	}
	if strings.Contains(err.Error(), correlationSecret) || strings.Contains(err.Error(), token) {
		t.Fatalf("credential material reached refusal: %v", err)
	}

	const rawSecret = "fixture-raw-output-never-report"
	candidate := &V2HostCandidate{}
	err = candidate.SendRawFrameDurable(context.Background(), (attachwire.Frame{
		Type: attachwire.TypeOutput, Seq: 1, Payload: []byte(rawSecret),
	}).Encode())
	if err == nil || strings.Contains(err.Error(), rawSecret) {
		t.Fatalf("raw frame material reached refusal: %v", err)
	}
}
