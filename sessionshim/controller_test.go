package sessionshim

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

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
