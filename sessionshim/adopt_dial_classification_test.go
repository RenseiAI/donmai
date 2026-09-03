package sessionshim

// Provenance: adoption-dial-classification-2026-09-03 — grep a build for this
// marker to prove a stalled shim is retried rather than reported as an
// unreachable socket.
//
// THE STRAND THIS UNDOES
//
// classifyAdoptionFailure routed any error the network stack produced to
// socket_unreachable, because its predicate asked only whether the error had a
// Timeout method — which every net.OpError and os.PathError has — and never
// called it. Measured live: a write timeout on an ALREADY-ESTABLISHED unix
// socket, to a shim whose pid had just been proved alive moments earlier, was
// classified socket_unreachable on the first re-adoption attempt.
//
// socket_unreachable is a claim about the ENDPOINT: refused, or gone. A shim
// that is merely slow to come back around its accept loop is neither, and the
// only way to tell a busy peer from an absent one is to ask again.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// stallingProxy fronts a real shim socket. It holds the FIRST connection open
// without forwarding a byte — the shape of a shim that has accepted but is not
// answering yet — and proxies every connection after that.
type stallingProxy struct {
	path        string
	connections atomic.Int32
}

func startStallingProxy(t *testing.T, path, target string, stall time.Duration) *stallingProxy {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on the interposing socket: %v", err)
	}
	proxy := &stallingProxy{path: path}
	done := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(done) })
		_ = listener.Close()
	})
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if proxy.connections.Add(1) == 1 {
				// Accepted, never answered. The controller's handshake read hits
				// its dial deadline against a socket that is unambiguously
				// established and a process that is unambiguously alive.
				go func(stalled net.Conn) {
					select {
					case <-time.After(stall):
					case <-done:
					}
					_ = stalled.Close()
				}(conn)
				continue
			}
			go proxyConnection(conn, target)
		}
	}()
	return proxy
}

func proxyConnection(client net.Conn, target string) {
	defer client.Close() //nolint:errcheck
	upstream, err := net.Dial("unix", target)
	if err != nil {
		return
	}
	defer upstream.Close() //nolint:errcheck
	go func() { _, _ = io.Copy(upstream, client) }()
	_, _ = io.Copy(client, upstream)
}

// TestAdoptionRetriesAStalledShimInsteadOfCallingItUnreachable is the RED/GREEN
// target. A live shim's record is repointed at a socket that accepts and then
// stalls the first connection for 20s — far past the dial timeout — and answers
// normally afterwards. The pass must retry and ADOPT it. Quarantining it, and
// especially quarantining it socket_unreachable, asserts the shim is gone while
// its harness is running and its socket is accepting.
func TestAdoptionRetriesAStalledShimInsteadOfCallingItUnreachable(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-stall", SessionID: "sess-stalled"}
	startInProcessShim(t, reg, dir, id, 1)

	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("read the published record: %v", err)
	}
	// Interpose in front of the shim's own socket. Everything else about the
	// record — pid, start identity, shim id, process epoch, workarea — stays
	// exactly what the live shim published, so the only thing this test changes
	// is how long the first connection takes to answer.
	proxyPath := dir + "/stall.sock"
	proxy := startStallingProxy(t, proxyPath, record.SocketPath, 20*time.Second)
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatalf("stat the interposing socket: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("socket device/inode identity is unavailable on this platform")
	}
	record.SocketPath = proxyPath
	//nolint:gosec,unconvert // Dev/Ino widths are platform-dependent; both are non-negative identifiers
	record.SocketDevice, record.SocketInode = uint64(stat.Dev), uint64(stat.Ino)
	if err := reg.Put(record); err != nil {
		t.Fatalf("republish the record against the interposing socket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := Adopt(ctx, AdoptOptions{
		Registry:     reg,
		ControllerID: "controller-stall",
		Filter:       func(candidate Identity) bool { return candidate == id },
		// Short enough that the stalled connection fails fast; the stall itself
		// outlives the whole pass.
		DialTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer result.Close()

	if len(result.Quarantined) != 0 {
		q := result.Quarantined[0]
		t.Fatalf("a stalled but live shim was quarantined %q: %s", q.Reason, q.Detail)
	}
	if len(result.Adopted) != 1 || result.Adopted[0].Identity() != id {
		t.Fatalf("adopted = %d controller(s), want the retried lineage", len(result.Adopted))
	}
	if got := proxy.connections.Load(); got < 2 {
		t.Fatalf("adoption made %d connection(s), want a retry after the stalled one", got)
	}
}

// TestAdoptionFailureClassificationSeparatesAbsentFromBusy pins the predicate
// itself, in both directions and by shape rather than by message.
func TestAdoptionFailureClassificationSeparatesAbsentFromBusy(t *testing.T) {
	t.Parallel()
	established := &net.OpError{
		Op: "write", Net: "unix",
		Addr: &net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"},
		Err:  os.ErrDeadlineExceeded,
	}
	tests := []struct {
		name          string
		err           error
		wantReason    QuarantineReason
		wantTransient bool
	}{
		{
			name:       "connect refused is the endpoint answering",
			err:        fmt.Errorf("sessionshim: dial adoption socket: %w", syscall.ECONNREFUSED),
			wantReason: QuarantineSocketUnreachable,
		},
		{
			name:       "socket file gone is the endpoint answering",
			err:        fmt.Errorf("sessionshim: stat shim socket: %w", fs.ErrNotExist),
			wantReason: QuarantineSocketUnreachable,
		},
		{
			name:          "write timeout on an established socket is a busy peer",
			err:           fmt.Errorf("shimwire: write Welcome header: %w", established),
			wantReason:    QuarantineAdoptionFailed,
			wantTransient: true,
		},
		{
			name:          "handshake deadline is a busy peer",
			err:           fmt.Errorf("sessionshim: read hello: %w", context.DeadlineExceeded),
			wantReason:    QuarantineAdoptionFailed,
			wantTransient: true,
		},
		{
			name:          "peer hung up mid-handshake is a busy peer",
			err:           fmt.Errorf("sessionshim: read hello: %w", io.ErrUnexpectedEOF),
			wantReason:    QuarantineAdoptionFailed,
			wantTransient: true,
		},
		{
			name:       "a refusal is neither, and is never retried",
			err:        fmt.Errorf("%w: shim refused at hello", ErrAdoptionRefused),
			wantReason: QuarantineIdentityMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reason, detail := classifyAdoptionFailure(test.err)
			if reason != test.wantReason {
				t.Fatalf("reason = %q, want %q", reason, test.wantReason)
			}
			if detail != test.err.Error() {
				t.Fatalf("detail = %q, want the refusal itself", detail)
			}
			if got := isTransientDialFailure(test.err); got != test.wantTransient {
				t.Fatalf("transient = %v, want %v", got, test.wantTransient)
			}
		})
	}
	if isTransientDialFailure(nil) {
		t.Fatal("a nil error was reported as a transient dial failure")
	}
	if !errors.Is(established, os.ErrDeadlineExceeded) {
		t.Fatal("the fixture's established-socket timeout does not unwrap to a deadline")
	}
}
