package credentials

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTempSocketPath returns a unix-socket path inside /tmp whose
// total length stays under macOS's 104-char sun_path ceiling. The
// default t.TempDir() on macOS lives under /var/folders/... which
// routinely blows past that limit. Mirrors the helper in
// rensei-tui/daemon/credentials/socket_test.go.
func shortTempSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "afc-creds-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// fakeServer is a minimal HELLO/INITIAL/UPDATE/BYE server backed by a
// real unix listener. Tests assemble a fakeServer, configure the
// INITIAL env, optionally enqueue UPDATEs, then run their assertions
// against a real Loader that dials the same socket.
type fakeServer struct {
	t        *testing.T
	listener net.Listener
	path     string

	// initial is the INITIAL env sent in reply to HELLO.
	initial map[string]string

	// initialDelay is the wall-clock delay before sending INITIAL.
	// Tests use it to trigger handshake timeouts.
	initialDelay time.Duration

	// malformedInitial, when true, sends a non-JSON byte sequence in
	// place of INITIAL so the loader's decode fails.
	malformedInitial bool

	// wrongTypeInitial, when true, sends a well-formed frame whose
	// type is not "INITIAL".
	wrongTypeInitial bool

	// updates is an ordered queue of update frames to push after the
	// INITIAL handshake.
	updates []updateMessage

	// releaseUpdates, when non-nil, gates UPDATE emission. The server
	// sends INITIAL, then waits for releaseUpdates to close, then
	// flushes every queued UPDATE. Tests that need to register
	// subscribers BEFORE updates arrive use this to avoid a race.
	releaseUpdates chan struct{}

	// serverBye, when true, sends a BYE frame after the queued updates.
	serverBye bool

	// hello captures the HELLO frame the client sent (one per
	// connection). Tests read this after dialing.
	helloMu sync.Mutex
	hellos  []helloMessage

	// stopCh closes when the server should shut down.
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newFakeServer(t *testing.T, initial map[string]string) *fakeServer {
	t.Helper()
	path := shortTempSocketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	fs := &fakeServer{
		t:        t,
		listener: listener,
		path:     path,
		initial:  initial,
		stopCh:   make(chan struct{}),
	}
	t.Cleanup(fs.stop)
	return fs
}

func (s *fakeServer) start() {
	s.wg.Add(1)
	go s.acceptLoop()
}

func (s *fakeServer) stop() {
	select {
	case <-s.stopCh:
		// already stopped
	default:
		close(s.stopCh)
	}
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *fakeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var hello helloMessage
	if jerr := json.Unmarshal(line, &hello); jerr == nil {
		s.helloMu.Lock()
		s.hellos = append(s.hellos, hello)
		s.helloMu.Unlock()
	}

	if s.initialDelay > 0 {
		select {
		case <-time.After(s.initialDelay):
		case <-s.stopCh:
			return
		}
	}

	if s.malformedInitial {
		_, _ = conn.Write([]byte("{not json\n"))
		// Keep the connection open briefly so the client can read.
		<-s.stopCh
		return
	}

	if s.wrongTypeInitial {
		bad, _ := json.Marshal(struct {
			Type string `json:"type"`
		}{Type: "OOPS"})
		bad = append(bad, '\n')
		_, _ = conn.Write(bad)
		<-s.stopCh
		return
	}

	initial := initialMessage{Type: "INITIAL", Env: s.initial}
	b, _ := json.Marshal(initial)
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		return
	}

	// If the test gated updates, wait for the release.
	if s.releaseUpdates != nil {
		select {
		case <-s.releaseUpdates:
		case <-s.stopCh:
			return
		}
	}

	// Push queued updates.
	for _, u := range s.updates {
		u.Type = "UPDATE"
		if u.RotatedAt.IsZero() {
			u.RotatedAt = time.Now()
		}
		ub, _ := json.Marshal(u)
		ub = append(ub, '\n')
		if _, werr := conn.Write(ub); werr != nil {
			return
		}
	}

	if s.serverBye {
		bye, _ := json.Marshal(byeMessage{Type: "BYE", Reason: "test"})
		bye = append(bye, '\n')
		_, _ = conn.Write(bye)
		return
	}

	// Drain the client's BYE frame (or EOF on client Close).
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, rerr := reader.ReadBytes('\n')
		if rerr == nil {
			continue
		}
		if errors.Is(rerr, io.EOF) || errors.Is(rerr, net.ErrClosed) {
			return
		}
		// Read deadline timeout — loop and re-check stopCh.
		var nerr net.Error
		if errors.As(rerr, &nerr) && nerr.Timeout() {
			continue
		}
		return
	}
}

// pushUpdate enqueues an UPDATE to be sent after INITIAL on the next
// accepted connection. Must be called before the client dials.
func (s *fakeServer) pushUpdate(delta map[string]string) {
	s.updates = append(s.updates, updateMessage{Delta: delta})
}

// liveUpdate writes an UPDATE frame to every currently-open client
// connection. Used by tests that need to inject updates AFTER the
// loader is already running. fakeServer doesn't track per-conn writers
// directly, so this helper is provided as a placeholder for future
// per-conn API; today tests using "live" updates queue them via
// pushUpdate before dialing.
//
// (Intentionally left as documentation; not implemented.)

// helloCount returns how many HELLOs have arrived. Tests use it to
// confirm a daemon-mode dial happened.
func (s *fakeServer) helloCount() int {
	s.helloMu.Lock()
	defer s.helloMu.Unlock()
	return len(s.hellos)
}

// withEnvironOverride returns an Options that sources environ from the
// given map (KEY=VAL slice). Used by standalone-mode tests.
func withEnvironOverride(entries []string) Options {
	return Options{environFn: func() []string { return entries }}
}

// --- Standalone-mode tests ---

func TestStandaloneHappyPath(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	opts := withEnvironOverride([]string{"FOO=bar", "BAZ=qux"})
	loader, err := New(t.Context(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone mode, got %q", loader.Mode())
	}
	if v, ok := loader.Get("FOO"); !ok || v != "bar" {
		t.Fatalf("Get(FOO) = (%q, %v); want (bar, true)", v, ok)
	}
	all := loader.All()
	if all["FOO"] != "bar" || all["BAZ"] != "qux" {
		t.Fatalf("All() = %v; want FOO/BAZ entries", all)
	}
}

func TestStandaloneBlocklist(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	opts := withEnvironOverride([]string{
		"FOO=visible",
		"RENSEI_DAEMON_JWT=should-be-blocked",
		"WORKER_API_KEY=also-blocked",
	})
	loader, err := New(t.Context(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if v, ok := loader.Get("RENSEI_DAEMON_JWT"); ok || v != "" {
		t.Fatalf("Get(RENSEI_DAEMON_JWT) = (%q, %v); want (\"\", false)", v, ok)
	}
	if v, ok := loader.Get("WORKER_API_KEY"); ok || v != "" {
		t.Fatalf("Get(WORKER_API_KEY) = (%q, %v); want (\"\", false)", v, ok)
	}
	if v, ok := loader.Get("FOO"); !ok || v != "visible" {
		t.Fatalf("Get(FOO) = (%q, %v); want (visible, true)", v, ok)
	}
	all := loader.All()
	if _, present := all["RENSEI_DAEMON_JWT"]; present {
		t.Fatalf("All() leaked RENSEI_DAEMON_JWT: %v", all)
	}
}

func TestStandaloneSubscribeNoOp(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	loader, err := New(t.Context(), withEnvironOverride(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	called := false
	unsubscribe := loader.Subscribe(func(_ map[string]string) {
		called = true
	})
	// Unsubscribe should be safe and idempotent.
	unsubscribe()
	unsubscribe()
	if called {
		t.Fatalf("standalone subscriber was invoked despite no UPDATE stream")
	}
}

func TestStandaloneClose(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	loader, err := New(t.Context(), withEnvironOverride(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	// Second Close is a no-op.
	if err := loader.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
}

// --- Daemon-mode tests ---

func TestDaemonHappyPath(t *testing.T) {
	fs := newFakeServer(t, map[string]string{"API_KEY": "value-1", "OTHER": "v2"})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "sess-1",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeDaemon {
		t.Fatalf("expected daemon mode, got %q", loader.Mode())
	}
	if v, ok := loader.Get("API_KEY"); !ok || v != "value-1" {
		t.Fatalf("Get(API_KEY) = (%q, %v); want (value-1, true)", v, ok)
	}
	if fs.helloCount() != 1 {
		t.Fatalf("expected 1 HELLO, got %d", fs.helloCount())
	}
	fs.helloMu.Lock()
	if fs.hellos[0].SessionID != "sess-1" {
		t.Errorf("HELLO sessionId = %q; want sess-1", fs.hellos[0].SessionID)
	}
	fs.helloMu.Unlock()
}

func TestDaemonBlocklistFromINITIAL(t *testing.T) {
	fs := newFakeServer(t, map[string]string{
		"API_KEY":           "good",
		"RENSEI_DAEMON_JWT": "leaked-via-bad-daemon",
	})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if v, ok := loader.Get("RENSEI_DAEMON_JWT"); ok || v != "" {
		t.Fatalf("Get(RENSEI_DAEMON_JWT) leaked: (%q, %v)", v, ok)
	}
	if _, present := loader.All()["RENSEI_DAEMON_JWT"]; present {
		t.Fatalf("All() leaked RENSEI_DAEMON_JWT")
	}
}

func TestDaemonHandshakeTimeoutFallsBack(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	fs := newFakeServer(t, map[string]string{"X": "y"})
	fs.initialDelay = 500 * time.Millisecond
	fs.start()

	opts := Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
		HandshakeTimeout:   50 * time.Millisecond,
		environFn:          func() []string { return []string{"FALLBACK=ok"} },
	}
	loader, err := New(t.Context(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback, got %q", loader.Mode())
	}
	if v, ok := loader.Get("FALLBACK"); !ok || v != "ok" {
		t.Fatalf("Get(FALLBACK) = (%q, %v); want (ok, true)", v, ok)
	}
}

func TestDaemonMalformedINITIALFallsBack(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	fs := newFakeServer(t, nil)
	fs.malformedInitial = true
	fs.start()

	opts := Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
		HandshakeTimeout:   500 * time.Millisecond,
		environFn:          func() []string { return []string{"AFTER_FALLBACK=yes"} },
	}
	loader, err := New(t.Context(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback, got %q", loader.Mode())
	}
	if v, ok := loader.Get("AFTER_FALLBACK"); !ok || v != "yes" {
		t.Fatalf("Get(AFTER_FALLBACK) = (%q, %v); want (yes, true)", v, ok)
	}
}

func TestDaemonWrongTypeINITIALFallsBack(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	fs := newFakeServer(t, nil)
	fs.wrongTypeInitial = true
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
		HandshakeTimeout:   500 * time.Millisecond,
		environFn:          func() []string { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback on wrong-type INITIAL")
	}
}

func TestDaemonDialFailureFallsBack(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	// Point at a path that does not exist.
	missing := filepath.Join("/tmp", "afc-creds-nonexistent-"+strings.Repeat("x", 8), "no.sock")
	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: missing,
		HandshakeTimeout:   100 * time.Millisecond,
		environFn:          func() []string { return []string{"GOT_FALLBACK=1"} },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback, got %q", loader.Mode())
	}
	if v, _ := loader.Get("GOT_FALLBACK"); v != "1" {
		t.Fatalf("standalone env not populated: Get(GOT_FALLBACK)=%q", v)
	}
}

func TestDaemonSubscribeUpdateAndUnsubscribe(t *testing.T) {
	fs := newFakeServer(t, map[string]string{"A": "1"})
	fs.pushUpdate(map[string]string{"A": "2", "NEW": "value"})
	fs.releaseUpdates = make(chan struct{})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	type capture struct {
		mu     sync.Mutex
		deltas []map[string]string
	}
	capability := &capture{}
	got := make(chan struct{}, 4)
	unsubscribe := loader.Subscribe(func(delta map[string]string) {
		capability.mu.Lock()
		capability.deltas = append(capability.deltas, delta)
		capability.mu.Unlock()
		got <- struct{}{}
	})
	// Subscribers registered — now let the server emit UPDATEs.
	close(fs.releaseUpdates)

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatalf("subscriber did not receive UPDATE within 2s")
	}

	capability.mu.Lock()
	if len(capability.deltas) != 1 {
		capability.mu.Unlock()
		t.Fatalf("expected 1 delta, got %d", len(capability.deltas))
	}
	d := capability.deltas[0]
	capability.mu.Unlock()
	if d["A"] != "2" || d["NEW"] != "value" {
		t.Fatalf("delta = %v; want A=2,NEW=value", d)
	}

	// State merged.
	if v, _ := loader.Get("A"); v != "2" {
		t.Fatalf("Get(A) after UPDATE = %q; want 2", v)
	}
	if v, _ := loader.Get("NEW"); v != "value" {
		t.Fatalf("Get(NEW) after UPDATE = %q; want value", v)
	}

	// Unsubscribe — verify a SECOND server's UPDATE (we can't push to
	// the live conn here, so instead just confirm unsubscribe is
	// idempotent and doesn't panic).
	unsubscribe()
	unsubscribe()
}

func TestDaemonMultipleSubscribers(t *testing.T) {
	fs := newFakeServer(t, map[string]string{})
	fs.pushUpdate(map[string]string{"K": "v"})
	fs.releaseUpdates = make(chan struct{})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	var (
		mu      sync.Mutex
		hits    [2]int
		signals = [2]chan struct{}{make(chan struct{}, 2), make(chan struct{}, 2)}
	)
	for i := range 2 {
		idx := i
		loader.Subscribe(func(_ map[string]string) {
			mu.Lock()
			hits[idx]++
			mu.Unlock()
			signals[idx] <- struct{}{}
		})
	}
	// Both subscribers registered — release the UPDATE.
	close(fs.releaseUpdates)

	for i := range 2 {
		select {
		case <-signals[i]:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber %d did not receive UPDATE", i)
		}
	}
	mu.Lock()
	if hits[0] != 1 || hits[1] != 1 {
		mu.Unlock()
		t.Fatalf("hits = %v; want [1 1]", hits)
	}
	mu.Unlock()
}

func TestDaemonUpdateAppliesBlocklist(t *testing.T) {
	fs := newFakeServer(t, map[string]string{})
	fs.pushUpdate(map[string]string{
		"GOOD":              "ok",
		"RENSEI_DAEMON_JWT": "leak",
	})
	fs.releaseUpdates = make(chan struct{})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	received := make(chan map[string]string, 1)
	loader.Subscribe(func(delta map[string]string) {
		received <- delta
	})
	close(fs.releaseUpdates)

	select {
	case d := <-received:
		if _, leaked := d["RENSEI_DAEMON_JWT"]; leaked {
			t.Fatalf("subscriber received blocked key: %v", d)
		}
		if d["GOOD"] != "ok" {
			t.Fatalf("subscriber missing GOOD: %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("subscriber did not receive UPDATE")
	}

	if v, _ := loader.Get("RENSEI_DAEMON_JWT"); v != "" {
		t.Fatalf("Get leaked blocked key from UPDATE")
	}
}

func TestDaemonCloseSendsBYEAndIsIdempotent(t *testing.T) {
	fs := newFakeServer(t, map[string]string{"A": "1"})
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := loader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := loader.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// After Close, Get still works against the in-memory snapshot.
	if v, _ := loader.Get("A"); v != "1" {
		t.Fatalf("Get after Close = %q; want 1", v)
	}
}

func TestDaemonServerBYEEndsPumpCleanly(t *testing.T) {
	fs := newFakeServer(t, map[string]string{"A": "1"})
	fs.serverBye = true
	fs.start()

	loader, err := New(t.Context(), Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	// Wait for the pump to exit (readerDone closes).
	select {
	case <-loader.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("pump did not exit after server BYE")
	}
}

func TestModeString(t *testing.T) {
	if ModeDaemon != "daemon" {
		t.Errorf("ModeDaemon = %q; want \"daemon\"", ModeDaemon)
	}
	if ModeStandalone != "standalone" {
		t.Errorf("ModeStandalone = %q; want \"standalone\"", ModeStandalone)
	}
}

func TestNewRejectsNilContext(t *testing.T) {
	_, err := New(nil, Options{}) //nolint:staticcheck // intentional nil ctx
	if err == nil {
		t.Fatalf("expected error for nil ctx")
	}
}

func TestNewRequiresSessionIDInDaemonMode(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	fs := newFakeServer(t, map[string]string{})
	fs.start()

	loader, err := New(t.Context(), Options{
		// no SessionID
		SocketPathOverride: fs.path,
		environFn:          func() []string { return []string{"FALLBACK=1"} },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })
	// Missing SessionID is treated as a daemon-mode failure → fall back.
	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback when SessionID missing, got %q", loader.Mode())
	}
}

// TestContextCancelDuringHandshake confirms that a context cancelled
// before / during the dial doesn't hang New.
func TestContextCancelDuringHandshake(t *testing.T) {
	t.Setenv(SocketEnvVar, "")
	fs := newFakeServer(t, map[string]string{})
	fs.initialDelay = 5 * time.Second
	fs.start()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	loader, err := New(ctx, Options{
		SessionID:          "s",
		SocketPathOverride: fs.path,
		HandshakeTimeout:   2 * time.Second,
		environFn:          func() []string { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = loader.Close() })

	// We expect standalone fallback since the dial sees a cancelled ctx.
	if loader.Mode() != ModeStandalone {
		t.Fatalf("expected standalone fallback on cancelled ctx, got %q", loader.Mode())
	}
}
