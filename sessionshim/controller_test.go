package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
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
		events: make(chan ControllerEvent, 64), eventQueue: make(chan ControllerEvent, selectedV3EventQueueLimit),
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

func TestSelectedV3PriorityEventQueueOverflowFailsClosed(t *testing.T) {
	controller := &Controller{
		selected:   shimwire.V3,
		eventQueue: make(chan ControllerEvent, selectedV3EventQueueLimit),
		closing:    make(chan struct{}),
	}
	for i := 0; i < selectedV3EventQueueLimit; i++ {
		if err := controller.publishEvent(ControllerEvent{Kind: EventHostFrame, Seq: uint64(i + 1)}); err != nil {
			t.Fatalf("fill priority queue at %d: %v", i, err)
		}
	}
	if err := controller.publishEvent(ControllerEvent{
		Kind: EventHostFrame, Seq: selectedV3EventQueueLimit + 1,
	}); err == nil || !strings.Contains(err.Error(), "exceeded its bound") {
		t.Fatalf("priority queue overflow = %v", err)
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
		events: make(chan ControllerEvent, 64), eventQueue: make(chan ControllerEvent, selectedV3EventQueueLimit),
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
