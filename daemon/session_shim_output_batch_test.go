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
// shim and PTY, and hands output to a real candidate whose relay fixture
// acknowledges each frame fixtureAckDelay after it arrives while the source
// emits every 33ms. The test pins the batching property structurally: the pump
// hands ready output to the carrier in multi-frame windows while output is
// still arriving. A pump that sends one frame per round trip fails the gate.
// The hold bound — a ready frame is never held past the gather flush — is
// pinned deterministically in logical time by
// TestCollectShimOutputBatchFlushesHeldOutput, which drives the same gather
// step with an injected timer; an end-to-end wall-clock hold assertion cannot
// separate a held pump from a loaded scheduler, so this test keeps none.
func TestShimOutputBatchBatchesLiveOutput(t *testing.T) {
	// The relay fixture acknowledges each frame this long after it arrives.
	// It paces the carrier round trips the pump must keep up with; the batching
	// verdict itself is structural (a multi-frame window while the source
	// emits), so scheduler jitter delays it instead of flipping it.
	const fixtureAckDelay = 75 * time.Millisecond
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
	// The shell reads its start line and then emits one line per tick.
	// The read is the only synchronization the test needs: the shell cannot
	// emit before the test writes "start", so every output frame belongs to
	// this run's emission window by construction. The shell floods 600 lines
	// (~20s), so the window outlasts the gate: a slow scheduler delays the
	// verdict without the source running dry first; the test emits its
	// Marker only after the gate below has passed.
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "stty -echo; read start; i=0; while [ $i -lt 600 ]; do printf 'output-%02d\\n' \"$((i % 100))\"; i=$((i+1)); sleep 0.033; done"}},
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
	type arrival struct {
		raw []byte
	}
	var mu sync.Mutex
	source := make(map[uint64]arrival)
	sourceDone := make(chan struct{})
	go func() {
		defer close(sourceDone)
		for frame := range direct.Frames() {
			mu.Lock()
			if _, seen := source[frame.Seq]; !seen {
				source[frame.Seq] = arrival{raw: frame.Encode()}
			}
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

	// The relay records every frame's bytes. The pump's
	// batch callback records each window's size and closes liveBatch on the
	// first multi-frame window. The gate below waits for both: relay traffic
	// AND a multi-frame batch.
	var wireMu sync.Mutex
	wireArrivals := make(map[uint64]struct{})
	wireBytes := make(map[uint64][]byte)
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
			wireMu.Lock()
			wireArrivals[frame.Seq] = struct{}{}
			wireBytes[frame.Seq] = append([]byte(nil), raw...)
			wireMu.Unlock()
			mu.Lock()
			original, ok := source[frame.Seq]
			mu.Unlock()
			// The pump may forward a frame before the direct subscription's
			// goroutine records it; only a byte mismatch against a RECORDED
			// frame is a failure. An unrecorded frame is still checked once
			// the subscription catches up: the final byte-exact audit below
			// compares every relayed frame against the recorded source.
			if ok && !bytes.Equal(raw, original.raw) {
				serverResult <- fmt.Errorf("source bytes differ at %d", frame.Seq)
				return
			}
			queue <- scheduledAck{frame.Seq, time.Now().Add(fixtureAckDelay)}
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
	var largestBatch int
	finished := make(chan struct{})
	// liveBatch is closed by the first batch callback: the relay receives a
	// batch only after the pump forwards it, so the first batch the relay
	// sees proves the pump forwarded mid-stream output rather than sitting
	// idle until the source went quiet.
	liveBatch := make(chan struct{})
	var liveOnce sync.Once
	d := &Daemon{shims: newSessionShimState()}
	d.opts.SessionShim = SessionShimConfig{
		CallbackTimeout: time.Second,
		OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
			err := candidate.SendRawFrameDurable(ctx, event.FrameBytes)
			if err == nil {
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
			if len(events) >= 2 {
				liveOnce.Do(func() { close(liveBatch) })
			}
			mu.Unlock()
			ack, err := candidate.SendRawFramesDurable(callCtx, raws)
			return ack, err
		},
	}
	d.shimIdentityRef.Store(&sessionShimIdentity{config: &d.opts.SessionShim})
	d.consumeShimEvents(ctrl)
	defer func() { _ = ctrl.Close(); d.shims.wg.Wait() }()
	// Start the source, then wait for two independent liveness facts: the
	// relay is receiving output frames (wireArrivals), and at least one of
	// those frames travelled inside a multi-frame batch (liveBatch). A pump
	// that forwards one frame at a time satisfies the first but never the
	// second. Frames keep arriving for the whole emission window, so early
	// frames arm both gates without waiting out the source. The wait only
	// needs to cover a slow scheduler, not the source: liveBatch closes on
	// the first multi-frame window, which the pump gathers within
	// milliseconds of the first frames arriving. A successful gate records
	// the sampled counts for the final log line.
	if _, err := shim.Session().WriteInput([]byte("start\n")); err != nil {
		t.Fatal(err)
	}
	// The shell emits one line per 33ms tick after reading "start", so the
	// first output frame proves the emission window opened. Waiting for it
	// here — rather than inside the relay gate — separates "the shell never
	// started" (a fixture failure, diagnosed here) from "the pump never
	// forwarded" (the behaviour under test, diagnosed at the gate).
	waitFor(t, 9*time.Second, "first source output frame", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, frame := range source {
			if bytes.Contains(frame.raw, []byte("output-")) {
				return true
			}
		}
		return false
	})
	var gateWire, gateSource, gateBatch int
	// The gate keeps waiting while the pipeline is making progress: under a
	// loaded -race scheduler each stage (PTY read, shim socket, controller
	// backlog, pump timer, fixture round trip) can stall for seconds at a
	// time, so a fixed deadline mistakes a slow scheduler for a dead pump.
	// Progress is fresh relay output lines or a newly observed batch window;
	// a pump that forwards one frame at a time makes the first kind and never
	// the second, so it still times out. The stall arm bounds every wait, and
	// the test context below bounds it further.
	lastProgress := time.Now()
	lastWire, lastBatch := -1, -1
	for {
		mu.Lock()
		wireMu.Lock()
		nWire, nSource, nBatch := 0, len(source), largestBatch
		for seq := range wireArrivals {
			if frame, ok := source[seq]; ok && bytes.Contains(frame.raw, []byte("output-")) {
				nWire++
			}
		}
		wireMu.Unlock()
		mu.Unlock()
		if nWire != lastWire || nBatch != lastBatch {
			lastProgress = time.Now()
			lastWire, lastBatch = nWire, nBatch
		}
		select {
		case <-liveBatch:
			if nWire >= 2 {
				gateWire, gateSource, gateBatch = nWire, nSource, nBatch
				goto gated
			}
		default:
		}
		if time.Since(lastProgress) > 9*time.Second {
			t.Fatalf("relay pipeline stalled: no new output lines or batch windows for 9s (wire=%d batch=%d)", lastWire, lastBatch)
		}
		time.Sleep(20 * time.Millisecond)
	}
gated:
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
	batchCount := largestBatch
	mu.Unlock()
	wireMu.Lock()
	floods := 0
	for seq := range wireArrivals {
		mu.Lock()
		frame, ok := source[seq]
		mu.Unlock()
		if ok && bytes.Contains(frame.raw, []byte("output-")) {
			floods++
		}
	}
	wireMu.Unlock()
	// The relay received output inside multi-frame batches: the pump gathered
	// ready frames into windows instead of forwarding one frame at a time.
	// The relay's byte-exact check above is the authority for every frame's
	// content; these counters carry only the batching requirement.
	wireMu.Lock()
	relayFrames := len(wireArrivals)
	wireMu.Unlock()
	mu.Lock()
	sourceFrames := len(source)
	mu.Unlock()
	if floods < 2 {
		t.Fatalf("relay received no live output; flooded=%d batchCount=%d relayFrames=%d sourceFrames=%d", floods, batchCount, relayFrames, sourceFrames)
	}
	if batchCount < 2 {
		t.Fatalf("production pump never batched live output; flooded=%d batchCount=%d relayFrames=%d sourceFrames=%d", floods, batchCount, relayFrames, sourceFrames)
	}
	// Every frame the relay received must match the source byte-exactly.
	// The direct subscription has observed the whole stream by now — the
	// Marker flushed through the pump after every output frame — so any
	// frame still unrecorded is genuinely missing, not merely delayed.
	mu.Lock()
	defer mu.Unlock()
	wireMu.Lock()
	defer wireMu.Unlock()
	for seq, raw := range wireBytes {
		original, ok := source[seq]
		if !ok {
			t.Fatalf("relay frame %d was never recorded at the source", seq)
		}
		if !bytes.Equal(raw, original.raw) {
			t.Fatalf("source bytes differ at %d", seq)
		}
	}
	t.Logf("DURABLE_OUTPUT_BATCH_GREEN flooded=%d largestBatch=%d gateWire=%d gateSource=%d gateBatch=%d", floods, batchCount, gateWire, gateSource, gateBatch)
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

// stubBatchTimer is a deterministic gather deadline for the batch step: the
// test fires it on demand, so the hold bound holds in logical time no matter
// how slow the scheduler is.
type stubBatchTimer struct {
	ch      chan time.Time
	stopped bool
}

func newStubBatchTimer() *stubBatchTimer { return &stubBatchTimer{ch: make(chan time.Time, 1)} }

func (t *stubBatchTimer) C() <-chan time.Time { return t.ch }

func (t *stubBatchTimer) Stop() bool {
	if t.stopped {
		return false
	}
	t.stopped = true
	return len(t.ch) == 0
}

func (t *stubBatchTimer) fire() { t.ch <- time.Now() }

// TestCollectShimOutputBatchFlushesHeldOutput pins the hold bound the
// end-to-end test cannot: ready output is handed on once the gather deadline
// fires, even when no further frame arrives. It drives the production gather
// step (collectShimOutputBatchWithTimer, the same function consumeShimEvents
// calls) with an injected deadline, so scheduler jitter cannot change the
// verdict: the verdict arrives when the test fires the timer, in logical
// time.
func TestCollectShimOutputBatchFlushesHeldOutput(t *testing.T) {
	output := func(seq uint64) sessionshim.ControllerEvent {
		// Seq values here are small fixture counters, never near the byte range.
		raw := (attachwire.Frame{Type: attachwire.TypeOutput, Seq: seq, Payload: []byte{byte(seq)}}).Encode() //nolint:gosec
		return sessionshim.ControllerEvent{Kind: sessionshim.EventHostFrame, FrameType: attachwire.TypeOutput, Seq: seq, FrameBytes: raw}
	}
	t.Run("flush on deadline with no further frames", func(t *testing.T) {
		events := make(chan sessionshim.ControllerEvent)
		timer := newStubBatchTimer()
		done := make(chan struct{})
		var batch []sessionshim.ControllerEvent
		var pending *sessionshim.ControllerEvent
		go func() {
			defer close(done)
			batch, pending = collectShimOutputBatchWithTimer(events, output(1), timer)
		}()
		// No frame arrives; the gather step must still return once its
		// deadline fires — a ready frame is never held past the flush.
		timer.fire()
		waitFor(t, 10*time.Second, "held output flush", func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		if len(batch) != 1 || batch[0].Seq != 1 || pending != nil {
			t.Fatalf("held batch=%d pending=%v, want one frame and no pending", len(batch), pending)
		}
	})
	t.Run("ready frames join before the deadline", func(t *testing.T) {
		events := make(chan sessionshim.ControllerEvent, 1)
		events <- output(2)
		timer := newStubBatchTimer()
		done := make(chan struct{})
		var batch []sessionshim.ControllerEvent
		var pending *sessionshim.ControllerEvent
		go func() {
			defer close(done)
			batch, pending = collectShimOutputBatchWithTimer(events, output(1), timer)
		}()
		// The queued frame joins the window first; the deadline then flushes
		// the coalesced window. Two facts, one order: joining is eager,
		// flushing waits for the deadline.
		waitFor(t, 10*time.Second, "queued frame to join the window", func() bool {
			return len(events) == 0
		})
		timer.fire()
		waitFor(t, 10*time.Second, "coalesced window flush", func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		if len(batch) != 2 || batch[1].Seq != 2 || pending != nil {
			t.Fatalf("coalesced batch=%d pending=%v, want both frames", len(batch), pending)
		}
		if !timer.stopped {
			t.Fatal("gather step did not release its deadline after flushing")
		}
	})
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
			sourceControl, sourceSpec := newBatchPTYSource(ctx, t, dir)
			shim, err := sessionshim.Start(sessionshim.Options{
				Identity: id, Registry: registry, ProcessEpoch: 1,
				Spec:   sourceSpec,
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
			// Frame count is a property of real PTY reads, not printf calls.
			// An out-of-band handshake gates each indivisible output byte.
			for _, b := range []byte("abcdef") {
				source = append(source, sourceControl.emit(ctx, t, direct.Frames(), b))
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
