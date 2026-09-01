package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestControllerProtocolRangeRequiresExplicitFullFrameConsumption(t *testing.T) {
	t.Parallel()
	protocolMin, protocolMax, err := (ControllerOptions{}).protocolRange()
	if err != nil || protocolMin != shimwire.V1 || protocolMax != shimwire.V2 {
		t.Fatalf("zero-value controller range = [%d,%d], %v; want released [1,2]", protocolMin, protocolMax, err)
	}
	protocolMin, protocolMax, err = (ControllerOptions{RequireFullHostFrames: true}).protocolRange()
	if err != nil || protocolMin != shimwire.V1 || protocolMax != shimwire.V3 {
		t.Fatalf("full-frame controller range = [%d,%d], %v; want [1,3]", protocolMin, protocolMax, err)
	}
	if _, _, err := (ControllerOptions{ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3}).protocolRange(); err == nil {
		t.Fatal("controller advertised max 3 without declaring full HostFrame consumption")
	}
	if _, _, err := (ControllerOptions{
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2, RequireFullHostFrames: true,
	}).protocolRange(); err == nil {
		t.Fatal("controller declared full HostFrame consumption while capped below v3")
	}
}

func TestVerifyHelloRequiresExactNestedRootButAllowsDegenerateLegacyOmission(t *testing.T) {
	hello := shimwire.Hello{
		Protocol: shimwire.ProtocolName, OrgID: "org", SessionID: "session", ShimID: "shim",
		PID: 42, ProcessStartedAt: 99, Phase: shimwire.PhaseRunning, WorkareaPath: "/work/root/repo",
	}
	record := Record{
		OrgID: "org", SessionID: "session", ShimID: "shim", PID: 42, ProcessStartedAt: 99,
		WorkareaPath: "/work/root/repo",
	}
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("nested missing-root verification = %v", err)
	}
	record.WorkareaRoot = "/work/root"
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); err != nil {
		t.Fatalf("exact nested root refused: %v", err)
	}
	record.WorkareaRoot = "/work/other"
	if err := verifyHello(hello, record, "/work/root/repo", "/work/root"); !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("wrong nested root verification = %v", err)
	}
	record.WorkareaRoot = ""
	hello.WorkareaPath = "/work/legacy"
	record.WorkareaPath = "/work/legacy"
	if err := verifyHello(hello, record, "/work/legacy", "/work/legacy"); err != nil {
		t.Fatalf("degenerate legacy omission refused: %v", err)
	}
}

func TestSelectedV3HeartbeatReceiptBypassesFullPublicEventBuffer(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 7, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 64), backlog: newEventBacklog(0),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.dispatchEvents()
	go controller.readLoop()
	peerDone := make(chan error, 1)
	burstWritten := make(chan struct{})
	allowReceipt := make(chan struct{})
	go func() {
		defer shimConn.Close() //nolint:errcheck
		reader, writer := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
		message, err := reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("heartbeat request = %s, %v", message.Type, err)
			return
		}
		heartbeat, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		const burst = 80 // deliberately greater than the public 64-event buffer
		for sequence := uint64(1); sequence <= burst; sequence++ {
			frame := attachwire.Frame{Type: attachwire.TypeOutput, Seq: sequence, Payload: []byte{byte(sequence)}}
			body, encodeErr := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: frame.Encode()})
			if encodeErr != nil {
				peerDone <- encodeErr
				return
			}
			if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
				peerDone <- err
				return
			}
		}
		close(burstWritten)
		<-allowReceipt
		receipt, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: heartbeat.Generation, AckedSeq: heartbeat.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		peerDone <- writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, receipt)
	}()

	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- controller.Heartbeat(0) }()
	<-burstWritten
	select {
	case err := <-heartbeatDone:
		t.Fatalf("Heartbeat returned before its persistence receipt: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowReceipt)
	select {
	case err := <-heartbeatDone:
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Heartbeat deadlocked behind the full public event buffer")
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	var sequences []uint64
	for event := range controller.Events() {
		sequences = append(sequences, event.Seq)
	}
	if len(sequences) != 80 {
		t.Fatalf("priority queue delivered %d events, want 80", len(sequences))
	}
	for index, sequence := range sequences {
		if sequence != uint64(index+1) {
			t.Fatalf("priority queue sequence[%d] = %d, want %d", index, sequence, index+1)
		}
	}
}

// TestEventBacklogBudgetMatchesTheShimRing pins the equality that keeps the
// daemon from being the first component to give up on a burst.
//
// Both numbers answer the same question - how much host output may be in flight
// before this system admits it has lost some. When they disagreed (a 192-frame
// controller bound against an 8 MiB ring) the controller collapsed on volume
// the shim absorbs by design, and the Gap the ring exists to declare became
// unreachable. Sourcing one from the other is what makes that impossible.
func TestEventBacklogBudgetMatchesTheShimRing(t *testing.T) {
	t.Parallel()
	if EventBacklogBudget != ptyhost.DefaultRingBytes {
		t.Fatalf("event backlog budget = %d, want the shim ring budget %d",
			EventBacklogBudget, ptyhost.DefaultRingBytes)
	}
	if publicEventBufferLimit != 64 {
		t.Fatalf("public event buffer = %d, want 64", publicEventBufferLimit)
	}
}

func TestEventBacklogBudgetOverflowFailsClosed(t *testing.T) {
	t.Parallel()
	const payload = 100
	budget := 4 * (eventBacklogOverheadBytes + payload)
	controller := &Controller{
		selected: shimwire.V3,
		backlog:  newEventBacklog(budget),
		closing:  make(chan struct{}),
	}
	for i := range 4 {
		event := ControllerEvent{Kind: EventHostFrame, Seq: uint64(i + 1), FrameBytes: make([]byte, payload)}
		if err := controller.publishEvent(event); err != nil {
			t.Fatalf("fill backlog at %d: %v", i, err)
		}
	}
	overflow := ControllerEvent{Kind: EventHostFrame, Seq: 5, FrameBytes: make([]byte, payload)}
	if err := controller.publishEvent(overflow); !errors.Is(err, ErrEventBacklogExceeded) {
		t.Fatalf("backlog overflow = %v, want ErrEventBacklogExceeded", err)
	}
	// Bytes, not frames: draining one event makes room again.
	if _, ok := controller.backlog.pop(); !ok {
		t.Fatal("backlog drained empty")
	}
	if err := controller.publishEvent(overflow); err != nil {
		t.Fatalf("backlog refused an event that fits after draining: %v", err)
	}
	if got := controller.backlog.queuedBytes(); got != budget {
		t.Fatalf("queued bytes = %d, want %d", got, budget)
	}
}

// TestEventBacklogAcceptsOneOversizedEvent pins the ring's own rule: a single
// frame larger than the whole budget is still retained when nothing else is
// queued, because refusing it would strand a session on one big redraw.
func TestEventBacklogAcceptsOneOversizedEvent(t *testing.T) {
	t.Parallel()
	backlog := newEventBacklog(128)
	if err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 1, FrameBytes: make([]byte, 4096)}); err != nil {
		t.Fatalf("oversized first event refused: %v", err)
	}
	if err := backlog.push(ControllerEvent{Kind: EventHostFrame, Seq: 2}); !errors.Is(err, ErrEventBacklogExceeded) {
		t.Fatalf("second event after an oversized one = %v, want refusal", err)
	}
}

// TestSelectedV3HeartbeatReceiptTimeoutKeepsTheController replaces the pin that
// used to require the OPPOSITE — that a receipt timeout drop the connection.
//
// Measured on an installed host: a durable write that was merely slow took the
// persistence receipt past the wait bound, this controller dropped the shim
// connection over it, nothing re-adopted the shim, and it reaped its own live
// harness when its orphan deadline expired. Twice, in the same minute, on two
// healthy sessions. "The receipt has not arrived yet" is a statement about how
// fast the durable side is answering; it is not evidence that this socket is
// broken, and it is never a reason to unsupervise a running harness.
//
// So the bound now bounds ONE CALLER'S WAIT: it reports the receipt pending,
// keeps the stream, and a receipt that lands late is consumed as the answer it
// is rather than as the unsolicited frame that WOULD be a reason to drop. The
// cursor still does not advance — the shim has not said it stored that
// sequence — which is what makes retrying safe rather than optimistic.
func TestSelectedV3HeartbeatReceiptTimeoutKeepsTheController(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 11, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 1), backlog: newEventBacklog(0),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.readLoop()

	peerDone := make(chan error, 1)
	releaseLateReceipt := make(chan struct{})
	go func() {
		defer shimConn.Close() //nolint:errcheck
		reader, writer := shimwire.NewReader(shimConn), shimwire.NewWriter(shimConn)
		// The slow one: read the request and answer nothing until released.
		message, err := reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("first heartbeat request = %s, %v", message.Type, err)
			return
		}
		first, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		<-releaseLateReceipt
		late, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: first.Generation, AckedSeq: first.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, late); err != nil {
			peerDone <- err
			return
		}
		// The retry: answered promptly, proving the stream survived the stall.
		message, err = reader.ReadVersion(shimwire.V3)
		if err != nil || message.Type != shimwire.TypeHeartbeat {
			peerDone <- fmt.Errorf("retried heartbeat request = %s, %v", message.Type, err)
			return
		}
		retried, err := shimwire.DecodeHeartbeat(message.Body)
		if err != nil {
			peerDone <- err
			return
		}
		receipt, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Generation: retried.Generation, AckedSeq: retried.AckedSeq, Phase: shimwire.PhaseRunning,
		})
		peerDone <- writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, receipt)
	}()

	started := time.Now()
	err := controller.Heartbeat(1)
	if !errors.Is(err, ErrHeartbeatReceiptPending) {
		t.Fatalf("heartbeat with an unanswered receipt = %v, want ErrHeartbeatReceiptPending", err)
	}
	if waited := time.Since(started); waited < heartbeatReceiptWaitBound {
		t.Fatalf("heartbeat reported the receipt pending after %s, want at least the %s wait bound",
			waited, heartbeatReceiptWaitBound)
	}
	// THE POINT: the shim connection is still up. A closed one here is the
	// measured regression, and every later assertion would be unreachable.
	select {
	case <-controller.closing:
		t.Fatal("a pending persistence receipt dropped the shim connection")
	case <-controller.Done():
		t.Fatal("a pending persistence receipt ended the controller read loop")
	default:
	}

	// The late receipt is the answer to a heartbeat this controller really
	// sent; consuming it must not be read as an unsolicited frame.
	close(releaseLateReceipt)

	if err := controller.Heartbeat(2); err != nil {
		t.Fatalf("heartbeat retried after a pending receipt: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("peer transport: %v", err)
	}
}

func TestSelectedV3RejectsHeartbeatInterposedInsideLiveSnapshotPair(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	call := &snapshotCall{
		request: shimwire.SnapshotRequest{RequestID: 77, Generation: 7, Mode: shimwire.SnapshotEmit},
		done:    make(chan struct{}),
	}
	controller := &Controller{
		w: shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 7, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		events: make(chan ControllerEvent, 64), backlog: newEventBacklog(0),
		done: make(chan struct{}), closing: make(chan struct{}), snapshotCalls: map[uint64]*snapshotCall{77: call},
	}
	go controller.dispatchEvents()
	go controller.readLoop()
	frame := attachwire.Frame{
		Type: attachwire.TypeSnapshot, Seq: 1,
		Payload: (attachwire.SnapshotEnvelope{
			AtSeq: 0, SnapFormat: attachwire.SnapFormatScreen, Snap: []byte{1},
		}).Encode(),
	}
	body, err := shimwire.EncodeHostFrame(shimwire.HostFrame{RequestID: 77, FrameBytes: frame.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	writer := shimwire.NewWriter(shimConn)
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
		t.Fatal(err)
	}
	heartbeat, _ := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: 7, AckedSeq: 0, Phase: shimwire.PhaseRunning})
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHeartbeat, heartbeat); err != nil {
		t.Fatal(err)
	}
	select {
	case <-controller.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("interposed Heartbeat did not terminate the malformed pair")
	}
	if !errors.Is(call.err, shimwire.ErrSnapshotMismatch) {
		t.Fatalf("interposed Heartbeat snapshot error = %v", call.err)
	}
	if event, ok := <-controller.Events(); ok {
		t.Fatalf("partial requested HostFrame escaped before its result: %+v", event)
	}
	_ = shimConn.Close()
}

func TestValidateAdoptionCommitRequiresExactGenerationAndExtensions(t *testing.T) {
	t.Parallel()

	wantExtensions := shimwire.Extensions{
		Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
		Required: []string{shimwire.ExtCarrierEpoch},
	}
	tests := []struct {
		name    string
		adopted shimwire.Adopted
		wantErr bool
	}{
		{name: "exact", adopted: shimwire.Adopted{Generation: 7, Extensions: wantExtensions}},
		{name: "higher generation", adopted: shimwire.Adopted{Generation: 8, Extensions: wantExtensions}, wantErr: true},
		{name: "omitted extension echo", adopted: shimwire.Adopted{Generation: 7}, wantErr: true},
		{name: "changed carrier epoch", adopted: shimwire.Adopted{
			Generation: 7,
			Extensions: shimwire.Extensions{
				Values:   map[string]string{shimwire.ExtCarrierEpoch: "20"},
				Required: []string{shimwire.ExtCarrierEpoch},
			},
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateAdoptionCommit(tc.adopted, 7, wantExtensions)
			if tc.wantErr && !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("validateAdoptionCommit = %v, want ErrAdoptionRefused", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateAdoptionCommit exact echo: %v", err)
			}
		})
	}
}

func TestDialRefusesInexactAdoptionCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*shimwire.Adopted)
		wantErr bool
	}{
		{name: "exact"},
		{name: "higher generation", mutate: func(adopted *shimwire.Adopted) { adopted.Generation++ }, wantErr: true},
		{name: "omitted extension echo", mutate: func(adopted *shimwire.Adopted) { adopted.Extensions = shimwire.Extensions{} }, wantErr: true},
		{name: "changed extension echo", mutate: func(adopted *shimwire.Adopted) {
			adopted.Extensions.Values[shimwire.ExtCarrierEpoch] = "20"
		}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := dialFakeAdoptionCommit(t, tc.mutate)
			if tc.wantErr && !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("Dial = %v, want ErrAdoptionRefused", err)
			}
			if tc.wantErr {
				generation, ok := authenticatedHelloGeneration(err)
				if !ok || generation != 6 {
					t.Fatalf("authenticated Hello generation = %d/%v, want 6/true", generation, ok)
				}
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Dial exact commit: %v", err)
			}
		})
	}
}

func dialFakeAdoptionCommit(t *testing.T, mutate func(*shimwire.Adopted)) error {
	t.Helper()
	registry, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-fake", SessionID: "session-fake"}
	socketPath := registry.SocketPath(id)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socketPath, RecordFileMode); err != nil {
		t.Fatalf("chmod socket: %v", err)
	}
	device, inode, err := statSocket(socketPath)
	if err != nil {
		t.Fatalf("statSocket: %v", err)
	}
	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-fake", ProcessEpoch: 4,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socketPath, SocketDevice: device, SocketInode: inode,
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: time.Now().UnixNano(),
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		writer, reader := shimwire.NewWriter(conn), shimwire.NewReader(conn)
		hello := shimwire.Hello{
			Protocol: shimwire.ProtocolName, Min: shimwire.ProtocolMin, Max: shimwire.ProtocolMax,
			OrgID: id.OrgID, SessionID: id.SessionID,
			ShimID: record.ShimID, ProcessEpoch: record.ProcessEpoch,
			PID: self.PID, ProcessStartedAt: self.StartedAt,
			Phase: shimwire.PhaseRunning, Generation: 6,
		}
		if writeErr := writeTyped(writer, shimwire.TypeHello, func() ([]byte, error) { return shimwire.EncodeHello(hello) }); writeErr != nil {
			serverErr <- writeErr
			return
		}
		message, readErr := reader.Read()
		if readErr != nil {
			serverErr <- readErr
			return
		}
		welcome, decodeErr := shimwire.DecodeWelcome(message.Body)
		if decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		adopted := shimwire.Adopted{
			Generation: welcome.ProposedGeneration,
			Extensions: welcome.Extensions,
			Contiguous: true,
			Phase:      shimwire.PhaseRunning,
		}
		if mutate != nil {
			adopted.Extensions.Values = cloneStringMap(adopted.Extensions.Values)
			mutate(&adopted)
		}
		serverErr <- writeTyped(writer, shimwire.TypeAdopted, func() ([]byte, error) { return shimwire.EncodeAdopted(adopted) })
	}()
	extensions := shimwire.Extensions{
		Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
		Required: []string{shimwire.ExtCarrierEpoch},
	}
	controller, dialErr := Dial(context.Background(), record, ControllerOptions{
		ControllerID:       "controller-fake",
		ProposedGeneration: 7,
		Extensions:         extensions,
	})
	if controller != nil {
		_ = controller.Close()
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake shim server: %v", err)
	}
	return dialErr
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// TestFailClosedStreamDropNamesItsReason pins the diagnosis half of a real
// field failure: a controller that drops its own connection must say why.
//
// Every fail-closed decision in the read loop used to close the socket
// silently. From every later caller's side that is indistinguishable from a
// peer that went away — input, resize, and the durable heartbeat all come back
// with "use of closed network connection", minutes later, naming nothing. The
// one operator-visible line was an acknowledgement failure for a frame the
// daemon had already accepted, which points at the wrong layer entirely.
//
// Restoring the bare `_ = c.Close()` in readLoop turns this RED.
func TestFailClosedStreamDropNamesItsReason(t *testing.T) {
	clientConn, shimConn := net.Pipe()
	defer clientConn.Close() //nolint:errcheck
	defer shimConn.Close()   //nolint:errcheck

	var log strings.Builder
	controller := &Controller{
		id: Identity{OrgID: "org-drop", SessionID: "session-drop"},
		w:  shimwire.NewWriter(clientConn), r: shimwire.NewReader(clientConn),
		gen: 3, selected: shimwire.V3, adopted: shimwire.Adopted{ReplayFrom: 1},
		// A one-byte budget makes the bound reachable without writing its full
		// production depth; the decision under test is the same one. Nothing
		// drains it: dispatchEvents is deliberately not started.
		events: make(chan ControllerEvent), backlog: newEventBacklog(1),
		logger:        slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})),
		done:          make(chan struct{}),
		closing:       make(chan struct{}),
		snapshotCalls: make(map[uint64]*snapshotCall),
	}
	go controller.readLoop()

	frame := attachwire.Frame{
		Type: attachwire.TypeOutput, Seq: 1,
		Payload: attachwire.EncodeOutput([]byte("x")),
	}
	body, err := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: frame.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	writer := shimwire.NewWriter(shimConn)
	if err := writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, body); err != nil {
		t.Fatal(err)
	}
	// The first frame is retained even oversized (the ring's own rule); the
	// second is what exceeds the budget.
	second := attachwire.Frame{Type: attachwire.TypeOutput, Seq: 2, Payload: attachwire.EncodeOutput([]byte("y"))}
	secondBody, err := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: second.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = writer.WriteVersion(shimwire.V3, shimwire.TypeHostFrame, secondBody) }()
	select {
	case <-controller.closing:
	case <-time.After(5 * time.Second):
		t.Fatal("fail-closed queue bound did not drop the connection")
	}
	<-controller.Done()
	line := log.String()
	if !strings.Contains(line, "controller dropped its shim connection") ||
		!strings.Contains(line, "org-drop/session-drop") ||
		!strings.Contains(line, "exceeded the in-flight budget") {
		t.Fatalf("fail-closed drop log = %q, want the session and the exact reason", line)
	}
}
