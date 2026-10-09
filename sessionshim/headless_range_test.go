package sessionshim

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// This file pins the headless range binding (session-shim v6 §1) from both
// ends of the wire:
//
//   - the headless shim advertises exactly [6,6] and refuses every lower
//     selection before any generation commits;
//   - this build's Adopt never treats a headless shim as a terminal session:
//     a real headless shim is quarantined as protocol_mismatch, and the
//     legacy shape (a headless shim on an interactive range) is refused at
//     the record, the Hello decode, or the record/Hello workload check
//     without any authority ever being proposed;
//   - a headless controller never adopts an interactive peer, and refuses
//     every PTY-shaped frame on a headless connection;
//   - an interactive shim keeps its released range and Hello.

// dialRawShim opens a raw controller connection to a shim socket, below the
// Controller type, so a test can send exactly the Welcome it is probing.
func dialRawShim(t *testing.T, socketPath string) (net.Conn, *shimwire.Reader, *shimwire.Writer) {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn, shimwire.NewReader(conn), shimwire.NewWriter(conn)
}

// readRawHello reads the shim's opening frame and returns both the decoded
// Hello and its exact body bytes.
func readRawHello(t *testing.T, r *shimwire.Reader) (shimwire.Hello, []byte) {
	t.Helper()
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if msg.Type != shimwire.TypeHello {
		t.Fatalf("opening frame = %s, want Hello", msg.Type)
	}
	hello, err := shimwire.DecodeHello(msg.Body)
	if err != nil {
		t.Fatalf("decode hello %s: %v", msg.Body, err)
	}
	return hello, msg.Body
}

// TestHeadlessShimAdvertisesExactlyV6 pins the advertisement half: the
// discovery record and the live Hello of a headless shim both carry exactly
// [6,6] beside workload headless, and the range resolver refuses any other
// headless range rather than narrowing to it.
func TestHeadlessShimAdvertisesExactlyV6(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	_, registry, id := startHeadlessFixture(t, runner, 1)

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatalf("Get headless record: %v", err)
	}
	if rec.ProtocolMin != shimwire.V6 || rec.ProtocolMax != shimwire.V6 {
		t.Fatalf("headless record range = [%d,%d], want exactly [6,6]", rec.ProtocolMin, rec.ProtocolMax)
	}

	_, r, _ := dialRawShim(t, rec.SocketPath)
	hello, raw := readRawHello(t, r)
	if hello.Min != shimwire.V6 || hello.Max != shimwire.V6 {
		t.Fatalf("headless Hello range = [%d,%d], want exactly [6,6]", hello.Min, hello.Max)
	}
	if hello.Workload != shimwire.ProfileHeadless || !strings.Contains(string(raw), `"workload":"headless"`) {
		t.Fatalf("headless Hello does not declare the profile: workload=%q body=%s", hello.Workload, raw)
	}

	for _, tc := range []struct {
		name             string
		reqMin, reqMax   uint32
		wantMin, wantMax uint32
		wantErr          bool
	}{
		{name: "default", wantMin: shimwire.V6, wantMax: shimwire.V6},
		{name: "exact", reqMin: shimwire.V6, reqMax: shimwire.V6, wantMin: shimwire.V6, wantMax: shimwire.V6},
		{name: "released range", reqMin: shimwire.V1, reqMax: shimwire.V5, wantErr: true},
		{name: "interactive v6 range", reqMin: shimwire.V1, reqMax: shimwire.V6, wantErr: true},
		{name: "v5 only", reqMin: shimwire.V5, reqMax: shimwire.V5, wantErr: true},
	} {
		gotMin, gotMax, err := shimProtocolRange(WorkloadHeadless, tc.reqMin, tc.reqMax)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: headless range [%d,%d] resolved to [%d,%d], want refusal", tc.name, tc.reqMin, tc.reqMax, gotMin, gotMax)
			}
			continue
		}
		if err != nil || gotMin != tc.wantMin || gotMax != tc.wantMax {
			t.Fatalf("%s: headless range = [%d,%d] %v, want [%d,%d]", tc.name, gotMin, gotMax, err, tc.wantMin, tc.wantMax)
		}
	}
}

// TestHeadlessShimRefusesEveryLowerSelection pins the selection half: a
// controller that selects any of v1 through v5 is answered version_mismatch
// before any generation commits, the runner is untouched, and the record
// still reports no adoption. Selecting v6 then adopts normally, so the
// refusals are about the version, not a dead shim.
func TestHeadlessShimRefusesEveryLowerSelection(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	shim, registry, id := startHeadlessFixture(t, runner, 1)
	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	welcome := func(w *shimwire.Writer, selected uint32, proposed shimwire.Generation) {
		t.Helper()
		body, err := shimwire.EncodeWelcome(shimwire.Welcome{
			Protocol: shimwire.ProtocolName, Selected: selected,
			ControllerID: "lower-selection", ProposedGeneration: proposed, ResumeFrom: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(shimwire.TypeWelcome, body); err != nil {
			t.Fatalf("write Welcome selected %d: %v", selected, err)
		}
	}

	for selected := shimwire.V1; selected <= shimwire.V5; selected++ {
		_, r, w := dialRawShim(t, rec.SocketPath)
		hello, _ := readRawHello(t, r)
		welcome(w, selected, hello.Generation+1)
		reply, err := r.Read()
		if err != nil {
			t.Fatalf("selected %d: read reply: %v", selected, err)
		}
		if reply.Type != shimwire.TypeError {
			t.Fatalf("selected %d: shim answered %s, want Error{version_mismatch}", selected, reply.Type)
		}
		refusal, err := shimwire.DecodeError(reply.Body)
		if err != nil || refusal.Code != shimwire.CodeVersionMismatch {
			t.Fatalf("selected %d: refusal = (%+v,%v), want version_mismatch", selected, refusal, err)
		}
		if got := shim.Generation(); got != 0 {
			t.Fatalf("selected %d: generation committed to %d, want none", selected, got)
		}
	}
	if runner.wasStopped() {
		t.Fatal("a refused lower selection reached the runner")
	}
	// No controller holds the shim: its record still carries the armed orphan
	// deadline that only an adoption disarms.
	if current, err := registry.Get(id); err != nil || current.ShimID != rec.ShimID || current.OrphanDeadlineUnixNano == 0 {
		t.Fatalf("record after refused selections = (%+v,%v), want the same unadopted shim with its orphan deadline armed", current, err)
	}

	_, r, w := dialRawShim(t, rec.SocketPath)
	hello, _ := readRawHello(t, r)
	welcome(w, shimwire.V6, hello.Generation+1)
	reply, err := r.Read()
	if err != nil {
		t.Fatalf("selected 6: read reply: %v", err)
	}
	if reply.Type != shimwire.TypeAdopted {
		t.Fatalf("selected 6: shim answered %s, want Adopted", reply.Type)
	}
	adopted, err := shimwire.DecodeAdopted(reply.Body)
	if err != nil || adopted.Generation != hello.Generation+1 {
		t.Fatalf("selected 6: Adopted = (%+v,%v), want generation %d", adopted, err, hello.Generation+1)
	}
}

// TestAdoptQuarantinesARealHeadlessShimAsProtocolMismatch drives the
// production startup pass against a real headless shim, with and without the
// full-host-frame opt-in the daemon passes. Neither adopts it: the shim's
// [6,6] has no overlap with this build's controller range, so it is
// quarantined as protocol_mismatch, it still consumes capacity, and neither
// the shim nor its runner is touched.
func TestAdoptQuarantinesARealHeadlessShimAsProtocolMismatch(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	shim, registry, id := startHeadlessFixture(t, runner, 1)

	for _, full := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		res, err := Adopt(ctx, AdoptOptions{Registry: registry, ControllerID: "released-daemon", RequireFullHostFrames: full})
		cancel()
		if err != nil {
			t.Fatalf("Adopt(full=%v): %v", full, err)
		}
		res.Close()
		if len(res.Adopted) != 0 {
			t.Fatalf("Adopt(full=%v) adopted %d headless shims as terminal sessions, want 0", full, len(res.Adopted))
		}
		if len(res.Quarantined) != 1 || res.Quarantined[0].Reason != QuarantineProtocolMismatch {
			t.Fatalf("Adopt(full=%v) quarantined %+v, want one protocol_mismatch", full, res.Quarantined)
		}
		if q := res.Quarantined[0]; q.Identity() != id || !q.ConsumesCapacity {
			t.Fatalf("Adopt(full=%v) quarantine = %+v, want the headless identity still consuming capacity", full, q)
		}
	}
	if shim.Generation() != 0 {
		t.Fatalf("headless shim generation = %d after two refused passes, want 0", shim.Generation())
	}
	if runner.wasStopped() {
		t.Fatal("a refused adoption stopped the headless runner")
	}
	if _, err := registry.Get(id); err != nil {
		t.Fatalf("headless record after refused adoption: %v (the shim must be left running)", err)
	}
}

// scriptedPeer is a raw shim endpoint that sends exactly the Hello a test
// gives it. It reproduces shapes no shim this build starts can produce, such
// as a headless shim on an interactive range (what the transport-neutral core
// advertised before the range binding), so the refusals below are driven
// through Adopt and Dial against the wire, not a helper.
type scriptedPeer struct {
	rec Record

	mu       sync.Mutex
	welcomes []shimwire.Welcome
}

func (p *scriptedPeer) welcomeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.welcomes)
}

// scriptedPeerShape names the record and Hello a scripted peer presents.
type scriptedPeerShape struct {
	recordWorkload Workload
	recordMin      uint32
	recordMax      uint32
	helloWorkload  Workload
	helloMin       uint32
	helloMax       uint32
}

// startScriptedPeer listens on the identity's socket, writes the record as raw
// bytes (so a malformed record reaches disk exactly as a buggy writer would
// leave it), and answers every connection with the shaped Hello. A Welcome is
// recorded and answered with an Adopted that commits it, so a guard that is
// missing shows up as an adoption, not as some other failure.
func startScriptedPeer(t *testing.T, registry *Registry, id Identity, shape scriptedPeerShape) *scriptedPeer {
	t.Helper()
	socketPath := registry.SocketPath(id)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen scripted peer: %v", err)
	}
	if err := os.Chmod(socketPath, RecordFileMode); err != nil {
		t.Fatal(err)
	}
	dev, ino, err := statSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	self, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-scripted", ProcessEpoch: 1,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socketPath, SocketDevice: dev, SocketInode: ino,
		ProtocolMin: shape.recordMin, ProtocolMax: shape.recordMax,
		Phase:             shimwire.PhaseRunning,
		WorkareaPath:      "/w/scripted",
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if shape.recordWorkload == WorkloadHeadless {
		rec.SchemaVersion, rec.Workload = HeadlessRecordSchemaVersion, WorkloadHeadless
	}
	rawRecord, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry.Dir(), id.RecordName()), rawRecord, RecordFileMode); err != nil {
		t.Fatalf("write scripted record: %v", err)
	}

	helloBody, err := shimwire.EncodeHello(shimwire.Hello{
		Protocol: shimwire.ProtocolName, Min: shape.helloMin, Max: shape.helloMax,
		OrgID: id.OrgID, SessionID: id.SessionID, ShimID: rec.ShimID, ProcessEpoch: rec.ProcessEpoch,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		HarnessPID: self.PID, HarnessStartedAt: self.StartedAt,
		WorkareaPath: rec.WorkareaPath, Phase: shimwire.PhaseRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if shape.helloWorkload == WorkloadHeadless {
		// Injected after encoding: this build's codec refuses to write a
		// headless Hello on a range below 6, which is the point.
		var asMap map[string]any
		if err := json.Unmarshal(helloBody, &asMap); err != nil {
			t.Fatal(err)
		}
		asMap["extensions"] = map[string]any{"values": map[string]string{shimwire.ExtWorkload: shimwire.WorkloadHeadless}}
		if helloBody, err = json.Marshal(asMap); err != nil {
			t.Fatal(err)
		}
	}

	peer := &scriptedPeer{rec: rec}
	var conns sync.WaitGroup
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				defer func() { _ = conn.Close() }()
				peer.serve(conn, helloBody)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	})
	return peer
}

func (p *scriptedPeer) serve(conn *net.UnixConn, helloBody []byte) {
	r, w := shimwire.NewReader(conn), shimwire.NewWriter(conn)
	if err := w.Write(shimwire.TypeHello, helloBody); err != nil {
		return
	}
	msg, err := r.Read()
	if err != nil || msg.Type != shimwire.TypeWelcome {
		return
	}
	welcome, err := shimwire.DecodeWelcome(msg.Body)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.welcomes = append(p.welcomes, welcome)
	p.mu.Unlock()
	replayFrom := welcome.ResumeFrom
	if replayFrom == 0 {
		replayFrom = 1
	}
	adopted, err := shimwire.EncodeAdopted(shimwire.Adopted{
		Generation: welcome.ProposedGeneration, Contiguous: true,
		ReplayFrom: replayFrom, Phase: shimwire.PhaseRunning, Extensions: welcome.Extensions,
	})
	if err != nil {
		return
	}
	if err := w.Write(shimwire.TypeAdopted, adopted); err != nil {
		return
	}
	for {
		if _, err := r.Read(); err != nil {
			return
		}
	}
}

// TestAdoptNeverAdoptsAHeadlessShimOnAnInteractiveRange drives the production
// startup pass against the legacy headless shape: a shim that declares
// workload headless on the released range, which this build's Adopt would
// otherwise select at v5 and adopt as a terminal session. Each guard refuses
// it before any Welcome is sent, so no authority is ever proposed:
//
//   - a headless record on [1,5] is record_malformed (the record rule);
//   - an interactive record whose live Hello declares headless on [1,5] is
//     refused at the Hello decode (the codec rule);
//   - an interactive record whose live Hello declares headless on [6,6] is
//     not the shim the record described (the workload identity check).
func TestAdoptNeverAdoptsAHeadlessShimOnAnInteractiveRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		shape  scriptedPeerShape
		reason QuarantineReason
	}{
		{
			name: "headless record on the released range",
			shape: scriptedPeerShape{
				recordWorkload: WorkloadHeadless, recordMin: shimwire.V1, recordMax: shimwire.V5,
				helloWorkload: WorkloadHeadless, helloMin: shimwire.V1, helloMax: shimwire.V5,
			},
			reason: QuarantineRecordMalformed,
		},
		{
			name: "interactive record, headless Hello on the released range",
			shape: scriptedPeerShape{
				recordWorkload: WorkloadInteractive, recordMin: shimwire.V1, recordMax: shimwire.V5,
				helloWorkload: WorkloadHeadless, helloMin: shimwire.V1, helloMax: shimwire.V5,
			},
			reason: QuarantineAdoptionFailed,
		},
		{
			name: "interactive record, headless Hello on v6",
			shape: scriptedPeerShape{
				recordWorkload: WorkloadInteractive, recordMin: shimwire.V1, recordMax: shimwire.V5,
				helloWorkload: WorkloadHeadless, helloMin: shimwire.V6, helloMax: shimwire.V6,
			},
			reason: QuarantineIdentityMismatch,
		},
	}
	for _, tc := range cases {
		for _, full := range []bool{false, true} {
			name := tc.name + ", selected up to v2"
			if full {
				name = tc.name + ", full host frames"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				registry := newTestRegistry(t)
				id := Identity{OrgID: "org-legacy-headless", SessionID: "sess-legacy-headless"}
				peer := startScriptedPeer(t, registry, id, tc.shape)

				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				res, err := Adopt(ctx, AdoptOptions{Registry: registry, ControllerID: "released-daemon", RequireFullHostFrames: full})
				if err != nil {
					t.Fatalf("Adopt(full=%v): %v", full, err)
				}
				t.Cleanup(res.Close)
				if len(res.Adopted) != 0 {
					c := res.Adopted[0]
					t.Fatalf("Adopt(full=%v) adopted the headless shim as a terminal session: selected v%d hello.workload=%q",
						full, c.SelectedVersion(), c.Hello().Workload)
				}
				if len(res.Quarantined) != 1 || res.Quarantined[0].Reason != tc.reason {
					t.Fatalf("Adopt(full=%v) quarantined %+v, want one %s", full, res.Quarantined, tc.reason)
				}
				if n := peer.welcomeCount(); n != 0 {
					t.Fatalf("Adopt(full=%v) proposed authority to the headless shim %d time(s), want never", full, n)
				}
			})
		}
	}
}

// TestHeadlessControllerNeverAdoptsAnInteractivePeer pins the controller's
// workload agreement in the direction a released range cannot reach yet: an
// interactive shim that advertises [1,6] overlaps a headless controller's
// [6,6], so version selection alone would pick 6 and adopt a terminal
// session as headless. The agreement refuses it before any Welcome.
func TestHeadlessControllerNeverAdoptsAnInteractivePeer(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t)
	id := Identity{OrgID: "org-interactive-v6", SessionID: "sess-interactive-v6"}
	peer := startScriptedPeer(t, registry, id, scriptedPeerShape{
		recordWorkload: WorkloadInteractive, recordMin: shimwire.V1, recordMax: shimwire.V6,
		helloWorkload: WorkloadInteractive, helloMin: shimwire.V1, helloMax: shimwire.V6,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctrl, err := Dial(ctx, peer.rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "headless-controller"})
	if err == nil {
		_ = ctrl.Close()
		t.Fatalf("headless controller adopted an interactive peer at selected v%d", ctrl.SelectedVersion())
	}
	if !errors.Is(err, shimwire.ErrVersionMismatch) {
		t.Fatalf("Dial = %v, want ErrVersionMismatch (classified protocol_mismatch)", err)
	}
	if n := peer.welcomeCount(); n != 0 {
		t.Fatalf("headless controller proposed authority to an interactive peer %d time(s), want never", n)
	}
}

// TestControllerRefusesFramesOffItsProfile pins the controller half of the
// profile vocabulary: a headless connection fails closed on every PTY-shaped
// frame and reports no terminal capability, and an interactive connection
// fails closed on HeadlessExit instead of publishing a second terminal
// authority.
func TestControllerRefusesFramesOffItsProfile(t *testing.T) {
	t.Parallel()

	newPipeController := func(t *testing.T, profile shimwire.Profile) (*Controller, *shimwire.Writer) {
		t.Helper()
		clientConn, peerConn := net.Pipe()
		t.Cleanup(func() {
			_ = clientConn.Close()
			_ = peerConn.Close()
		})
		c := &Controller{
			w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
			gen: 3, selected: shimwire.V6, profile: profile,
			events: make(chan ControllerEvent, 64), done: make(chan struct{}), closing: make(chan struct{}),
			backlog:       newEventBacklog(0, 0, nil, nil, 0, 0),
			snapshotCalls: make(map[uint64]*snapshotCall), credentialCalls: make(map[uint64]*credentialCall),
		}
		go c.dispatchEvents()
		go c.readLoop()
		return c, shimwire.NewWriter(peerConn)
	}
	awaitMalformedEnd := func(t *testing.T, c *Controller, what string) {
		t.Helper()
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not end the stream", what)
		}
		if cause := c.StreamEndCause(); !errors.Is(cause, shimwire.ErrMalformed) {
			t.Fatalf("%s ended the stream with %v, want ErrMalformed", what, cause)
		}
		for ev := range c.Events() {
			if ev.Kind != EventError {
				t.Fatalf("%s was acted on: published %s", what, ev.Kind)
			}
		}
	}

	ptyShaped := []shimwire.MessageType{
		shimwire.TypeOutput, shimwire.TypeGap, shimwire.TypeSnapshot, shimwire.TypeExit,
		shimwire.TypeSnapshotResult, shimwire.TypeHostFrame, shimwire.TypeCheckpointResult,
	}
	for _, mt := range ptyShaped {
		t.Run("headless refuses "+mt.String(), func(t *testing.T) {
			t.Parallel()
			c, peer := newPipeController(t, shimwire.ProfileHeadless)
			if err := peer.WriteVersion(shimwire.V6, mt, []byte{0x01}); err != nil {
				t.Fatalf("peer write %s: %v", mt, err)
			}
			awaitMalformedEnd(t, c, mt.String()+" on a headless connection")
		})
	}

	t.Run("interactive refuses HeadlessExit", func(t *testing.T) {
		t.Parallel()
		c, peer := newPipeController(t, shimwire.ProfileInteractive)
		body, err := shimwire.EncodeHeadlessExit(shimwire.HeadlessExit{
			Seq: 1, ProcessEpoch: 1, Cause: shimwire.HeadlessExitCompleted,
			OutboxKey: "k", OutboxState: shimwire.HeadlessExitDelivered, GroupReaped: true, ObservedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := peer.WriteVersion(shimwire.V6, shimwire.TypeHeadlessExit, body); err != nil {
			t.Fatalf("peer write HeadlessExit: %v", err)
		}
		awaitMalformedEnd(t, c, "HeadlessExit on an interactive connection")
		if c.ObservedHeadlessExit() != nil {
			t.Fatal("an interactive connection recorded a HeadlessExit as its terminal observation")
		}
	})

	t.Run("capabilities", func(t *testing.T) {
		t.Parallel()
		headless := &Controller{selected: shimwire.V6, profile: shimwire.ProfileHeadless}
		if headless.SupportsFullHostFrames() || headless.SupportsAuthoritativeSnapshot() || headless.SupportsAttributedInput() {
			t.Fatal("a headless controller reports terminal capability")
		}
		if headless.Workload() != WorkloadHeadless {
			t.Fatalf("headless controller workload = %q", headless.Workload())
		}
		interactive := &Controller{selected: shimwire.V5}
		if !interactive.SupportsFullHostFrames() || !interactive.SupportsAuthoritativeSnapshot() || !interactive.SupportsAttributedInput() {
			t.Fatal("an interactive v5 controller lost terminal capability")
		}
		if interactive.Workload() != WorkloadInteractive {
			t.Fatalf("interactive controller workload = %q", interactive.Workload())
		}
	})
}

// TestInteractiveShimKeepsItsReleasedRangeAndHello pins the interactive side
// of the binding: a real interactive shim still advertises the build range in
// its record and Hello, carries no workload member in either, and stays
// schema 1. (Selected v1-v5 frame bytes are pinned by shimwire's goldens.)
func TestInteractiveShimKeepsItsReleasedRangeAndHello(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t)
	id := Identity{OrgID: "org-interactive-range", SessionID: "sess-interactive-range"}
	sh, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}},
		Orphan: settledOrphanPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sh.Terminate(context.Background())
		_ = sh.Close()
	})

	rawRecord, err := os.ReadFile(filepath.Join(registry.Dir(), id.RecordName()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawRecord), "workload") {
		t.Fatalf("interactive record carries a workload member: %s", rawRecord)
	}
	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.SchemaVersion != RecordSchemaVersion || rec.ProtocolMin != shimwire.ProtocolMin || rec.ProtocolMax != shimwire.ProtocolMax {
		t.Fatalf("interactive record = schema %d range [%d,%d], want schema %d range [%d,%d]",
			rec.SchemaVersion, rec.ProtocolMin, rec.ProtocolMax, RecordSchemaVersion, shimwire.ProtocolMin, shimwire.ProtocolMax)
	}

	_, r, _ := dialRawShim(t, rec.SocketPath)
	hello, raw := readRawHello(t, r)
	if hello.Min != shimwire.ProtocolMin || hello.Max != shimwire.ProtocolMax {
		t.Fatalf("interactive Hello range = [%d,%d], want [%d,%d]", hello.Min, hello.Max, shimwire.ProtocolMin, shimwire.ProtocolMax)
	}
	if hello.Workload != shimwire.ProfileInteractive || strings.Contains(string(raw), "workload") {
		t.Fatalf("interactive Hello declares a workload: workload=%q body=%s", hello.Workload, raw)
	}
}
