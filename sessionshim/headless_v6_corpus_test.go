package sessionshim

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// This file is the §5 conformance corpus for the headless wire version: the
// discriminating cases the contract demands beyond the codec unit pins in
// shimwire/. Each column drives a production entry point — Adopt for
// selection and record strictness, the shim's control read for profile
// refusal, dispatchCredentialUpdate for the update correlation, PushCredential
// for the controller side, and the finalize acknowledgement bound for the
// ack-wait — never a helper alone.
//
// The columns, in contract order:
//
//   - mixed-version selection: a headless [6,6] record meets a released
//     controller as protocol_mismatch (quarantined, never adopted, never
//     killed), and an interactive [1,6] range still selects 5 with a v5
//     controller;
//   - strict record decode: an older daemon's schema-1 decoder refuses a
//     headless record (TestOldSchemaDecoderRefusesHeadlessRecord), and this
//     build, which reads schema 2, refuses a headless record whose range
//     includes a version below 6 and quarantines it as record_malformed;
//   - stale-generation and changed-duplicate updates through the shim's
//     dispatch, plus the exact-retry replay;
//   - every refused PTY-shaped type on a headless connection, answered on the
//     wire through the production control read;
//   - the acknowledgement wait and its timeout;
//   - byte-identical released traffic (pinned in shimwire goldens; the corpus
//     asserts a released controller still adopts a released shim here).

// TestV6CorpusHeadlessRangeMeetsAReleasedControllerAsMismatch drives Adopt —
// the production startup pass — against a headless [6,6] record while this
// build still advertises max 5. The record names a live process (this test's
// own) so the pass reaches the range check rather than stale classification,
// and the outcome must be protocol_mismatch quarantine: the seat is never
// treated as a terminal session and never ended.
func TestV6CorpusHeadlessRangeMeetsAReleasedControllerAsMismatch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := Identity{OrgID: "org-v6-corpus", SessionID: "sess-headless-range"}
	self, err := Self()
	if err != nil {
		t.Fatalf("own process identity: %v", err)
	}
	rec := Record{
		SchemaVersion: HeadlessRecordSchemaVersion, Workload: WorkloadHeadless,
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-headless-1", ProcessEpoch: 1,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: reg.SocketPath(id), SocketDevice: 1, SocketInode: 2,
		ProtocolMin: shimwire.HeadlessMin, ProtocolMax: shimwire.HeadlessMax,
		Phase:             shimwire.PhaseRunning,
		WorkareaPath:      "/w/a",
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := reg.Put(rec); err != nil {
		t.Fatalf("Put headless record: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "released-controller"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(res.Close)

	if len(res.Adopted) != 0 {
		t.Fatalf("adopted %d headless shims with a max-5 controller, want 0", len(res.Adopted))
	}
	if len(res.Quarantined) != 1 {
		t.Fatalf("quarantined %d, want 1 (adopted=%d stale=%d)", len(res.Quarantined), len(res.Adopted), len(res.Stale))
	}
	q := res.Quarantined[0]
	if q.Reason != QuarantineProtocolMismatch {
		t.Fatalf("quarantine reason = %q, want protocol_mismatch", q.Reason)
	}
	if q.ProtocolMin != shimwire.HeadlessMin || q.ProtocolMax != shimwire.HeadlessMax {
		t.Fatalf("quarantine projection lost the headless range: [%d,%d]", q.ProtocolMin, q.ProtocolMax)
	}
	if !q.ConsumesCapacity {
		t.Fatal("quarantined headless shim reported consumesCapacity=false; its runner is still running")
	}
	// The Done-when quarantine half, algebraically as well: a v5 controller
	// range has no overlap with [6,6], and the adoption classifier maps the
	// resulting version mismatch to protocol_mismatch without string matching.
	if _, err := shimwire.Negotiate(shimwire.HeadlessMin, shimwire.HeadlessMax, shimwire.V1, shimwire.V5); !errors.Is(err, shimwire.ErrVersionMismatch) {
		t.Fatalf("headless [6,6] vs released [1,5] = %v, want ErrVersionMismatch", err)
	}
	if reason, _ := classifyAdoptionFailure(shimwire.ErrVersionMismatch); reason != QuarantineProtocolMismatch {
		t.Fatalf("classifier mapped ErrVersionMismatch to %q, want protocol_mismatch", reason)
	}
}

// TestV6CorpusInteractiveV6RangeStillSelectsV5 pins the other selection half:
// an interactive shim advertising [1,6] meets a released [1,5] controller at
// selected 5. No peer sends a v6 type merely because its binary advertises 6,
// so overlap resolves to the highest COMMON version.
func TestV6CorpusInteractiveV6RangeStillSelectsV5(t *testing.T) {
	t.Parallel()

	selected, err := shimwire.Negotiate(shimwire.V1, shimwire.V6, shimwire.V1, shimwire.V5)
	if err != nil || selected != shimwire.V5 {
		t.Fatalf("interactive [1,6] vs released [1,5] = (%d,%v), want selected 5", selected, err)
	}
	if selected, err := shimwire.Negotiate(shimwire.V1, shimwire.V6, shimwire.V1, shimwire.V6); err != nil || selected != shimwire.V6 {
		t.Fatalf("interactive [1,6] vs v6-capable [1,6] = (%d,%v), want selected 6", selected, err)
	}
	// And the live proof that a released controller still adopts a released
	// shim while this corpus runs: a real shim adopted at v2.
	id := Identity{OrgID: "org-v6-corpus", SessionID: "sess-released-adopt"}
	f := startShimHelper(t, id, 0)
	res := f.adoptAsMax(t, "released-controller-v2", shimwire.V2)
	if len(res.Adopted) != 1 {
		t.Fatalf("adopted %d released shims, want 1 (quarantined=%d stale=%d)", len(res.Adopted), len(res.Quarantined), len(res.Stale))
	}
	if res.Adopted[0].SelectedVersion() != shimwire.V2 {
		t.Fatalf("selected = v%d, want v2", res.Adopted[0].SelectedVersion())
	}
}

// TestV6CorpusHeadlessRecordOnALowerRangeIsRefused drives the registry — the
// production record path — with a headless schema-2 record on the released
// [1,5] range: the shape a headless shim built before the range binding
// advertised. The contract makes the headless key on a range that includes a
// version below 6 malformed, so this build's strict decoder refuses it and the
// startup pass quarantines it as record_malformed rather than selecting v5 and
// adopting a headless shim as a terminal session. (The older-daemon column,
// a schema-1-only decoder meeting any headless record, is
// TestOldSchemaDecoderRefusesHeadlessRecord.)
func TestV6CorpusHeadlessRecordOnALowerRangeIsRefused(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	id := Identity{OrgID: "org-v6-corpus", SessionID: "sess-headless-record"}
	self, err := Self()
	if err != nil {
		t.Fatalf("own process identity: %v", err)
	}
	rec := testRecord(t, id, reg)
	rec.PID, rec.ProcessStartedAt = self.PID, self.StartedAt
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The headless record shape (schema 2 with the workload member) on the
	// released range testRecord carries.
	asMap["schemaVersion"] = 2
	asMap["workload"] = "headless"
	tampered, err := json.Marshal(asMap)
	if err != nil {
		t.Fatalf("marshal headless record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reg.Dir(), id.RecordName()), tampered, RecordFileMode); err != nil {
		t.Fatalf("write headless record: %v", err)
	}

	if rec.ProtocolMin >= shimwire.V6 {
		t.Fatalf("fixture range [%d,%d] does not include a version below 6", rec.ProtocolMin, rec.ProtocolMax)
	}
	if _, err := reg.Get(id); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("Get headless record on [%d,%d] = %v, want ErrRecordInvalid", rec.ProtocolMin, rec.ProtocolMax, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "older-daemon"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(res.Close)
	if len(res.Adopted) != 0 {
		t.Fatalf("adopted %d headless records, want 0", len(res.Adopted))
	}
	if len(res.Quarantined) != 1 || res.Quarantined[0].Reason != QuarantineRecordMalformed {
		t.Fatalf("quarantined = %+v, want one record_malformed", res.Quarantined)
	}
}

// credentialShimPipe builds the two ends of a selected-v6 control connection
// for dispatch-level corpus columns: the shim side (a fenced Shim with the
// given committed generation) and the controller end of the pipe. Dispatch
// takes the decoded body directly, so the test feeds dispatch the body while
// the pipe carries only the shim's answers back to the controller end.
func credentialShimPipe(t *testing.T, committed shimwire.Generation) (*Shim, *controllerConn, *shimwire.Reader) {
	t.Helper()
	clientConn, shimConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close() //nolint:errcheck
		_ = shimConn.Close()   //nolint:errcheck
	})
	shim := &Shim{gen: committed}
	ctrl := &controllerConn{
		w: shimwire.NewWriter(shimConn), selected: shimwire.V6,
		credentialLedger: make(map[uint64]*credentialLedgerEntry),
	}
	return shim, ctrl, shimwire.NewReader(clientConn)
}

// readCredentialResult is the test's controller end: read one shim answer,
// failing the test rather than parking forever when the shim never answers.
func readCredentialResult(t *testing.T, r *shimwire.Reader) shimwire.CredentialResult {
	t.Helper()
	type outcome struct {
		result shimwire.CredentialResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := r.ReadVersion(shimwire.V6)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		if msg.Type != shimwire.TypeCredentialResult {
			done <- outcome{err: errors.New("shim answered " + msg.Type.String() + ", want CredentialResult")}
			return
		}
		result, err := shimwire.DecodeCredentialResult(msg.Body)
		done <- outcome{result: result, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ReadVersion(CredentialResult): %v", got.err)
		}
		return got.result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the shim's credential answer")
		return shimwire.CredentialResult{}
	}
}

// TestV6CorpusCredentialUpdateCorrelation drives dispatchCredentialUpdate —
// the production dispatch path — through a real framed connection: success on
// the committed generation, exact-retry replay without reinstall, changed
// duplicate, and the stale-generation refusal. The fence and the ledger are
// what slice 3's provider install will stand on, so the corpus pins them
// before the install exists.
func TestV6CorpusCredentialUpdateCorrelation(t *testing.T) {
	t.Parallel()

	shim, ctrl, reader := credentialShimPipe(t, 3)
	served := make(chan error, 4)
	serve := func(body []byte) {
		served <- shim.dispatchCredentialUpdate(ctrl, body)
	}

	update := shimwire.CredentialUpdate{RequestID: 7, Generation: 3, WorkerID: "wrk_9", Bearer: "opaque", ExpiresAt: 1760000000000000000}
	raw, err := shimwire.EncodeCredentialUpdate(update)
	if err != nil {
		t.Fatal(err)
	}
	go serve(raw)
	// The answer is read before dispatch is joined: dispatch blocks writing
	// it into the pipe until this end reads, so joining first would deadlock
	// the rendezvous.
	if got := readCredentialResult(t, reader); got != (shimwire.CredentialResult{RequestID: 7, Generation: 3, Status: shimwire.CredentialSuccess}) {
		t.Fatalf("success result = %+v", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("dispatch success update: %v", err)
	}

	// Exact retry: the same bytes again replay the first result.
	go serve(raw)
	if got := readCredentialResult(t, reader); got.Status != shimwire.CredentialSuccess || got.RequestID != 7 {
		t.Fatalf("exact retry result = %+v, want the first success replayed", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("dispatch exact retry: %v", err)
	}

	// Changed duplicate: the same id with different content.
	changed := update
	changed.Bearer = "rotated"
	changedRaw, err := shimwire.EncodeCredentialUpdate(changed)
	if err != nil {
		t.Fatal(err)
	}
	go serve(changedRaw)
	if got := readCredentialResult(t, reader); got.Status != shimwire.CredentialDuplicateChanged || got.RequestID != 7 {
		t.Fatalf("changed duplicate result = %+v, want duplicate_changed", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("dispatch changed duplicate: %v", err)
	}

	// Stale generation: a resurfaced controller's update is refused, and the
	// connection stays up for the next frame.
	stale := shimwire.CredentialUpdate{RequestID: 8, Generation: 2, WorkerID: "wrk_9", Bearer: "opaque", ExpiresAt: 1760000000000000000}
	staleRaw, err := shimwire.EncodeCredentialUpdate(stale)
	if err != nil {
		t.Fatal(err)
	}
	go serve(staleRaw)
	if got := readCredentialResult(t, reader); got.Status != shimwire.CredentialStaleGeneration || got.RequestID != 8 {
		t.Fatalf("stale result = %+v, want stale_generation", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("dispatch stale update: %v", err)
	}
}

// TestV6CorpusHeadlessReadRefusesEveryPTYType drives readControllerFrame —
// the production control read — on a headless-profile connection: every
// PTY-shaped frame is answered with Error{code:"malformed"} on the wire and
// never reaches dispatch (Handled), while the headless vocabulary dispatches
// normally.
func TestV6CorpusHeadlessReadRefusesEveryPTYType(t *testing.T) {
	t.Parallel()

	ptyShaped := []shimwire.MessageType{
		shimwire.TypeOutput, shimwire.TypeGap, shimwire.TypeSnapshot,
		shimwire.TypeInput, shimwire.TypeResize, shimwire.TypeExit,
		shimwire.TypeSnapshotRequest, shimwire.TypeSnapshotResult,
		shimwire.TypeHostFrame, shimwire.TypeAttributedInput,
		shimwire.TypeCheckpointRequest, shimwire.TypeCheckpointResult,
	}
	for _, mt := range ptyShaped {
		t.Run(mt.String(), func(t *testing.T) {
			t.Parallel()
			clientConn, shimConn := net.Pipe()
			defer clientConn.Close() //nolint:errcheck
			defer shimConn.Close()   //nolint:errcheck
			shim := &Shim{}
			ctrl := &controllerConn{
				w:       shimwire.NewWriter(shimConn),
				profile: shimwire.ProfileHeadless, selected: shimwire.V6,
				credentialLedger: make(map[uint64]*credentialLedgerEntry),
			}
			shimReader, clientReader := shimwire.NewReader(shimConn), shimwire.NewReader(clientConn)
			clientWriter := shimwire.NewWriter(clientConn)

			frameDone := make(chan controllerFrame, 1)
			go func() {
				frame, err := shim.readControllerFrame(ctrl, shimReader)
				if err != nil {
					return
				}
				frameDone <- frame
			}()

			body := []byte{0x11, 0x22}
			if mt == shimwire.TypeInput {
				body = shimwire.EncodeInput(3, []byte("x"))
			}
			if err := clientWriter.WriteVersion(shimwire.V6, mt, body); err != nil {
				t.Fatalf("WriteVersion(%s): %v", mt, err)
			}
			answer, err := clientReader.ReadVersion(shimwire.V6)
			if err != nil {
				t.Fatalf("read refusal answer: %v", err)
			}
			if answer.Type != shimwire.TypeError {
				t.Fatalf("refusal answer = %s, want Error", answer.Type)
			}
			refusal, err := shimwire.DecodeError(answer.Body)
			if err != nil || refusal.Code != shimwire.CodeMalformed {
				t.Fatalf("refusal answer = (%+v,%v), want code malformed", refusal, err)
			}
			select {
			case frame := <-frameDone:
				if !frame.Handled {
					t.Fatalf("%s reached dispatch on a headless connection", mt)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s was not refused by the production read", mt)
			}
		})
	}
}

// TestV6CorpusControllerPushCorrelatesResults drives PushCredential — the
// production controller entry point — against a scripted v6 peer: success,
// exact-retry without a second wire write, changed-duplicate refused locally,
// and a stale_generation answer surfacing as a typed refusal.
func TestV6CorpusControllerPushCorrelatesResults(t *testing.T) {
	t.Parallel()

	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	defer shimConn.Close()   //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 3, selected: shimwire.V6,
		events: make(chan ControllerEvent, 64), done: make(chan struct{}), closing: make(chan struct{}),
		backlog:       newEventBacklog(0, 0, nil, nil, 0, 0),
		snapshotCalls: make(map[uint64]*snapshotCall), credentialCalls: make(map[uint64]*credentialCall),
	}
	if !controller.SupportsCredentialPush() {
		t.Fatal("selected-v6 controller reports no credential push")
	}
	peerReader, peerWriter := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
	// The controller's read loop is what correlates answers; drive it for
	// this connection the way Dial does — before the first blocking peer
	// write, since the pipe rendezvous needs a reader on the other end.
	go controller.readLoop()

	pushDone := make(chan struct{})
	var first shimwire.CredentialResult
	var firstErr error
	go func() {
		defer close(pushDone)
		first, firstErr = controller.PushCredential(context.Background(), 7, "wrk_9", "opaque", 1760000000000000000)
	}()

	// The peer sees exactly one update and answers success.
	msg, err := peerReader.ReadVersion(shimwire.V6)
	if err != nil {
		t.Fatalf("peer read update: %v", err)
	}
	if msg.Type != shimwire.TypeCredentialUpdate {
		t.Fatalf("peer saw %s, want CredentialUpdate", msg.Type)
	}
	update, err := shimwire.DecodeCredentialUpdate(msg.Body)
	if err != nil {
		t.Fatalf("peer decode update: %v", err)
	}
	if update.RequestID != 7 || update.Generation != 3 || update.Bearer != "opaque" {
		t.Fatalf("peer update = %+v", update)
	}
	success, err := shimwire.EncodeCredentialResult(shimwire.CredentialResult{RequestID: 7, Generation: 3, Status: shimwire.CredentialSuccess})
	if err != nil {
		t.Fatal(err)
	}
	if err := peerWriter.WriteVersion(shimwire.V6, shimwire.TypeCredentialResult, success); err != nil {
		t.Fatalf("peer write success: %v", err)
	}
	select {
	case <-pushDone:
	case <-time.After(5 * time.Second):
		t.Fatal("PushCredential did not complete after the peer answered")
	}
	if firstErr != nil || first.Status != shimwire.CredentialSuccess {
		t.Fatalf("PushCredential = (%+v,%v), want success", first, firstErr)
	}

	// Exact retry: cached, no second wire write. The peer must see nothing.
	retry, err := controller.PushCredential(context.Background(), 7, "wrk_9", "opaque", 1760000000000000000)
	if err != nil || retry != first {
		t.Fatalf("exact retry = (%+v,%v), want the cached first result", retry, err)
	}
	if err := shimConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := peerReader.ReadVersion(shimwire.V6); err == nil {
		t.Fatal("exact retry wrote a second update; it must replay the first result")
	}

	// Changed duplicate: refused locally, before another wire request.
	if _, err := controller.PushCredential(context.Background(), 7, "wrk_9", "rotated", 1760000000000000000); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("changed duplicate = %v, want ErrSnapshotMismatch", err)
	}
	_ = controller.Close()
}

// TestV6CorpusControllerPushRefusesStaleGeneration drives the stale half
// through the same production entry point: the peer answers stale_generation
// and PushCredential surfaces the typed refusal.
func TestV6CorpusControllerPushRefusesStaleGeneration(t *testing.T) {
	t.Parallel()

	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	defer shimConn.Close()   //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 3, selected: shimwire.V6,
		events: make(chan ControllerEvent, 64), done: make(chan struct{}), closing: make(chan struct{}),
		backlog:       newEventBacklog(0, 0, nil, nil, 0, 0),
		snapshotCalls: make(map[uint64]*snapshotCall), credentialCalls: make(map[uint64]*credentialCall),
	}
	peerReader, peerWriter := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
	go controller.readLoop()

	pushDone := make(chan error, 1)
	go func() {
		_, err := controller.PushCredential(context.Background(), 8, "wrk_9", "opaque", 1760000000000000000)
		pushDone <- err
	}()
	msg, err := peerReader.ReadVersion(shimwire.V6)
	if err != nil || msg.Type != shimwire.TypeCredentialUpdate {
		t.Fatalf("peer saw (%s,%v), want CredentialUpdate", msg.Type, err)
	}
	stale, err := shimwire.EncodeCredentialResult(shimwire.CredentialResult{RequestID: 8, Generation: 3, Status: shimwire.CredentialStaleGeneration})
	if err != nil {
		t.Fatal(err)
	}
	if err := peerWriter.WriteVersion(shimwire.V6, shimwire.TypeCredentialResult, stale); err != nil {
		t.Fatalf("peer write stale: %v", err)
	}
	select {
	case err := <-pushDone:
		if !errors.Is(err, shimwire.ErrSnapshotRefused) {
			t.Fatalf("stale push = %v, want ErrSnapshotRefused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PushCredential did not complete after the stale answer")
	}
	_ = controller.Close()
}

// TestV6CorpusHeadlessExitAcknowledgementWait drives the acknowledgement
// column: the controller observes a HeadlessExit as its terminal event, then
// acknowledges it with the generation-fenced Heartbeat ackedSeq=1 the shim's
// finalize bound waits for. A missing observation cannot be acknowledged.
func TestV6CorpusHeadlessExitAcknowledgementWait(t *testing.T) {
	t.Parallel()

	if err := (&Controller{}).AcknowledgeHeadlessExit(context.Background()); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("ack without an observation = %v, want ErrSnapshotMismatch", err)
	}

	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	defer shimConn.Close()   //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 3, selected: shimwire.V6, profile: shimwire.ProfileHeadless,
		events: make(chan ControllerEvent, 64), done: make(chan struct{}), closing: make(chan struct{}),
		backlog:       newEventBacklog(0, 0, nil, nil, 0, 0),
		snapshotCalls: make(map[uint64]*snapshotCall), credentialCalls: make(map[uint64]*credentialCall),
	}
	peerReader, peerWriter := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
	// Selected v6 carries the v3 event rail: the read loop feeds the backlog
	// and dispatchEvents drains it to Events, exactly as Dial wires them.
	go controller.dispatchEvents()
	go controller.readLoop()

	exitBody, err := shimwire.EncodeHeadlessExit(shimwire.HeadlessExit{
		Seq: 1, ProcessEpoch: 1, Cause: shimwire.HeadlessExitCompleted,
		OutboxKey: "org/sess/attempt-2", OutboxState: shimwire.HeadlessExitDelivered,
		GroupReaped: true, ObservedAt: 1760000000000000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := peerWriter.WriteVersion(shimwire.V6, shimwire.TypeHeadlessExit, exitBody); err != nil {
		t.Fatalf("peer write HeadlessExit: %v", err)
	}
	select {
	case ev := <-controller.Events():
		if ev.Kind != EventHeadlessExit || ev.Seq != 1 {
			t.Fatalf("terminal event = %+v, want headless_exit seq 1", ev)
		}
		if ev.HeadlessExit.Cause != shimwire.HeadlessExitCompleted || ev.HeadlessExit.OutboxKey != "org/sess/attempt-2" {
			t.Fatalf("terminal observation = %+v", ev.HeadlessExit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HeadlessExit never arrived as a controller event")
	}
	if controller.ObservedHeadlessExit() == nil {
		t.Fatal("observed headless exit not retained")
	}

	// The acknowledgement is a generation-fenced Heartbeat with ackedSeq=1.
	// The peer answers it the way the shim's heartbeat path does so the ack
	// completes rather than timing out.
	ackDone := make(chan error, 1)
	go func() { ackDone <- controller.AcknowledgeHeadlessExit(context.Background()) }()
	msg, err := peerReader.ReadVersion(shimwire.V6)
	if err != nil {
		t.Fatalf("peer read ack: %v", err)
	}
	if msg.Type != shimwire.TypeHeartbeat {
		t.Fatalf("ack frame = %s, want Heartbeat", msg.Type)
	}
	ack, err := shimwire.DecodeHeartbeat(msg.Body)
	if err != nil {
		t.Fatalf("ack decode: %v", err)
	}
	if ack.Generation != 3 || ack.AckedSeq != 1 {
		t.Fatalf("ack = %+v, want generation 3 ackedSeq 1", ack)
	}
	receipt, err := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: 3, AckedSeq: 1, Phase: shimwire.PhaseExited})
	if err != nil {
		t.Fatal(err)
	}
	if err := peerWriter.WriteVersion(shimwire.V6, shimwire.TypeHeartbeat, receipt); err != nil {
		t.Fatalf("peer write receipt: %v", err)
	}
	select {
	case err := <-ackDone:
		if err != nil {
			t.Fatalf("AcknowledgeHeadlessExit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("acknowledgement did not complete after the receipt")
	}
	_ = controller.Close()
}

// TestV6CorpusFinalizeWaitTimesOut pins the timeout half of the
// acknowledgement column: the shim's finalize wait returns (rather than
// parking forever) when the ackedSeq=1 receipt never arrives, leaving the
// tombstone for the next adoption. It drives waitForDurableAck — the exact
// wait finalizeTerminal spends — with a short bound.
func TestV6CorpusFinalizeWaitTimesOut(t *testing.T) {
	t.Parallel()

	shim := &Shim{ackNotify: make(chan struct{})}
	started := time.Now()
	shim.waitForDurableAck(1, 100*time.Millisecond)
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("finalize wait returned after %s, want at least the 100ms bound", waited)
	}

	// And the wait ends early when the receipt lands: advance the cursor and
	// wake the waiter the way persistHeartbeatAck does.
	shim.ackedSeq = 1
	close(shim.ackNotify)
	done := make(chan struct{})
	go func() {
		defer close(done)
		shim.waitForDurableAck(1, 5*time.Second)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalize wait did not return after the acknowledgement landed")
	}
}

// TestV6CorpusReleasedTrafficStillAdopts is the byte-identity column at the
// adoption level: while the v6 codec ships, a released [1,5] shim is still
// adopted by this build's startup pass, and the v6 types never appear on its
// wire. (The golden byte pins live in shimwire; this test proves the
// admission the goldens assume.)
func TestV6CorpusReleasedTrafficStillAdopts(t *testing.T) {
	t.Parallel()

	id := Identity{OrgID: "org-v6-corpus", SessionID: "sess-released-wire"}
	f := startShimHelper(t, id, 0)
	res := f.adoptAs(t, "released-controller")
	if len(res.Adopted) != 1 {
		t.Fatalf("adopted %d, want 1 (quarantined=%d stale=%d)", len(res.Adopted), len(res.Quarantined), len(res.Stale))
	}
	c := res.Adopted[0]
	if c.SelectedVersion() > shimwire.V5 {
		t.Fatalf("selected = v%d, want at most v5 while the launch gate holds", c.SelectedVersion())
	}
	// The session is live in both directions after adoption.
	exchange(t, c, "v6-corpus-live")
}

// TestV6CorpusHeadlessExitIsTheSoleTerminalAuthority pins the contract's
// "no legacy Exit accompanies it" rule at the controller: observing a
// HeadlessExit then an Exit (or the reverse) is a changed-observation refusal,
// never a second terminal event.
func TestV6CorpusHeadlessExitIsTheSoleTerminalAuthority(t *testing.T) {
	t.Parallel()

	controller := &Controller{
		events: make(chan ControllerEvent, 64),
		done:   make(chan struct{}), closing: make(chan struct{}),
		snapshotCalls: make(map[uint64]*snapshotCall), credentialCalls: make(map[uint64]*credentialCall),
	}
	exit := shimwire.HeadlessExit{
		Seq: 1, ProcessEpoch: 1, Cause: shimwire.HeadlessExitCompleted,
		OutboxKey: "k", OutboxState: shimwire.HeadlessExitDelivered,
		GroupReaped: true, ObservedAt: 1760000000000000000,
	}
	body, err := shimwire.EncodeHeadlessExit(exit)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.observeHeadlessExit(body); err != nil {
		t.Fatalf("observe HeadlessExit: %v", err)
	}
	if err := controller.observeExit(shimwire.ExitMsg{Seq: 9, ExitCode: 1}); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("Exit after HeadlessExit = %v, want ErrSnapshotMismatch", err)
	}
	changed, err := shimwire.EncodeHeadlessExit(shimwire.HeadlessExit{
		Seq: 1, ProcessEpoch: 1, Cause: shimwire.HeadlessExitOrphaned,
		OutboxKey: "k", OutboxState: shimwire.HeadlessExitPending,
		GroupReaped: true, ObservedAt: 1760000000000000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.observeHeadlessExit(changed); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("changed HeadlessExit = %v, want ErrSnapshotMismatch", err)
	}
	// Draining the one terminal event proves exactly one was published.
	select {
	case ev := <-controller.Events():
		if ev.Kind != EventHeadlessExit {
			t.Fatalf("terminal event kind = %q, want headless_exit", ev.Kind)
		}
	default:
		t.Fatal("no terminal event was published for the observed HeadlessExit")
	}
	select {
	case ev := <-controller.Events():
		t.Fatalf("second terminal event published: %+v", ev.Kind)
	default:
	}
}
