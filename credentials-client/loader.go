package credentials

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalcreds "github.com/RenseiAI/agentfactory-tui/internal/credentials"
)

// SocketEnvVar is the name of the environment variable the loader reads
// to discover the credential unix socket. When unset, the loader runs
// in standalone mode.
const SocketEnvVar = "RENSEI_CREDENTIAL_SOCKET"

// defaultHandshakeTimeout is used when [Options.HandshakeTimeout] is
// zero. Five seconds is generous for a local unix-socket round trip
// (microseconds in practice) but rules out a hung daemon making New
// block forever.
const defaultHandshakeTimeout = 5 * time.Second

// Mode constants returned by [Loader.Mode].
const (
	ModeDaemon     = "daemon"
	ModeStandalone = "standalone"
)

// Options configures [New].
//
// All fields are optional except SessionID, which is required when the
// loader runs in daemon mode. In standalone mode SessionID is ignored.
type Options struct {
	// SessionID is the session this loader's credentials belong to.
	// Sent verbatim as the HELLO frame's sessionId in daemon mode.
	// Ignored in standalone mode.
	SessionID string

	// HandshakeTimeout caps the dial + HELLO + INITIAL round-trip.
	// Default: 5 s.
	HandshakeTimeout time.Duration

	// Logger receives one info line on standalone-fallback and warn
	// lines for subscriber-callback panics. Credential values are
	// never logged. When nil, a no-op logger is used.
	Logger *slog.Logger

	// SocketPathOverride, when non-empty, replaces the value read from
	// SocketEnvVar. Intended for tests; production callers leave it
	// zero.
	SocketPathOverride string

	// environFn overrides os.Environ() for standalone-mode tests. Unit
	// tests assign a closure that returns a controlled slice.
	environFn func() []string
}

// Loader is the unified agent-side credential reader.
//
// One Loader covers both daemon mode and standalone mode. Methods are
// safe for concurrent use.
type Loader struct {
	mode string

	mu  sync.RWMutex
	env map[string]string

	logger *slog.Logger

	// daemon-mode-only state. nil in standalone mode.
	conn        net.Conn
	subMu       sync.Mutex
	subscribers map[int]func(delta map[string]string)
	nextSubID   int

	// closed protects against double-close; the pumper goroutine reads
	// it via atomic load to skip log lines after Close.
	closed atomic.Bool

	// readerDone signals the background reader goroutine has exited
	// (daemon mode only). Closed by the reader.
	readerDone chan struct{}
}

// New constructs a [Loader].
//
// If [SocketEnvVar] (or [Options.SocketPathOverride]) is set, New
// attempts daemon mode: it dials the socket, writes a HELLO frame, and
// waits up to HandshakeTimeout for an INITIAL frame. On success the
// returned loader's [Loader.Mode] is "daemon" and a background goroutine
// pumps subsequent UPDATE frames to subscribers.
//
// On any daemon-mode failure — env unset, dial error, handshake
// timeout, malformed INITIAL — New falls back to standalone mode. A
// single info line is logged on fallback. The returned loader's
// [Loader.Mode] is "standalone" and [Loader.Get] / [Loader.All] return
// values sourced from os.Environ() (filtered through
// [internalcreds.IsBlocked]).
//
// New never returns an error in practice; the error return is reserved
// for unrecoverable configuration mistakes (e.g. a nil ctx). Callers
// can treat a non-nil error as a programming bug.
func New(ctx context.Context, opts Options) (*Loader, error) {
	if ctx == nil {
		return nil, errors.New("credentials: New requires a non-nil context")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	socketPath := opts.SocketPathOverride
	if socketPath == "" {
		socketPath = os.Getenv(SocketEnvVar)
	}

	// No socket configured → straight to standalone.
	if socketPath == "" {
		return newStandalone(opts, logger), nil
	}

	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = defaultHandshakeTimeout
	}

	loader, err := dialAndHandshake(ctx, socketPath, opts.SessionID, timeout)
	if err != nil {
		logger.Info("[creds] socket unavailable, falling back to process env",
			slog.String("reason", err.Error()))
		return newStandalone(opts, logger), nil
	}
	loader.logger = logger

	// Spin up the background pump for UPDATE / BYE frames.
	loader.readerDone = make(chan struct{})
	go loader.pump()

	return loader, nil
}

// newStandalone builds a Loader pre-populated from os.Environ().
func newStandalone(opts Options, logger *slog.Logger) *Loader {
	environFn := opts.environFn
	if environFn == nil {
		environFn = os.Environ
	}
	env := make(map[string]string)
	for _, entry := range environFn() {
		idx := strings.IndexByte(entry, '=')
		if idx < 0 {
			continue
		}
		name := entry[:idx]
		// Apply blocklist at ingest so we never even hold blocked
		// values in standalone-mode state. (Daemon mode applies it at
		// read time to match the wire-stream semantics; both end up
		// returning ("", false) for blocked names.)
		if internalcreds.IsBlocked(name) {
			continue
		}
		env[name] = entry[idx+1:]
	}
	return &Loader{
		mode:        ModeStandalone,
		env:         env,
		logger:      logger,
		subscribers: nil, // unused in standalone mode
	}
}

// dialAndHandshake performs the unix-socket dial + HELLO + INITIAL
// sequence. Returns a partially-initialised Loader on success.
func dialAndHandshake(
	ctx context.Context,
	socketPath, sessionID string,
	timeout time.Duration,
) (*Loader, error) {
	if sessionID == "" {
		return nil, errors.New("daemon mode requires Options.SessionID")
	}

	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial unix %s: %w", socketPath, err)
	}

	// Cap reads / writes through the handshake.
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	// 1. Send HELLO.
	helloBytes, err := json.Marshal(helloMessage{Type: "HELLO", SessionID: sessionID})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("marshal HELLO: %w", err)
	}
	helloBytes = append(helloBytes, '\n')
	if _, err := conn.Write(helloBytes); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write HELLO: %w", err)
	}

	// 2. Read INITIAL.
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read INITIAL: %w", err)
	}
	var initial initialMessage
	if jerr := json.Unmarshal(line, &initial); jerr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("decode INITIAL: %w", jerr)
	}
	if initial.Type != "INITIAL" {
		_ = conn.Close()
		return nil, fmt.Errorf("expected INITIAL frame, got %q", initial.Type)
	}

	// Clear the handshake deadline; the background pumper runs with no
	// deadline (UPDATE frames arrive whenever the daemon emits them).
	_ = conn.SetDeadline(time.Time{})

	env := make(map[string]string, len(initial.Env))
	for k, v := range initial.Env {
		if internalcreds.IsBlocked(k) {
			continue
		}
		env[k] = v
	}

	return &Loader{
		mode:        ModeDaemon,
		env:         env,
		conn:        conn,
		subscribers: make(map[int]func(delta map[string]string)),
		// readerDone + logger are wired by the caller (New) so that
		// dialAndHandshake remains free of test-visible side effects.
		// They MUST be non-nil before pump() runs.
	}, nil
}

// pump runs in daemon mode only. It reads UPDATE / BYE frames from the
// socket, merges deltas into the env map, and fans them out to
// subscribers. Exits on EOF, connection error, or explicit Close.
//
// pump's reader uses a NewReader on the connection. dialAndHandshake's
// reader already consumed bytes through the INITIAL frame's terminating
// '\n'; allocating a new bufio.Reader after the handshake would lose
// any bytes already buffered in the handshake reader. We avoid that by
// having pump receive the reader from the loader. (See pumpReader
// below.)
func (l *Loader) pump() {
	defer close(l.readerDone)
	l.pumpReader(bufio.NewReader(l.conn))
}

// pumpReader is the real loop; split out for testability and so a
// caller that retained the handshake reader can pass it in. Today
// pump() always allocates a fresh reader because dialAndHandshake
// consumes exactly one frame before returning and bufio.Reader's
// internal buffer is empty when only one '\n'-terminated line was read
// — but in case future protocol additions buffer ahead, the seam
// exists.
func (l *Loader) pumpReader(reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			l.handleFrame(line)
		}
		if err != nil {
			// Any read error terminates the pump. EOF is the normal
			// path on server-initiated BYE / Close.
			if !l.closed.Load() && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				l.logger.Warn("[creds] socket read failed",
					slog.String("error", err.Error()))
			}
			return
		}
	}
}

// handleFrame parses one line of JSON and dispatches it. Unknown frame
// types are ignored for forward compatibility with future protocol
// extensions.
func (l *Loader) handleFrame(line []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		l.logger.Warn("[creds] malformed frame", slog.String("error", err.Error()))
		return
	}
	switch probe.Type {
	case "UPDATE":
		var msg updateMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			l.logger.Warn("[creds] UPDATE decode failed",
				slog.String("error", err.Error()))
			return
		}
		l.applyUpdate(msg.Delta)
	case "BYE":
		// Server-initiated close; the next read will hit EOF and pump
		// returns. Nothing else to do here.
	default:
		// Unknown / future type — ignore.
	}
}

// applyUpdate merges the delta into the env map (after blocklist
// filtering) and fans out to subscribers.
func (l *Loader) applyUpdate(delta map[string]string) {
	if len(delta) == 0 {
		return
	}
	filtered := make(map[string]string, len(delta))
	for k, v := range delta {
		if internalcreds.IsBlocked(k) {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return
	}

	l.mu.Lock()
	if l.env == nil {
		l.env = make(map[string]string, len(filtered))
	}
	for k, v := range filtered {
		l.env[k] = v
	}
	l.mu.Unlock()

	// Snapshot the subscriber list under the sub lock, then invoke
	// callbacks WITHOUT holding it (callbacks may re-enter Get/All).
	l.subMu.Lock()
	cbs := make([]func(map[string]string), 0, len(l.subscribers))
	for _, cb := range l.subscribers {
		cbs = append(cbs, cb)
	}
	l.subMu.Unlock()

	for _, cb := range cbs {
		l.invokeCallback(cb, filtered)
	}
}

// invokeCallback runs cb with delta, swallowing panics so a misbehaving
// subscriber can't crash the pump goroutine. The delta is a per-call
// copy so the callback can retain it without us worrying about future
// mutation. (Today applyUpdate never mutates the filtered map after
// passing it in, but the copy keeps the contract explicit.)
func (l *Loader) invokeCallback(cb func(map[string]string), delta map[string]string) {
	defer func() {
		if rec := recover(); rec != nil {
			l.logger.Warn("[creds] subscriber panic",
				slog.Any("recovered", rec))
		}
	}()
	copyMap := make(map[string]string, len(delta))
	for k, v := range delta {
		copyMap[k] = v
	}
	cb(copyMap)
}

// Get returns the credential value for name, or ("", false) if absent
// or blocklisted.
//
// In standalone mode, blocked names were never inserted into the map at
// ingest, so the IsBlocked check here is redundant — but the cost is
// nil and it shields against future code paths that might inject into
// the map without filtering.
func (l *Loader) Get(name string) (string, bool) {
	if internalcreds.IsBlocked(name) {
		return "", false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.env[name]
	return v, ok
}

// All returns a copy of the loader's current credential map with the
// blocklist applied. The map is safe for the caller to mutate; future
// loader state changes will not be reflected.
//
// Order is undefined (Go maps are unordered).
func (l *Loader) All() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]string, len(l.env))
	for k, v := range l.env {
		if internalcreds.IsBlocked(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// Subscribe registers cb to receive UPDATE deltas. Returns a function
// that, when called, removes the subscription. Unsubscribe is
// idempotent; subsequent calls are no-ops.
//
// In standalone mode, no UPDATE frames ever arrive, so cb is never
// invoked. Subscribe still returns a valid unsubscribe func to keep the
// API surface mode-agnostic.
//
// Callbacks are invoked asynchronously from the loader's reader
// goroutine. A panic inside cb is recovered; a slow cb blocks the
// pump for that one call but cannot starve other subscribers (each
// callback runs sequentially in the same goroutine — callers should
// not perform long-running I/O inside cb).
func (l *Loader) Subscribe(cb func(delta map[string]string)) func() {
	if cb == nil {
		return func() {}
	}
	// Standalone mode: no UPDATE stream. Return a no-op unsubscribe.
	if l.mode == ModeStandalone {
		return func() {}
	}

	l.subMu.Lock()
	id := l.nextSubID
	l.nextSubID++
	l.subscribers[id] = cb
	l.subMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.subMu.Lock()
			delete(l.subscribers, id)
			l.subMu.Unlock()
		})
	}
}

// Close releases socket resources. Safe to call multiple times. In
// standalone mode, Close is a no-op.
//
// Close attempts a graceful BYE frame before closing the connection; a
// write failure during BYE is ignored — the connection is closed
// either way.
func (l *Loader) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	if l.mode != ModeDaemon || l.conn == nil {
		return nil
	}

	// Best-effort BYE; cap with a short deadline so a hung peer can't
	// hang Close.
	bye, _ := json.Marshal(byeMessage{Type: "BYE", Reason: "client-close"})
	bye = append(bye, '\n')
	_ = l.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = l.conn.Write(bye)

	err := l.conn.Close()

	// Wait briefly for the pump goroutine to exit so callers that
	// inspect l.subscribers / l.env after Close see a quiesced state.
	if l.readerDone != nil {
		select {
		case <-l.readerDone:
		case <-time.After(1 * time.Second):
			// Pump didn't exit — accept the leak rather than block
			// Close indefinitely. This path should be unreachable
			// because conn.Close above unblocks the reader.
		}
	}
	return err
}

// Mode reports whether the loader is in daemon or standalone mode.
// Returns one of [ModeDaemon] or [ModeStandalone]. Useful for
// diagnostic log lines and smoke-test assertions.
func (l *Loader) Mode() string {
	return l.mode
}
