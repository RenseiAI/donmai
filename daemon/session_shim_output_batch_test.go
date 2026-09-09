package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/coder/websocket"
)

// The production pump consumes a real selected-v3+ Controller, backed by a real
// shim and PTY. The real candidate writes to a local relay fixture with 75ms
// acknowledgement latency while the source emits every33ms. Holding a batch
// callback at this boundary therefore tests the wiring as well as the API.
func TestShimOutputBatchBoundsDurableFrameAge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "dob")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // test registry cleanup
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-v2", SessionID: "session-v2"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "stty -echo; read start; i=0; while [ $i -lt 18 ]; do printf 'output-%02d\\n' \"$i\"; i=$((i+1)); sleep 0.033; done; read finish"}},
		Orphan: sessionshim.OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shim.Terminate(context.Background()); _ = shim.Close() }()
	direct, err := shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close() //nolint:errcheck // test owns subscription
	type sourceFrame struct {
		raw []byte
		at  time.Time
	}
	var mu sync.Mutex
	source := make(map[uint64]sourceFrame)
	sourceDone := make(chan struct{})
	go func() {
		defer close(sourceDone)
		for frame := range direct.Frames() {
			mu.Lock()
			source[frame.Seq] = sourceFrame{frame.Encode(), time.Now()}
			mu.Unlock()
		}
	}()
	initial, _, err := shim.Session().EmitSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{Registry: registry, ControllerID: "batch-test", RequireFullHostFrames: true, ResumeFrom: func(sessionshim.Identity) uint64 { return initial.Seq + 1 }})
	if err != nil || len(adopted.Adopted) != 1 {
		t.Fatalf("adopt = %d, %v", len(adopted.Adopted), err)
	}
	defer adopted.Close()
	ctrl := adopted.Adopted[0]

	// A reader accepts frames while a separate writer delays each ACK until75ms
	// after arrival. This models round-trip latency without inventing per-frame
	// serial service time at the fixture server.
	serverResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck // fixture owns connection
		if _, _, err := conn.Read(ctx); err != nil {
			serverResult <- err
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: attachwirev2.DecimalUint64(initial.Seq)})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			serverResult <- err
			return
		}
		type scheduledAck struct {
			seq uint64
			due time.Time
		}
		queue := make(chan scheduledAck, 64)
		writerDone := make(chan error, 1)
		go func() {
			for offer := range queue {
				timer := time.NewTimer(time.Until(offer.due))
				select {
				case <-ctx.Done():
					timer.Stop()
					writerDone <- ctx.Err()
					return
				case <-timer.C:
				}
				ack, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: attachwirev2.DecimalUint64(offer.seq)})
				if err := conn.Write(ctx, websocket.MessageBinary, ack.Encode()); err != nil {
					writerDone <- err
					return
				}
			}
			writerDone <- nil
		}()
		defer func() { close(queue) }()
		last := initial.Seq
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				serverResult <- err
				return
			}
			frame, err := attachwire.DecodeFrame(raw)
			if err != nil || frame.Seq != last+1 {
				serverResult <- fmt.Errorf("relay sequence %d after %d: %v", frame.Seq, last, err)
				return
			}
			last = frame.Seq
			mu.Lock()
			original, ok := source[frame.Seq]
			mu.Unlock()
			if !ok || !bytes.Equal(raw, original.raw) {
				serverResult <- fmt.Errorf("source bytes differ at %d", frame.Seq)
				return
			}
			queue <- scheduledAck{frame.Seq, time.Now().Add(75 * time.Millisecond)}
			if frame.Type == attachwire.TypeMarker {
				// Wait for the final scheduled ACK without closing the live socket early.
				close(queue)
				err = <-writerDone
				queue = make(chan scheduledAck)
				serverResult <- err
				<-ctx.Done()
				return
			}
		}
	}))
	defer server.Close()
	token := daemonBatchToken(t, nil)
	candidate, err := attachclient.DialV2HostCandidate(ctx, attachclient.V2HostConfig{
		AttachURL:         strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:       func(context.Context) (string, error) { return token, nil },
		ResumeDisposition: &attachclient.V2ResumeDisposition{ProofSchemaVersion: attachclient.V2ProofSchemaV2, Authority: attachclient.V2ResumeSameHandoff, State: attachclient.V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: initial.Seq},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close() //nolint:errcheck // test owns candidate
	if _, err := candidate.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	var maxAge time.Duration
	var largestBatch int
	finished := make(chan struct{})
	recordAge := func(events []sessionshim.ControllerEvent) {
		mu.Lock()
		defer mu.Unlock()
		for _, event := range events {
			if event.FrameType == attachwire.TypeOutput {
				if age := time.Since(source[event.Seq].at); age > maxAge {
					maxAge = age
				}
			}
		}
	}
	d := &Daemon{shims: newSessionShimState()}
	d.opts.SessionShim = SessionShimConfig{
		CallbackTimeout: time.Second,
		OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
			err := candidate.SendRawFrameDurable(ctx, event.FrameBytes)
			if err == nil {
				recordAge([]sessionshim.ControllerEvent{event})
				if event.FrameType == attachwire.TypeMarker {
					close(finished)
				}
			}
			return err
		},
		OnSessionEventDurableBatch: func(callCtx context.Context, _ sessionshim.Identity, events []sessionshim.ControllerEvent) (uint64, error) {
			raws := make([][]byte, len(events))
			size := 0
			for i, event := range events {
				raws[i] = event.FrameBytes
				size += len(raws[i])
			}
			if len(events) > attachclient.DurableOutputBatchMaxFrames || size > attachclient.DurableOutputBatchMaxBytes {
				return 0, fmt.Errorf("unbounded batch: %d frames/%d bytes", len(events), size)
			}
			mu.Lock()
			largestBatch = max(largestBatch, len(events))
			mu.Unlock()
			ack, err := candidate.SendRawFramesDurable(callCtx, raws)
			if err == nil {
				recordAge(events)
			}
			return ack, err
		},
	}
	d.shimIdentityRef.Store(&sessionShimIdentity{config: &d.opts.SessionShim})
	d.consumeShimEvents(ctrl)
	defer func() { _ = ctrl.Close(); d.shims.wg.Wait() }()
	if _, err := shim.Session().WriteInput([]byte("start\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "18 source output frames", func() bool {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, frame := range source {
			if bytes.Contains(frame.raw, []byte("output-")) {
				count++
			}
		}
		return count >= 18
	})
	if err := shim.Session().EmitMarker("batch-finished"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("production pump did not flush final Marker")
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	age, batchCount := maxAge, largestBatch
	mu.Unlock()
	if age > 200*time.Millisecond {
		t.Fatalf("durable maxFrameAge=%s exceeds200ms; largestBatch=%d", age, batchCount)
	}
	if batchCount < 2 {
		t.Fatal("real pump never handed multiple source frames to candidate")
	}
	t.Logf("DURABLE_OUTPUT_BATCH_GREEN maxFrameAge=%s budget=200ms largestBatch=%d", age, batchCount)
	cancel()
}

func daemonBatchToken(t *testing.T, mutate func(map[string]any)) string {
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
		"carrier_boundary":                 "0",
		"resolved_boundary":                "0",
		"last_host_seq":                    "0",
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

func TestCollectShimOutputBatchBoundsAndBarriers(t *testing.T) {
	output := func(seq uint64, size int) sessionshim.ControllerEvent {
		raw := (attachwire.Frame{Type: attachwire.TypeOutput, Seq: seq, Payload: make([]byte, size)}).Encode()
		return sessionshim.ControllerEvent{Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeOutput, Seq: seq, FrameBytes: raw}
	}
	for _, tc := range []struct {
		name              string
		size, count, want int
	}{
		{"frame limit", 1, 40, attachclient.DurableOutputBatchMaxFrames},
		{"byte limit", 100000, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan sessionshim.ControllerEvent, tc.count)
			for i := 1; i < tc.count; i++ {
				events <- output(uint64(i+1), tc.size)
			}
			close(events)
			batch, pending := collectShimOutputBatch(events, output(1, tc.size))
			if len(batch) != tc.want {
				t.Fatalf("batch length=%d want%d", len(batch), tc.want)
			}
			size := 0
			for i, event := range batch {
				size += len(event.FrameBytes)
				if event.Seq != uint64(i+1) {
					t.Fatal("batch reordered output")
				}
			}
			if size > attachclient.DurableOutputBatchMaxBytes {
				t.Fatalf("batch bytes=%d", size)
			}
			if pending == nil {
				next := <-events
				pending = &next
			}
			if pending.Seq != batch[len(batch)-1].Seq+1 {
				t.Fatal("bounded lookahead lost next frame")
			}
		})
	}
	for _, kind := range []attachwire.EventType{attachwire.TypeSnapshot, attachwire.TypeMarker, attachwire.TypeExit, attachwire.TypeResize} {
		t.Run(kind.String(), func(t *testing.T) {
			events := make(chan sessionshim.ControllerEvent, 2)
			barrier := sessionshim.ControllerEvent{Kind: sessionshim.EventHostFrame, FrameType: kind, Seq: 2}
			events <- barrier
			events <- output(3, 1)
			batch, pending := collectShimOutputBatch(events, output(1, 1))
			if len(batch) != 1 || pending == nil || pending.FrameType != kind || pending.Seq != 2 {
				t.Fatal("control barrier was crossed")
			}
			if after := <-events; after.Seq != 3 {
				t.Fatal("post-barrier output was consumed")
			}
		})
	}
	events := make(chan sessionshim.ControllerEvent, 1)
	events <- sessionshim.ControllerEvent{Kind: sessionshim.EventGap}
	batch, pending := collectShimOutputBatch(events, output(1, 1))
	if len(batch) != 1 || pending == nil || pending.Kind != sessionshim.EventGap {
		t.Fatal("gap barrier was crossed")
	}
	start := time.Now()
	batch, pending = collectShimOutputBatch(make(chan sessionshim.ControllerEvent), output(1, 1))
	if len(batch) != 1 || pending != nil || time.Since(start) > 200*time.Millisecond {
		t.Fatal("idle output did not flush on a bounded timer")
	}
}

func TestShimOutputBatchPersistsOnlyConfirmedPrefixAndReplaysSuffix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt func([]sessionshim.ControllerEvent) (uint64, error)
		want    uint64
	}{
		{"partial failure", func(batch []sessionshim.ControllerEvent) (uint64, error) {
			return batch[1].Seq, fmt.Errorf("carrier dropped")
		}, 2},
		{"unacknowledged failure", func([]sessionshim.ControllerEvent) (uint64, error) { return 0, fmt.Errorf("carrier dropped") }, 0},
		{"partial success is invalid", func(batch []sessionshim.ControllerEvent) (uint64, error) { return batch[1].Seq, nil }, 0},
		{"future receipt is invalid", func(batch []sessionshim.ControllerEvent) (uint64, error) { return batch[len(batch)-1].Seq + 1, nil }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dir, err := os.MkdirTemp("/tmp", "dbp")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir) //nolint:errcheck // test registry
			registry, err := sessionshim.NewRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			id := sessionshim.Identity{OrgID: "batch-org", SessionID: "batch-session"}
			shim, err := sessionshim.Start(sessionshim.Options{
				Identity: id, Registry: registry, ProcessEpoch: 1,
				Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 0.05; i=0; while [ $i -lt 6 ]; do printf 'frame-%d\\n' \"$i\"; i=$((i+1)); sleep 0.02; done; read finish"}},
				Orphan: sessionshim.OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 100 * time.Millisecond},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = shim.Terminate(context.Background()); _ = shim.Close() }()
			direct, err := shim.Session().Subscribe(0)
			if err != nil {
				t.Fatal(err)
			}
			defer direct.Close() //nolint:errcheck // test subscription
			var source [][]byte
			for len(source) < 6 {
				select {
				case frame := <-direct.Frames():
					source = append(source, frame.Encode())
				case <-ctx.Done():
					t.Fatal("source did not produce6 exact frames")
				}
			}
			if err := shim.Session().EmitMarker("barrier"); err != nil {
				t.Fatal(err)
			}
			result, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{Registry: registry, ControllerID: "batch-first", RequireFullHostFrames: true})
			if err != nil || len(result.Adopted) != 1 {
				t.Fatalf("adopt=%+v %v", result, err)
			}
			defer result.Close()
			ctrl := result.Adopted[0]
			reached := make(chan struct{})
			release := make(chan struct{})
			callbackError := make(chan error, 1)
			d := &Daemon{shims: newSessionShimState()}
			d.opts.SessionShim = SessionShimConfig{
				CallbackTimeout: time.Second,
				OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
					return fmt.Errorf("barrier or serial frame overtook batch: %d", event.Seq)
				},
				OnSessionEventDurableBatch: func(_ context.Context, _ sessionshim.Identity, batch []sessionshim.ControllerEvent) (uint64, error) {
					if len(batch) != 6 {
						callbackError <- fmt.Errorf("batch has%d frames, want6", len(batch))
						close(reached)
						<-release
						return 0, fmt.Errorf("bad batch")
					}
					for i, event := range batch {
						if !bytes.Equal(event.FrameBytes, source[i]) {
							callbackError <- fmt.Errorf("batch bytes changed at%d", i)
							close(reached)
							<-release
							return 0, fmt.Errorf("bad bytes")
						}
					}
					close(reached)
					<-release
					return tc.receipt(batch)
				},
			}
			d.shimIdentityRef.Store(&sessionShimIdentity{config: &d.opts.SessionShim})
			d.consumeShimEvents(ctrl)
			defer func() { _ = ctrl.Close(); d.shims.wg.Wait() }()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("pump did not call batch")
			}
			if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
				t.Fatalf("cursor advanced before ACK:%d", got)
			}
			close(release)
			select {
			case <-ctrl.Done():
			case <-ctx.Done():
				t.Fatal("refused batch did not close controller")
			}
			d.shims.wg.Wait()
			select {
			case err := <-callbackError:
				t.Fatal(err)
			default:
			}
			if got := d.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != tc.want {
				t.Fatalf("cursor=%d want%d", got, tc.want)
			}
			replay, err := sessionshim.Adopt(ctx, sessionshim.AdoptOptions{
				Registry: registry, ControllerID: "batch-replacement", RequireFullHostFrames: true,
				ResumeFrom: func(sessionshim.Identity) uint64 { return d.SessionShimForwardedSeq(id.OrgID, id.SessionID) + 1 },
			})
			if err != nil || len(replay.Adopted) != 1 {
				t.Fatalf("re-adopt=%+v %v", replay, err)
			}
			defer replay.Close()
			for _, want := range source[tc.want:] {
				select {
				case event := <-replay.Adopted[0].Events():
					if !bytes.Equal(event.FrameBytes, want) {
						t.Fatalf("replay suffix bytes changed:seq%d", event.Seq)
					}
				case <-ctx.Done():
					t.Fatal("exact suffix was not replayed")
				}
			}
		})
	}
}
