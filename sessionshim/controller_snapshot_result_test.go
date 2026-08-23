package sessionshim

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/shimwire"
)

func validRawPeerScreen(t *testing.T) []byte {
	t.Helper()
	screen := attachwire.Screen{
		Epoch: 1, EchoMode: attachwire.EchoUnknown,
		Cols: 1, Rows: 1, ActiveBuffer: attachwire.BufferPrimary,
		CursorVisible: true,
		Primary:       []attachwire.Cell{{RuneBytes: []byte(" "), FG: attachwire.DefaultColor, BG: attachwire.DefaultColor}},
	}
	encoded, err := screen.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func snapshotResultForRawPeer(
	req shimwire.SnapshotRequest,
	atSeq, frameSeq, relTime uint64,
	inStream bool,
	format uint8,
	screen []byte,
) shimwire.SnapshotResult {
	envelope := attachwire.SnapshotEnvelope{AtSeq: atSeq, SnapFormat: format, Snap: screen}
	frame := attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: frameSeq, RelTime: relTime, Payload: envelope.Encode()}
	return shimwire.SnapshotResult{
		RequestID: req.RequestID, Generation: req.Generation, Mode: req.Mode,
		AtSeq: atSeq, InStream: inStream, Bytes: frame.Encode(),
	}
}

func TestControllerRejectsRawPeerSnapshotResultWithoutAuthoritativeDisposition(t *testing.T) {
	t.Parallel()
	validScreen := validRawPeerScreen(t)
	exit7 := &shimwire.ExitMsg{Seq: 7, ExitCode: 0}
	tests := []struct {
		name   string
		exit   *shimwire.ExitMsg
		result func(shimwire.SnapshotRequest) shimwire.SnapshotResult
	}{
		{
			name: "pre-Exit sequence-zero direct result", result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 4, 0, 0, false, attachwire.SnapFormatScreen, validScreen)
			},
		},
		{
			name: "post-Exit wrong atSeq", exit: exit7, result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 6, 0, 0, false, attachwire.SnapFormatScreen, validScreen)
			},
		},
		{
			name: "post-Exit nonzero relTime", exit: exit7, result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 7, 0, 1, false, attachwire.SnapFormatScreen, validScreen)
			},
		},
		{
			name: "non-screen format", result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 4, 5, 1, true, 2, validScreen)
			},
		},
		{
			name: "invalid encoded screen", result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 4, 5, 1, true, attachwire.SnapFormatScreen, []byte{0xff})
			},
		},
		{
			name: "live result after Exit", exit: exit7, result: func(req shimwire.SnapshotRequest) shimwire.SnapshotResult {
				return snapshotResultForRawPeer(req, 7, 8, 1, true, attachwire.SnapFormatScreen, validScreen)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller := dialRawSnapshotPeer(t, tc.exit, tc.result)
			if tc.exit != nil {
				select {
				case event := <-controller.Events():
					if event.Kind != EventExit || event.Exit != *tc.exit {
						t.Fatalf("first event = %+v, want Exit %+v", event, *tc.exit)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for ordered Exit observation")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := controller.EmitSnapshot(ctx); !errors.Is(err, shimwire.ErrSnapshotMismatch) {
				t.Fatalf("EmitSnapshot = %v, want ErrSnapshotMismatch", err)
			}
		})
	}
}

func dialRawSnapshotPeer(
	t *testing.T,
	exit *shimwire.ExitMsg,
	result func(shimwire.SnapshotRequest) shimwire.SnapshotResult,
) *Controller {
	t.Helper()
	registry, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-raw-result", SessionID: "session-raw-result"}
	socketPath := registry.SocketPath(id)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socketPath, RecordFileMode); err != nil {
		t.Fatal(err)
	}
	device, inode, err := statSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	self, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID, ShimID: "shim-raw-result", ProcessEpoch: 1,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socketPath, SocketDevice: device, SocketInode: inode,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2, Phase: shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
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
		helloBody, encodeErr := shimwire.EncodeHello(shimwire.Hello{
			Protocol: shimwire.ProtocolName, Min: shimwire.V1, Max: shimwire.V2,
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: record.ShimID,
			ProcessEpoch: record.ProcessEpoch, PID: self.PID, ProcessStartedAt: self.StartedAt,
			Phase: shimwire.PhaseRunning, Generation: 2,
		})
		if encodeErr != nil {
			serverErr <- encodeErr
			return
		}
		if writeErr := writer.Write(shimwire.TypeHello, helloBody); writeErr != nil {
			serverErr <- writeErr
			return
		}
		welcomeMessage, readErr := reader.Read()
		if readErr != nil {
			serverErr <- readErr
			return
		}
		welcome, decodeErr := shimwire.DecodeWelcome(welcomeMessage.Body)
		if decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		adoptedBody, encodeErr := shimwire.EncodeAdopted(shimwire.Adopted{
			Generation: welcome.ProposedGeneration, Extensions: welcome.Extensions,
			Contiguous: true, Phase: shimwire.PhaseRunning,
		})
		if encodeErr != nil {
			serverErr <- encodeErr
			return
		}
		if writeErr := writer.Write(shimwire.TypeAdopted, adoptedBody); writeErr != nil {
			serverErr <- writeErr
			return
		}
		if exit != nil {
			exitBody, exitErr := shimwire.EncodeExit(*exit)
			if exitErr != nil {
				serverErr <- exitErr
				return
			}
			if writeErr := writer.WriteVersion(shimwire.V2, shimwire.TypeExit, exitBody); writeErr != nil {
				serverErr <- writeErr
				return
			}
		}
		requestMessage, readErr := reader.ReadVersion(shimwire.V2)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		request, decodeErr := shimwire.DecodeSnapshotRequest(requestMessage.Body)
		if decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		resultBody, encodeErr := shimwire.EncodeSnapshotResult(result(request))
		if encodeErr != nil {
			serverErr <- encodeErr
			return
		}
		serverErr <- writer.WriteVersion(shimwire.V2, shimwire.TypeSnapshotResult, resultBody)
	}()
	controller, err := Dial(context.Background(), record, ControllerOptions{ControllerID: "controller-raw-result"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controller.Close()
		select {
		case err := <-serverErr:
			if err != nil {
				t.Errorf("raw snapshot peer: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("raw snapshot peer did not finish")
		}
	})
	return controller
}
