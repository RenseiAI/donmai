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
		"handoff_nonce":                    base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"prepared_correlation_digest":      strings.Repeat("a", 64),
		"proof_schema_version":             "2",
		"store_authority_id":               "store-v2-test",
		"proof_revision":                   "1",
		"proof_digest":                     strings.Repeat("b", 64),
		"carrier_boundary":                 "4",
		"resolved_boundary":                "4",
		"last_host_seq":                    "4",
		"reservation_request_id":           "223e4567-e89b-42d3-a456-426614174000",
		"reservation_request_digest":       strings.Repeat("c", 64),
		"reserved_candidate_carrier_epoch": "9",
		"carrier_epoch_floor":              "9",
		"predecessor_abandonment":          nil,
		"protocol":                         attachwirev2.ProtocolVersion,
		"orgId":                            "org-v2",
		"iat":                              time.Now().Add(-time.Minute).Unix(),
		"exp":                              time.Now().Add(time.Hour).Unix(),
		"aud":                              "relay",
		"jti":                              "123e4567-e89b-42d3-a456-426614174000",
	}
	if mutate != nil {
		mutate(claims)
	}
	header, _ := json.Marshal(map[string]any{"alg": "EdDSA", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
}

func v2TestProofV1Token(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	return v2TestToken(t, func(claims map[string]any) {
		delete(claims, "proof_schema_version")
		delete(claims, "carrier_epoch_floor")
		delete(claims, "predecessor_abandonment")
		if mutate != nil {
			mutate(claims)
		}
	})
}

func v2TokenWithRawPayload(t *testing.T, token string, payload []byte) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("test token is malformed")
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
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

func v2ResumeSnapshot(sequence uint64) attachwire.Frame {
	return attachwire.Frame{
		Type: attachwire.TypeSnapshot, Seq: sequence,
		Payload: (attachwire.SnapshotEnvelope{
			AtSeq: sequence - 1, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte{1, 2, 3},
		}).Encode(),
	}
}

func TestV2CandidateActivationAndDurableAckOrdering(t *testing.T) {
	outputSeen := make(chan struct{})
	allowOutputAck := make(chan struct{})
	retryFirstSeen := make(chan struct{})
	serverErr := make(chan error, 1)
	candidateSnapshot := v2ResumeSnapshot(5)
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
		// Keep the first activation in flight long enough for the second caller to
		// join it. An implementation that emits once per caller queues a duplicate
		// here, which the next exact raw-frame assertion rejects.
		time.Sleep(50 * time.Millisecond)
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
	type activationResult struct {
		ack uint64
		err error
	}
	activationStart := make(chan struct{})
	activationDone := make(chan activationResult, 2)
	for range 2 {
		go func() {
			<-activationStart
			ack, activateErr := candidate.Activate(ctx)
			activationDone <- activationResult{ack: ack, err: activateErr}
		}()
	}
	close(activationStart)
	for range 2 {
		result := <-activationDone
		if result.err != nil || result.ack != 5 {
			t.Fatalf("concurrent Activate = %d, %v; want shared ack 5", result.ack, result.err)
		}
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
	if err := candidate.DeclareHostGap(ctx, 10, 11); err == nil {
		t.Fatal("host gap overtook an unacknowledged exact frame")
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

func TestV2ActiveResumeAcceptsImmediateExactCarrierActiveWithoutSnapshot(t *testing.T) {
	serverErr := make(chan error, 1)
	framesSent := make(chan struct{})
	inputSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		frame, _, err := readV2TestFrame(ctx, conn)
		message, controlErr := v2ControlFromFrame(frame)
		if err != nil || controlErr != nil || message.ControlType() != attachwire.CtrlSubscribe {
			serverErr <- fmt.Errorf("active resume first frame = %T/%v/%v", message, err, controlErr)
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{
			PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
		})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			serverErr <- err
			return
		}
		input := attachwire.Frame{Type: attachwire.TypeInput, Payload: (attachwire.InputPayload{
			InputSeq: 1, UserID: []byte("viewer"), Data: []byte("queued-before-local-publication"),
		}).Encode()}
		if err := conn.Write(ctx, websocket.MessageBinary, input.Encode()); err != nil {
			serverErr <- err
			return
		}
		close(framesSent)
		quietCtx, quietCancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer quietCancel()
		if _, _, err := readV2TestFrame(quietCtx, conn); err == nil {
			serverErr <- errors.New("active resume emitted an unexpected Snapshot or carrier_activate")
			return
		}
		serverErr <- nil
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:   strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource: func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		ResumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
		},
		OnInput: func(context.Context, attachwire.InputPayload) error {
			inputSeen <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close() //nolint:errcheck
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err == nil {
		t.Fatal("active resume waited for a duplicate mandatory Snapshot")
	}
	<-framesSent
	select {
	case <-inputSeen:
		t.Fatal("queued active-resume Input crossed before local publication")
	case <-time.After(50 * time.Millisecond):
	}
	if ack, err := candidate.Activate(ctx); err != nil || ack != 12 {
		t.Fatalf("active resume = %d, %v", ack, err)
	}
	select {
	case <-inputSeen:
	case <-ctx.Done():
		t.Fatal("queued active-resume Input did not drain after local publication")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestV2ActiveResumeKeepsAuthorityClosedUntilLocalPublication(t *testing.T) {
	var inputCalls int
	candidate := &V2HostCandidate{
		cfg: V2HostConfig{OnInput: func(context.Context, attachwire.InputPayload) error {
			inputCalls++
			return nil
		}},
		claims: v2HostClaims{Epoch: 3, CarrierEpoch: 9}, ackSeq: 12, highestSent: 12,
		resumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
		},
		notify: make(chan struct{}), closedCh: make(chan struct{}), localActiveCh: make(chan struct{}),
	}
	if err := candidate.acceptCarrierActive(attachwirev2.CarrierActive{
		PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if !candidate.remoteActive || candidate.active {
		t.Fatalf("remote/local activation = %v/%v, want true/false", candidate.remoteActive, candidate.active)
	}
	ackVersion := candidate.ackVersion
	input := attachwire.Frame{Type: attachwire.TypeInput, Payload: (attachwire.InputPayload{
		InputSeq: 1, UserID: []byte("viewer"), Data: []byte("blocked"),
	}).Encode()}
	inputDone := make(chan error, 1)
	go func() { inputDone <- candidate.handleV2Inbound(context.Background(), input) }()
	select {
	case err := <-inputDone:
		t.Fatalf("active-resume Input did not wait for local publication: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if inputCalls != 0 || candidate.ackVersion != ackVersion {
		t.Fatalf("pre-publication Input effects = callbacks:%d ackVersion:%d->%d", inputCalls, ackVersion, candidate.ackVersion)
	}
	if ack, err := candidate.Activate(context.Background()); err != nil || ack != 12 {
		t.Fatalf("local publication release = %d, %v", ack, err)
	}
	if err := <-inputDone; err != nil || inputCalls != 1 {
		t.Fatalf("post-publication Input = calls:%d err:%v", inputCalls, err)
	}
}

func TestV2ReceiptStoredResumeActivatesExactPendingSnapshotWithoutResend(t *testing.T) {
	snapshot := v2ResumeSnapshot(12)
	carrierActiveConsumed := make(chan struct{})
	releaseServer := make(chan struct{})
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		frame, _, err := readV2TestFrame(ctx, conn)
		message, controlErr := v2ControlFromFrame(frame)
		if err != nil || controlErr != nil || message.ControlType() != attachwire.CtrlSubscribe {
			serverErr <- fmt.Errorf("pending resume first frame = %T/%v/%v", message, err, controlErr)
			return
		}
		frame, _, err = readV2TestFrame(ctx, conn)
		message, controlErr = v2ControlFromFrame(frame)
		if err != nil || controlErr != nil || message.ControlType() != attachwirev2.CtrlCarrierActivate {
			serverErr <- fmt.Errorf("pending resume emitted duplicate Snapshot before activation: %s/%T/%v/%v", frame.Type, message, err, controlErr)
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{
			PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
		})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			serverErr <- err
			return
		}
		// Keep the leg open until Activate has consumed the exact acknowledgement.
		// Closing immediately after Write races the client's read loop and turns a
		// successful carrier_active into an EOF.
		select {
		case <-carrierActiveConsumed:
		case <-releaseServer:
			serverErr <- errors.New("client did not consume exact carrier_active")
			return
		}
		serverErr <- nil
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseServer) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL: strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource: func(context.Context) (string, error) {
			return v2TestToken(t, func(claims map[string]any) {
				claims["carrier_boundary"] = "10"
				claims["resolved_boundary"] = "11"
				claims["last_host_seq"] = "11"
			}), nil
		},
		ResumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10,
			CandidateSnapshotSeq: 12, CandidateSnapshot: snapshot.Encode(),
			GapFromSeq: 11, GapToSeq: 11, GapReason: attachwirev2.GapControllerUnforwarded,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close() //nolint:errcheck
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err == nil {
		t.Fatal("receipt-stored resume waited for a duplicate mandatory Snapshot")
	}
	if err := candidate.SendCandidateSnapshot(ctx, snapshot.Encode()); err == nil {
		t.Fatal("receipt-stored resume resent its staged Snapshot")
	}
	ack, err := candidate.Activate(ctx)
	if err != nil {
		t.Fatalf("receipt-stored resume = %d, %v", ack, err)
	}
	close(carrierActiveConsumed)
	if ack != 12 {
		t.Fatalf("receipt-stored resume acknowledgement = %d, want 12", ack)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestV2ReceiptStoredResumeRequiresExactCarrierActive(t *testing.T) {
	snapshot := v2ResumeSnapshot(12)
	for _, test := range []struct {
		name   string
		active *attachwirev2.CarrierActive
	}{
		{name: "omitted"},
		{name: "wrong cursor", active: &attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 11}},
	} {
		t.Run(test.name, func(t *testing.T) {
			carrierActiveRejected := make(chan struct{})
			releaseServer := make(chan struct{})
			serverErr := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
				if err != nil {
					serverErr <- err
					return
				}
				defer conn.CloseNow() //nolint:errcheck
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				frame, _, err := readV2TestFrame(ctx, conn)
				message, controlErr := v2ControlFromFrame(frame)
				if err != nil || controlErr != nil || message.ControlType() != attachwire.CtrlSubscribe {
					serverErr <- fmt.Errorf("pending resume first frame = %T/%v/%v", message, err, controlErr)
					return
				}
				frame, _, err = readV2TestFrame(ctx, conn)
				message, controlErr = v2ControlFromFrame(frame)
				if err != nil || controlErr != nil || message.ControlType() != attachwirev2.CtrlCarrierActivate {
					serverErr <- fmt.Errorf("pending resume activation frame = %T/%v/%v", message, err, controlErr)
					return
				}
				if test.active != nil {
					active, err := attachwirev2.BuildControlFrame(*test.active)
					if err != nil {
						serverErr <- err
						return
					}
					if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
						serverErr <- err
						return
					}
					select {
					case <-carrierActiveRejected:
					case <-releaseServer:
						serverErr <- errors.New("client did not reject wrong carrier_active")
						return
					}
				}
				serverErr <- nil
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(releaseServer) })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
				AttachURL: strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
				TokenSource: func(context.Context) (string, error) {
					return v2TestToken(t, func(claims map[string]any) {
						claims["carrier_boundary"] = "10"
						claims["resolved_boundary"] = "11"
						claims["last_host_seq"] = "11"
					}), nil
				},
				ResumeDisposition: &V2ResumeDisposition{
					ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
					State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10,
					CandidateSnapshotSeq: 12, CandidateSnapshot: snapshot.Encode(),
					GapFromSeq: 11, GapToSeq: 11, GapReason: attachwirev2.GapControllerUnforwarded,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.Close() //nolint:errcheck
			ack, err := candidate.Activate(ctx)
			if err == nil {
				t.Fatalf("receipt-stored resume accepted %s carrier_active with acknowledgement %d", test.name, ack)
			}
			if test.active != nil {
				if !strings.Contains(err.Error(), "carrier_active cursor is outside the exact sent stream") {
					t.Fatalf("wrong carrier_active error = %v", err)
				}
				close(carrierActiveRejected)
			}
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestV2ResumeDispositionRejectsMismatchedFrameCarrierAndAck(t *testing.T) {
	snapshot := v2ResumeSnapshot(12)
	base := V2HostConfig{
		AttachURL: "ws://example.invalid/v2/rooms/session-v2",
		TokenSource: func(context.Context) (string, error) {
			return "unused", nil
		},
		ResumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10,
			CandidateSnapshotSeq: 12, CandidateSnapshot: snapshot.Encode(),
		},
	}
	for name, mutate := range map[string]func(*V2HostConfig){
		"frame sequence": func(config *V2HostConfig) {
			config.ResumeDisposition.CandidateSnapshotSeq++
		},
		"frame correlation": func(config *V2HostConfig) {
			changed := v2ResumeSnapshot(12)
			envelope, _ := attachwire.DecodeSnapshotEnvelope(changed.Payload)
			envelope.AtSeq--
			changed.Payload = envelope.Encode()
			config.ResumeDisposition.CandidateSnapshot = changed.Encode()
		},
		"ack cursor": func(config *V2HostConfig) { config.DurableHighWater = 9 },
	} {
		config := base
		resume := cloneV2ResumeDisposition(*base.ResumeDisposition)
		config.ResumeDisposition = &resume
		mutate(&config)
		if err := config.withDefaults(); err == nil {
			t.Errorf("%s mismatch was accepted", name)
		}
	}
	wrongCarrier := base
	resume := cloneV2ResumeDisposition(*base.ResumeDisposition)
	resume.PTYEpoch++
	wrongCarrier.ResumeDisposition = &resume
	wrongCarrier.TokenSource = func(context.Context) (string, error) { return v2TestToken(t, nil), nil }
	if _, err := DialV2HostCandidate(context.Background(), wrongCarrier); err == nil {
		t.Fatal("resume disposition from another authenticated PTY/carrier was accepted")
	}

	active := &V2HostCandidate{
		claims: v2HostClaims{Epoch: 3, CarrierEpoch: 9}, ackSeq: 12, highestSent: 12,
		resumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 12,
		},
		notify: make(chan struct{}), closedCh: make(chan struct{}),
	}
	if err := active.acceptCarrierActive(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 11}); err == nil || active.active {
		t.Fatal("active resume accepted a changed journal high-water")
	}
	pending := &V2HostCandidate{
		claims: v2HostClaims{Epoch: 3, CarrierEpoch: 9}, ackSeq: 10, highestSent: 12,
		candidateSent: true, pendingSeq: 12, pendingRaw: snapshot.Encode(),
		resumeDisposition: &V2ResumeDisposition{
			ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeSameHandoff,
			State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10, CandidateSnapshotSeq: 12,
		},
		notify: make(chan struct{}), closedCh: make(chan struct{}),
	}
	if err := pending.acceptCarrierActive(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 10}); err == nil || pending.active {
		t.Fatal("receipt-stored resume accepted an ack that did not cover its staged Snapshot")
	}
	duplicateRequest, err := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonResync})
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.handleV2Inbound(context.Background(), duplicateRequest); err == nil {
		t.Fatal("receipt-stored resume accepted a duplicate mandatory Snapshot request")
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
		func(claims map[string]any) { delete(claims, "proof_digest") },
		func(claims map[string]any) { claims["proof_revision"] = 1 },
		func(claims map[string]any) { claims["proof_revision"] = "0" },
		func(claims map[string]any) { claims["carrier_boundary"] = "5" },
		func(claims map[string]any) { claims["resolved_boundary"] = "5" },
		func(claims map[string]any) { claims["last_host_seq"] = "5" },
		func(claims map[string]any) { claims["reserved_candidate_carrier_epoch"] = "10" },
		func(claims map[string]any) { claims["reservation_request_digest"] = strings.Repeat("A", 64) },
		func(claims map[string]any) { claims["proof_schema_version"] = 2 },
		func(claims map[string]any) { claims["proof_schema_version"] = "02" },
		func(claims map[string]any) { delete(claims, "carrier_epoch_floor") },
		func(claims map[string]any) { claims["carrier_epoch_floor"] = 9 },
		func(claims map[string]any) { claims["carrier_epoch_floor"] = "09" },
		func(claims map[string]any) { claims["carrier_epoch_floor"] = "10" },
		func(claims map[string]any) { delete(claims, "predecessor_abandonment") },
		func(claims map[string]any) { claims["predecessor_abandonment"] = 1 },
	}
	for _, mutate := range cases {
		if _, err := parseV2HostClaims(v2TestToken(t, mutate), now); err == nil {
			t.Fatal("invalid v2 claim set was accepted")
		}
	}
}

func TestV2ProofSchemaAndPredecessorAreStrict(t *testing.T) {
	now := time.Now()
	valid := map[string]any{
		"target_reservation_request_id":     "323e4567-e89b-42d3-a456-426614174000",
		"target_reservation_request_digest": strings.Repeat("d", 64),
		"source_candidate_state":            "receipt_stored",
		"abandonment_request_id":            "423e4567-e89b-42d3-a456-426614174000",
		"abandonment_request_digest":        strings.Repeat("e", 64),
		"abandonment_revision":              "2",
		"abandonment_digest":                strings.Repeat("f", 64),
		"abandoned_candidate_carrier_epoch": "8",
	}
	if claims, err := parseV2HostClaims(v2TestToken(t, func(claims map[string]any) {
		claims["predecessor_abandonment"] = valid
	}), now); err != nil || claims.PredecessorAbandonment == nil || claims.CarrierEpochFloor != 9 {
		t.Fatalf("parse exact proof-v2 predecessor: claims=%+v err=%v", claims, err)
	}

	mutations := map[string]func(map[string]any){
		"unknown":               func(value map[string]any) { value["unknown"] = true },
		"omitted":               func(value map[string]any) { delete(value, "abandonment_digest") },
		"partial":               func(value map[string]any) { delete(value, "target_reservation_request_id") },
		"numeric revision":      func(value map[string]any) { value["abandonment_revision"] = 2 },
		"noncanonical revision": func(value map[string]any) { value["abandonment_revision"] = "02" },
		"unknown state":         func(value map[string]any) { value["source_candidate_state"] = "active" },
		"uppercase digest":      func(value map[string]any) { value["abandonment_digest"] = strings.Repeat("F", 64) },
		"noncanonical uuid":     func(value map[string]any) { value["abandonment_request_id"] = "423E4567-E89B-42D3-A456-426614174000" },
		"non-lower epoch":       func(value map[string]any) { value["abandoned_candidate_carrier_epoch"] = "9" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			predecessor := make(map[string]any, len(valid))
			for key, value := range valid {
				predecessor[key] = value
			}
			mutate(predecessor)
			if _, err := parseV2HostClaims(v2TestToken(t, func(claims map[string]any) {
				claims["predecessor_abandonment"] = predecessor
			}), now); err == nil {
				t.Fatal("invalid proof-v2 predecessor was accepted")
			}
		})
	}
}

func TestV2ProofSchemaRejectsDuplicateTopLevelAndPredecessorMembers(t *testing.T) {
	now := time.Now()
	token := v2TestToken(t, nil)
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	duplicateTop := bytes.Replace(payload,
		[]byte(`"proof_schema_version":"2"`),
		[]byte(`"proof_schema_version":"2","proof_schema_version":"2"`), 1)
	if _, err := parseV2HostClaims(v2TokenWithRawPayload(t, token, duplicateTop), now); err == nil {
		t.Fatal("duplicate proof_schema_version was accepted")
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["predecessor_abandonment"] = json.RawMessage(`{
		"target_reservation_request_id":"323e4567-e89b-42d3-a456-426614174000",
		"target_reservation_request_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"source_candidate_state":"preparing",
		"abandonment_request_id":"423e4567-e89b-42d3-a456-426614174000",
		"abandonment_request_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		"abandonment_revision":"2",
		"abandonment_revision":"2",
		"abandonment_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"abandoned_candidate_carrier_epoch":"8"}`)
	duplicateNested, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseV2HostClaims(v2TokenWithRawPayload(t, token, duplicateNested), now); err == nil {
		t.Fatal("duplicate predecessor member was accepted")
	}
}

func TestV2ProofV1IsRetainedSameHandoffOnly(t *testing.T) {
	claims, err := parseV2HostClaims(v2TestProofV1Token(t, nil), time.Now())
	if err != nil || claims.ProofSchemaVersion != V2ProofSchemaV1 {
		t.Fatalf("parse retained proof-v1: claims=%+v err=%v", claims, err)
	}
	if err := validateV2ProofDisposition(claims, V2HostConfig{DurableHighWater: 4}); err == nil {
		t.Fatal("fresh admission accepted frozen proof-v1")
	}
	snapshot := v2ResumeSnapshot(5)
	resume := &V2ResumeDisposition{
		ProofSchemaVersion: V2ProofSchemaV1, Authority: V2ResumeSameHandoff,
		State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 4,
		CandidateSnapshotSeq: 5, CandidateSnapshot: snapshot.Encode(),
	}
	if err := validateV2ResumeDisposition(*resume); err != nil {
		t.Fatalf("retained exact same-handoff proof-v1: %v", err)
	}
	if err := validateV2ProofDisposition(claims, V2HostConfig{ResumeDisposition: resume}); err != nil {
		t.Fatalf("retained proof-v1 disposition: %v", err)
	}
	active := &V2ResumeDisposition{
		ProofSchemaVersion: V2ProofSchemaV1, Authority: V2ResumeSameHandoff,
		State: V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5,
	}
	if err := validateV2ResumeDisposition(*active); err != nil {
		t.Fatalf("retained active proof-v1 drain: %v", err)
	}
	if err := validateV2ProofDisposition(claims, V2HostConfig{ResumeDisposition: active}); err != nil {
		t.Fatalf("retained active proof-v1 disposition: %v", err)
	}
	resume.Authority = V2ResumeAdoptedCandidateRecovery
	if err := validateV2ResumeDisposition(*resume); err == nil {
		t.Fatal("proof-v1 accepted adopted-candidate recovery")
	}
}

func TestV2AdoptedCandidateRecoveryReusesReceiptStoredCandidate(t *testing.T) {
	resume := V2ResumeDisposition{
		ProofSchemaVersion: V2ProofSchemaV2, Authority: V2ResumeAdoptedCandidateRecovery,
		State: V2ResumeReceiptStored, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 4,
		CandidateSnapshotSeq: 5, CandidateSnapshot: v2ResumeSnapshot(5).Encode(),
	}
	if err := validateV2ResumeDisposition(resume); err != nil {
		t.Fatalf("adopted candidate recovery: %v", err)
	}
	resume.State = V2ResumeActive
	resume.CandidateSnapshotSeq = 0
	resume.CandidateSnapshot = nil
	resume.AckSeq = 5
	if err := validateV2ResumeDisposition(resume); err == nil {
		t.Fatal("changed-controller active carrier reused equal candidate")
	}
}

func TestV2RetainedCredentialIsExactCopiedAndRedacted(t *testing.T) {
	original := []byte("exact-original-bearer-never-log")
	credential, err := NewV2RetainedCredential(original)
	if err != nil {
		t.Fatal(err)
	}
	original[0] = 'X'
	token, err := credential.TokenSource()(context.Background())
	if err != nil || token != "exact-original-bearer-never-log" {
		t.Fatalf("retained token changed: token=%q err=%v", token, err)
	}
	for _, rendered := range []string{fmt.Sprint(credential), fmt.Sprintf("%+v", credential), fmt.Sprintf("%#v", credential)} {
		if strings.Contains(rendered, "exact-original") || !strings.Contains(rendered, "redacted") {
			t.Fatalf("credential formatting leaked or omitted redaction: %q", rendered)
		}
	}
	disposition := V2ResumeDisposition{CandidateSnapshot: []byte("retained-snapshot-never-log")}
	if rendered := fmt.Sprintf("%+v", disposition); strings.Contains(rendered, "retained-snapshot") || !strings.Contains(rendered, "redacted") {
		t.Fatalf("resume disposition formatting leaked or omitted redaction: %q", rendered)
	}
}

func TestV2ProofBoundControllerGapPrecedesExactResolvedSnapshot(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := readV2TestFrame(ctx, conn); err != nil {
			serverErr <- err
			return
		}
		request, _ := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonResync})
		if err := conn.Write(ctx, websocket.MessageBinary, request.Encode()); err != nil {
			serverErr <- err
			return
		}
		gapFrame, _, err := readV2TestFrame(ctx, conn)
		gapMessage, gapErr := v2ControlFromFrame(gapFrame)
		gap, ok := gapMessage.(attachwirev2.HostGap)
		if err != nil || gapErr != nil || !ok || gap.FromSeq != 5 || gap.ToSeq != 6 ||
			gap.Reason != attachwirev2.GapControllerUnforwarded {
			serverErr <- fmt.Errorf("proof gap = %#v, %v, %v", gapMessage, err, gapErr)
			return
		}
		_, raw, err := readV2TestFrame(ctx, conn)
		frame, decodeErr := attachwire.DecodeFrame(raw)
		envelope, envelopeErr := attachwire.DecodeSnapshotEnvelope(frame.Payload)
		if err != nil || decodeErr != nil || envelopeErr != nil || frame.Seq != 7 || envelope.AtSeq != 6 {
			serverErr <- fmt.Errorf("proof Snapshot = %+v/%+v, %v/%v/%v", frame, envelope, err, decodeErr, envelopeErr)
			return
		}
		serverErr <- nil
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL: strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource: func(context.Context) (string, error) {
			return v2TestToken(t, func(claims map[string]any) {
				claims["resolved_boundary"] = "6"
				claims["last_host_seq"] = "6"
			}), nil
		},
		DurableHighWater: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close() //nolint:errcheck
	if _, err := candidate.WaitMandatorySnapshotRequest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := candidate.DeclareHostGap(ctx, 5, 6); err == nil {
		t.Fatal("pre-active proof recovery accepted ring_evicted in place of controller_unforwarded")
	}
	if err := candidate.DeclareHostGapWithReason(ctx, 5, 6, attachwirev2.GapControllerUnforwarded); err != nil {
		t.Fatal(err)
	}
	if err := candidate.SendCandidateSnapshot(ctx, v2ResumeSnapshot(7).Encode()); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
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
