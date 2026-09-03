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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
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

// postHelloStallProxy also fronts a real shim socket, but its stalled
// connection forwards the shim's Hello to the controller and then forwards
// nothing back. Preparation runs INSIDE the handshake, after Hello is
// authenticated and before the Welcome write, so this is the shape that reaches
// preparation and then times out — the shape the measured-live failure had.
type postHelloStallProxy struct {
	connections atomic.Int32

	mu       sync.Mutex
	previous func()
}

func (p *postHelloStallProxy) releasePrevious() {
	p.mu.Lock()
	release := p.previous
	p.previous = nil
	p.mu.Unlock()
	if release != nil {
		release()
	}
}

func (p *postHelloStallProxy) retainPrevious(release func()) {
	p.mu.Lock()
	p.previous = release
	p.mu.Unlock()
}

func startPostHelloStallProxy(t *testing.T, path, target string, stallThrough int32) *postHelloStallProxy {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on the interposing socket: %v", err)
	}
	proxy := &postHelloStallProxy{}
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.releasePrevious()
	})
	go func() {
		for {
			client, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// Release the stalled pair before the next handshake begins, so the
			// shim is never asked to hold two controller connections at once.
			proxy.releasePrevious()
			if proxy.connections.Add(1) > stallThrough {
				go proxyConnection(client, target)
				continue
			}
			upstream, dialErr := net.Dial("unix", target)
			if dialErr != nil {
				_ = client.Close()
				continue
			}
			// Shim to controller only: Hello arrives and preparation runs; the
			// controller's Welcome is never delivered, so its read of Adopted
			// hits the dial deadline on an established socket.
			go func() { _, _ = io.Copy(client, upstream) }()
			proxy.retainPrevious(func() {
				_ = client.Close()
				_ = upstream.Close()
			})
		}
	}()
	return proxy
}

// repointRecordAtSocket republishes id's record against an interposing socket,
// leaving every other field the live shim published exactly as it is.
func repointRecordAtSocket(t *testing.T, reg *Registry, id Identity, socketPath string) {
	t.Helper()
	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("read the published record: %v", err)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat the interposing socket: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("socket device/inode identity is unavailable on this platform")
	}
	record.SocketPath = socketPath
	//nolint:gosec,unconvert // Dev/Ino widths are platform-dependent; both are non-negative identifiers
	record.SocketDevice, record.SocketInode = uint64(stat.Dev), uint64(stat.Ino)
	if err := reg.Put(record); err != nil {
		t.Fatalf("republish the record against the interposing socket: %v", err)
	}
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
	repointRecordAtSocket(t, reg, id, proxyPath)

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
			// The discriminating case. A net.OpError HAS a Timeout method, so a
			// predicate that only asks whether the method exists calls this
			// transient. Its errno says otherwise: the peer reset an established
			// connection. Neither absent nor merely busy — classify it by what
			// it says, not by what interface it happens to satisfy.
			name: "a reset established connection is neither absent nor busy",
			err: fmt.Errorf("sessionshim: read hello: %w", &net.OpError{
				Op: "read", Net: "unix",
				Addr: &net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"},
				Err:  syscall.ECONNRESET,
			}),
			wantReason: QuarantineAdoptionFailed,
		},
		{
			// Same shape, same reasoning, the other way: a refused connect also
			// arrives inside a net.OpError, and its errno is what makes it the
			// endpoint answering rather than a slow one.
			name: "a refused connect inside an OpError is still the endpoint answering",
			err: fmt.Errorf("sessionshim: dial adoption socket: %w", &net.OpError{
				Op: "dial", Net: "unix",
				Addr: &net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"},
				Err:  syscall.ECONNREFUSED,
			}),
			wantReason: QuarantineSocketUnreachable,
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

// TestTransientDialRetryDoesNotReprepareTheCandidate pins B4's rule: a retry
// re-dials the ALREADY-PREPARED candidate.
//
// Preparation runs inside the handshake, after Hello authentication and before
// the Welcome write, so the failure shape this retry exists for happens AFTER
// preparation has already succeeded. Asking again would mint a second
// control-plane reservation for a lineage whose first one is admitted and
// undisposed — on a path with no drift to repair and no abandonment verb to
// call. The first answer must be retained and replayed.
func TestTransientDialRetryDoesNotReprepareTheCandidate(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := Identity{OrgID: "org-reprepare", SessionID: "sess-prepared-once"}
	startInProcessShim(t, reg, dir, id, 1)

	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("read the published record: %v", err)
	}
	proxyPath := dir + "/hello.sock"
	proxy := startPostHelloStallProxy(t, proxyPath, record.SocketPath, 1)
	repointRecordAtSocket(t, reg, id, proxyPath)

	var (
		prepareMu sync.Mutex
		prepares  int
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := Adopt(ctx, AdoptOptions{
		Registry:     reg,
		ControllerID: "controller-reprepare",
		Filter:       func(candidate Identity) bool { return candidate == id },
		DialTimeout:  300 * time.Millisecond,
		Prepare: func(context.Context, AdoptionPreparation) (PreparedAdoption, error) {
			prepareMu.Lock()
			defer prepareMu.Unlock()
			prepares++
			return PreparedAdoption{Correlation: []byte("candidate-1")}, nil
		},
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer result.Close()

	if len(result.Quarantined) != 0 {
		q := result.Quarantined[0]
		t.Fatalf("a shim that stalled after Hello was quarantined %q: %s", q.Reason, q.Detail)
	}
	if len(result.Adopted) != 1 {
		t.Fatalf("adopted = %d controller(s), want the retried lineage", len(result.Adopted))
	}
	if got := proxy.connections.Load(); got < 2 {
		t.Fatalf("adoption made %d connection(s), want a retry after the post-Hello stall", got)
	}
	prepareMu.Lock()
	defer prepareMu.Unlock()
	if prepares != 1 {
		t.Fatalf("preparations = %d across %d dials, want exactly 1 — a retry must not mint a second reservation",
			prepares, proxy.connections.Load())
	}
}

// TestAdoptionRetryDialTimeoutIsCapped pins B3's bound. Adopt walks records in
// one sequential loop, so a hung record's cost is paid by every lineage behind
// it. The first attempt keeps the caller's timeout — a first dial is not
// evidence of anything yet — and every retry is capped, so the worst case for
// one record is bounded at first + 2s + 2s rather than three full timeouts.
func TestAdoptionRetryDialTimeoutIsCapped(t *testing.T) {
	t.Parallel()
	const defaultDialTimeout = 5 * time.Second
	tests := []struct {
		name       string
		configured time.Duration
		attempt    int
		want       time.Duration
	}{
		{name: "first attempt keeps the default", configured: 0, attempt: 1, want: 0},
		{name: "first attempt keeps a configured value", configured: 9 * time.Second, attempt: 1, want: 9 * time.Second},
		{name: "retry caps the default", configured: 0, attempt: 2, want: adoptionRetryDialTimeout},
		{name: "retry caps a longer configured value", configured: 9 * time.Second, attempt: 3, want: adoptionRetryDialTimeout},
		{name: "retry never raises a shorter one", configured: 250 * time.Millisecond, attempt: 2, want: 250 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := adoptionAttemptDialTimeout(test.configured, test.attempt); got != test.want {
				t.Fatalf("dial timeout = %s, want %s", got, test.want)
			}
		})
	}
	// The bound is pinned by value: the worst case one hung record can cost the
	// serialized pass must stay legible next to a shim orphan deadline.
	if adoptionRetryDialTimeout != 2*time.Second {
		t.Fatalf("retry dial timeout = %s, want 2s", adoptionRetryDialTimeout)
	}
	worst := defaultDialTimeout
	for attempt := 2; attempt <= adoptionDialAttempts; attempt++ {
		worst += adoptionAttemptDialTimeout(0, attempt)
	}
	if worst != 9*time.Second {
		t.Fatalf("worst case for one hung record = %s, want 9s (5+2+2)", worst)
	}
}

// TestResolvePreparedAdoptionValidatesEveryAnswerTheSameWay pins the extracted
// resolver both the handshake and a later re-prepare call. A preparation answer
// that reached the wire through these checks and one that reached a durable
// receipt through none is how a raised floor gets silently dropped.
func TestResolvePreparedAdoptionValidatesEveryAnswerTheSameWay(t *testing.T) {
	t.Parallel()
	cursor := func(v uint64) *uint64 { return &v }
	tests := []struct {
		name     string
		prepared PreparedAdoption
		bounds   PreparedAdoptionBounds
		wantErr  string
		want     ResolvedPreparedAdoption
	}{
		{
			name:     "no answer resolves to nothing",
			prepared: PreparedAdoption{},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
		},
		{
			name:     "a raised cursor is accepted",
			prepared: PreparedAdoption{ResumeFrom: cursor(7)},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
			want:     ResolvedPreparedAdoption{ResumeFrom: 7, ResumeProvided: true},
		},
		{
			name:     "the successor of Hello LastSeq is the ceiling",
			prepared: PreparedAdoption{ResumeFrom: cursor(10)},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
			want:     ResolvedPreparedAdoption{ResumeFrom: 10, ResumeProvided: true},
		},
		{
			name:     "a regressed cursor refuses",
			prepared: PreparedAdoption{ResumeFrom: cursor(3)},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
			wantErr:  "regresses local floor",
		},
		{
			name:     "a cursor past Hello LastSeq+1 refuses",
			prepared: PreparedAdoption{ResumeFrom: cursor(11)},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
			wantErr:  "is ahead of Hello LastSeq",
		},
		{
			name:     "an unset Hello cursor bounds nothing, so it refuses",
			prepared: PreparedAdoption{ResumeFrom: cursor(5)},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: ^uint64(0)},
			wantErr:  "is ahead of Hello LastSeq",
		},
		{
			name:     "a prepared cursor against a static one refuses",
			prepared: PreparedAdoption{ResumeFrom: cursor(7)},
			bounds:   PreparedAdoptionBounds{StaticResumeConfigured: true, LocalResumeFrom: 4, HelloLastSeq: 9},
			wantErr:  "both configured",
		},
		{
			name:     "a prepared generation is returned",
			prepared: PreparedAdoption{ControllerGeneration: 12},
			bounds:   PreparedAdoptionBounds{LocalResumeFrom: 4, HelloLastSeq: 9},
			want:     ResolvedPreparedAdoption{ControllerGeneration: 12},
		},
		{
			name:     "a prepared generation against a static one refuses",
			prepared: PreparedAdoption{ControllerGeneration: 12},
			bounds:   PreparedAdoptionBounds{StaticGenerationConfigured: true, LocalResumeFrom: 4, HelloLastSeq: 9},
			wantErr:  "both configured",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolvePreparedAdoption(test.prepared, test.bounds)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, test.wantErr)
				}
				if !errors.Is(err, ErrAdoptionPreparation) {
					t.Fatalf("error %v is not classified as a preparation failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ControllerGeneration != test.want.ControllerGeneration ||
				got.ResumeFrom != test.want.ResumeFrom || got.ResumeProvided != test.want.ResumeProvided {
				t.Fatalf("resolved = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestHandshakeWiresTheRealBoundsIntoTheResolver pins the handshake's CALL to
// ResolvePreparedAdoption, not the resolver itself.
//
// The resolver has its own table, but a table proves only that the function
// refuses what it is told to refuse. Nothing there notices a caller that hands
// it `StaticResumeConfigured: false`, `StaticGenerationConfigured: false`, or
// `LocalResumeFrom: 0` regardless of what it actually has — and each of those
// silently disables one whole check. Now that a second caller passes different
// bounds, the wiring is the part that can drift.
//
// Each case configures the real condition and requires the refusal it implies.
func TestHandshakeWiresTheRealBoundsIntoTheResolver(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	cursor := func(v uint64) *uint64 { return &v }
	tests := []struct {
		name    string
		session string
		options func() ControllerOptions
		wantErr string
	}{
		{
			// StaticResumeConfigured: the caller already fixed the cursor, so a
			// prepared one is a second authority rather than the answer.
			name:    "an externally configured resume conflicts with a prepared one",
			session: "sess-static-resume",
			options: func() ControllerOptions {
				return ControllerOptions{
					ControllerID:               "controller-static-resume",
					ResumeFrom:                 1,
					ResumeExternallyConfigured: true,
					PrepareAdoption: func(evidence AdoptionPreparation) (PreparedAdoption, error) {
						return PreparedAdoption{ResumeFrom: cursor(evidence.LastHostSeq + 1)}, nil
					},
				}
			},
			wantErr: "static and proof-resolved resume cursors are both configured",
		},
		{
			// StaticGenerationConfigured: same shape, on the generation axis.
			name:    "a static generation conflicts with a prepared one",
			session: "sess-static-generation",
			options: func() ControllerOptions {
				return ControllerOptions{
					ControllerID:   "controller-static-generation",
					NextGeneration: func(current shimwire.Generation) shimwire.Generation { return current + 1 },
					PrepareAdoption: func(evidence AdoptionPreparation) (PreparedAdoption, error) {
						return PreparedAdoption{
							ControllerGeneration: evidence.CurrentControllerGeneration + 1,
							ResumeFrom:           cursor(evidence.LastHostSeq + 1),
						}, nil
					},
				}
			},
			wantErr: "prepared and static controller generations are both configured",
		},
		{
			// LocalResumeFrom: the floor a prepared cursor may raise and never
			// regress. A zero handed to the resolver disables it outright.
			name:    "a prepared cursor under the local floor is refused",
			session: "sess-local-floor",
			options: func() ControllerOptions {
				return ControllerOptions{
					ControllerID: "controller-local-floor",
					PrepareAdoption: func(AdoptionPreparation) (PreparedAdoption, error) {
						return PreparedAdoption{ResumeFrom: cursor(0)}, nil
					},
				}
			},
			wantErr: "regresses local floor",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := shortTempDir(t)
			reg, err := NewRegistry(dir)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			id := Identity{OrgID: "org-bounds", SessionID: test.session}
			startInProcessShim(t, reg, dir, id, 1)
			rec, err := reg.Get(id)
			if err != nil {
				t.Fatalf("read the published record: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ctrl, dialErr := Dial(ctx, rec, test.options())
			if dialErr == nil {
				_ = ctrl.Close()
				t.Fatal("the handshake accepted a preparation answer its own bounds forbid")
			}
			if !strings.Contains(dialErr.Error(), test.wantErr) {
				t.Fatalf("refusal %v does not contain %q", dialErr, test.wantErr)
			}
			if !errors.Is(dialErr, ErrAdoptionPreparation) {
				t.Fatalf("refusal %v is not classified as a preparation failure", dialErr)
			}
		})
	}
}
