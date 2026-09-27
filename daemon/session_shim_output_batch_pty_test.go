package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

const batchPTYControlEnv = "DONMAI_TEST_BATCH_PTY_CONTROL"

// TestShimOutputBatchPTYHelper is reexecuted as the real PTY child. Readiness
// and output commands use a separate socket, never terminal input/echo. The
// child emits exactly one non-control byte per command and no test-runner text.
func TestShimOutputBatchPTYHelper(_ *testing.T) {
	socket := os.Getenv(batchPTYControlEnv)
	if socket == "" {
		return
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		os.Exit(2)
	}
	var b [1]byte
	for {
		if _, err = io.ReadFull(conn, b[:]); err != nil {
			os.Exit(0)
		}
		if _, err = os.Stdout.Write(b[:]); err != nil {
			os.Exit(3)
		}
	}
}

type batchPTYSource struct {
	listener *net.UnixListener
	conn     net.Conn
}

func newBatchPTYSource(ctx context.Context, t *testing.T, dir string) (*batchPTYSource, ptyhost.Spec) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "output-control.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if deadline, ok := ctx.Deadline(); ok {
		if err = listener.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := &batchPTYSource{listener: listener}
	t.Cleanup(func() {
		if source.conn != nil {
			_ = source.conn.Close()
		}
	})
	return source, ptyhost.Spec{Command: []string{binary, "-test.run=^TestShimOutputBatchPTYHelper$"}, Env: []string{batchPTYControlEnv + "=" + listener.Addr().String()}}
}

func (s *batchPTYSource) emit(ctx context.Context, t *testing.T, frames <-chan attachwire.Frame, b byte) []byte {
	t.Helper()
	if s.conn == nil {
		conn, err := s.listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		s.conn = conn
		if deadline, ok := ctx.Deadline(); ok {
			if err = conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
		}
	}
	if b < 'a' || b > 'z' {
		t.Fatal("fixture output must be one printable non-control byte")
	}
	if _, err := s.conn.Write([]byte{b}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("PTY source ended before its output")
		}
		if frame.Type != attachwire.TypeOutput {
			t.Fatalf("source frame type=%v, want output", frame.Type)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != b {
			t.Fatalf("source output = %q, want exact byte %q", frame.Payload, b)
		}
		// The next command is issued only after this exact real frame is consumed.
		// A single byte cannot fragment, and no next byte exists yet to coalesce.
		return frame.Encode()
	case <-ctx.Done():
		t.Fatal("source did not produce6 exact frames")
	}
	return nil
}
