package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

const interactiveNameBootstrapShutdownTimeout = 5 * time.Second

// attachToExistingNamedSession reports whether spec explicitly signals that
// SessionName identifies an ALREADY-EXISTING native session to attach to,
// as opposed to a name to assign a freshly created one — the default, and
// the behavior for every current producer (custom name or platform-canonical
// id-shaped name alike). See agent.InteractiveSpec.ResumeExisting.
func attachToExistingNamedSession(spec agent.Spec) bool {
	return spec.Interactive != nil && spec.Interactive.ResumeExisting
}

type namedInteractiveAppServer struct {
	remoteURL string
	cmd       *exec.Cmd
	waitCh    <-chan error
	socketDir string
	// client stays open for the life of the server so the caller can drive
	// further RPCs against the exact same connection — required for the
	// fresh-session path, which must observe a thread/started notification
	// emitted after this function returns (see
	// awaitAndNameLiveThreadWithRequest).
	client    *interactiveWebSocketClient
	closeOnce sync.Once
	closeErr  error
}

// startNamedInteractiveAppServer starts one bounded Unix-socket app-server in
// the same CODEX_HOME the TUI will use, and completes its initialize/
// initialized handshake. The server remains alive while the TUI attaches
// over --remote; its cleanup is tied to the returned PTY Handle.
//
// It deliberately does NOT create (or, for the fresh path, name) a thread
// itself — the two native session shapes need different RPC sequences, and
// only one of them is safe to run before any PTY side effect:
//
//   - ATTACH to an existing session (spec.Interactive.ResumeExisting true):
//     this function resumes the target by name/id via the thread/resume RPC
//     (the same primitive Provider.Resume already uses for the headless
//     lane) BEFORE returning, so a missing target fails closed here with a
//     typed error and no PTY is ever spawned against the wrong thread.
//   - FRESH session (ResumeExisting false — the default; every current
//     producer takes this path for both custom names and the platform's
//     canonical id-shaped names): this function does nothing thread-related.
//     The caller spawns the PTY with bare `--remote <socket>` (no resume
//     subcommand) and lets the TUI create its own thread, then names that
//     thread POST-HOC once it observes the thread's own thread/started
//     notification on the connection this function leaves open — see
//     awaitAndNameLiveThreadWithRequest. This mirrors the proven headless
//     pattern in handle.go (create, then thread/name/set the same live
//     thread) rather than #480's original design of creating+naming a
//     thread here and reattaching to it later from the PTY's own process.
//
// That original design cannot work with the pinned CLI: a thread created
// via thread/start but never given a turn is not resumable by ANY
// interactive invocation this CLI supports. Verified against codex-cli
// 0.151.0 by bootstrapping exactly such a thread and attempting every
// plausible attach shape against it: `resume --remote <socket> <name>` and
// `resume --remote <socket> <raw-thread-uuid>` both fail with "No saved
// session found" / "no rollout found for thread id ..." (the CLI's resume
// lookup is keyed to a persisted rollout file on disk, not live app-server
// state); `resume --remote <socket>` with no id opens an interactive picker
// — unusable headless; and bare `--remote <socket>` silently creates an
// unrelated new thread, orphaning the one just created and named. That
// unreachable-orphan failure mode is the root cause of the production
// defect this file fixes: #480 unconditionally ran `codex resume <name>`
// for any non-empty SessionName, mistaking mere name presence for an
// explicit attach signal, and a fresh platform-named session (custom or
// canonical) always hit the CLI's own "No saved session found" error.
func startNamedInteractiveAppServer(
	ctx context.Context,
	bin string,
	opts Options,
	spec agent.Spec,
	launch interactiveLaunch,
	childEnv map[string]string,
	codexHome string,
) (*namedInteractiveAppServer, error) {
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	rpcTimeout := opts.RPCTimeout
	if rpcTimeout <= 0 {
		rpcTimeout = 30 * time.Second
	}
	setupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append([]string(nil), opts.Args...)
	if len(args) == 0 {
		args = []string{"app-server"}
	}
	socketDir, err := os.MkdirTemp("", "donmai-codex-app-")
	if err != nil {
		return nil, fmt.Errorf("create codex interactive socket directory: %w", err)
	}
	socketPath := filepath.Join(socketDir, "app.sock")
	remoteURL := "unix://" + socketPath
	args = append(args, appServerConfigArgs(launch.argv)...)
	args = append(args, "--listen", remoteURL)
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // binary and args use the provider's resolved harness configuration.
	cmd.Dir = spec.Cwd
	cmd.Env = mergeEnv(childEnv, nil, codexHome)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("codex interactive name bootstrap stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stderr.Close()
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("codex interactive name bootstrap spawn: %w", err)
	}
	go drainStderr(stderr)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	server := &namedInteractiveAppServer{
		remoteURL: remoteURL, cmd: cmd, waitCh: waitCh, socketDir: socketDir,
	}
	probe, err := dialInteractiveAppServer(setupCtx, socketPath)
	if err != nil {
		return nil, errors.Join(err, server.close())
	}
	_ = probe.Close()
	client, err := dialInteractiveWebSocket(setupCtx, socketPath)
	if err != nil {
		return nil, errors.Join(err, server.close())
	}
	server.client = client

	initRaw, err := client.request(setupCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "donmai",
			"title":   "Donmai Interactive Name Bootstrap",
			"version": "0.5.0",
		},
		"capabilities": map[string]any{"experimentalApi": true},
	}, timeout)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("codex interactive name bootstrap initialize: %w", err), server.close())
	}
	var initResp struct {
		CodexHome string `json:"codexHome"`
	}
	if err := json.Unmarshal(initRaw, &initResp); err != nil || initResp.CodexHome == "" ||
		!sameResolvedPath(initResp.CodexHome, codexHome) {
		return nil, errors.Join(errors.New("codex interactive name bootstrap did not confirm the selected config home"), server.close())
	}
	if err := client.notify(setupCtx, "initialized", map[string]any{}); err != nil {
		return nil, errors.Join(fmt.Errorf("codex interactive name bootstrap initialized notification: %w", err), server.close())
	}
	if attachToExistingNamedSession(spec) {
		if err := resumeExistingNamedThreadWithRequest(setupCtx, spec, client.request, rpcTimeout); err != nil {
			return nil, errors.Join(err, server.close())
		}
		// The target is now proven to exist; the PTY opens its own
		// independent --remote connection to attach (`codex resume --remote
		// <socket> <name>`), so nothing further is needed on this
		// diagnostic connection. Close it now, while the app-server peer is
		// still fully alive, rather than leaving it open for the life of
		// the session — see namedInteractiveAppServer.closeClient's doc
		// comment for why a teardown-time close instead produced a
		// spurious "cleanup_failed" ResultEvent for every named session.
		_ = server.closeClient()
	}
	if opts.interactiveNameServerStarted != nil {
		opts.interactiveNameServerStarted(server.remoteURL)
	}
	return server, nil
}

// finishNamingLiveInteractiveThread waits for the thread the just-spawned
// PTY creates (a thread/started notification on server's still-open
// connection) and names it post-hoc via thread/name/set + a thread/read
// verify. Called by the caller ONLY for the fresh (non-attach) path, after
// the PTY has been spawned — see startNamedInteractiveAppServer's doc
// comment for why the sequencing must be this way around.
func finishNamingLiveInteractiveThread(ctx context.Context, spec agent.Spec, server *namedInteractiveAppServer, timeout time.Duration) error {
	if server == nil || server.client == nil {
		return errors.New("codex interactive name bootstrap connection is not open")
	}
	if err := awaitAndNameLiveThreadWithRequest(ctx, spec, server.client.awaitNotification, server.client.request, timeout); err != nil {
		return err
	}
	// Naming succeeded and the PTY already has its own independent
	// connection to the app-server (the one that created the thread just
	// named). Close this diagnostic connection now, while the app-server
	// peer is still fully alive, instead of leaving it open for the life of
	// the session — see namedInteractiveAppServer.closeClient's doc comment.
	_ = server.closeClient()
	return nil
}

type namedThreadRequest func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error)

type notificationWaiter func(ctx context.Context, method string, timeout time.Duration) (json.RawMessage, error)

// awaitAndNameLiveThreadWithRequest waits for the method/thread that the PTY
// itself created (a thread/started notification), then names that exact
// live thread and verifies the readback — the fresh-session sequence. The
// notification and request dependencies are injected so this logic is unit
// testable without a real websocket/PTY.
func awaitAndNameLiveThreadWithRequest(
	ctx context.Context,
	spec agent.Spec,
	awaitNotification notificationWaiter,
	request namedThreadRequest,
	timeout time.Duration,
) error {
	if spec.SessionName == "" {
		return errors.New("codex interactive live-thread naming requires a session name")
	}
	raw, err := awaitNotification(ctx, "thread/started", timeout)
	if err != nil {
		return fmt.Errorf("thread/started: %w", err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		return fmt.Errorf("thread/started: decode notification: %w", err)
	}
	if started.Thread.ID == "" {
		return errors.New("thread/started: empty thread id in notification")
	}
	if _, err := request(ctx, "thread/name/set", map[string]any{
		"threadId": started.Thread.ID,
		"name":     spec.SessionName,
	}, timeout); err != nil {
		return fmt.Errorf("thread/name/set: %w", err)
	}
	readRaw, err := request(ctx, "thread/read", map[string]any{
		"threadId": started.Thread.ID,
	}, timeout)
	if err != nil {
		return fmt.Errorf("thread/read after name set: %w", err)
	}
	var readback struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(readRaw, &readback); err != nil {
		return fmt.Errorf("thread/read after name set: decode response: %w", err)
	}
	if readback.Thread.ID != started.Thread.ID || readback.Thread.Name != spec.SessionName {
		return fmt.Errorf(
			"thread/read after name set: got id %q name %q, want id %q name %q",
			readback.Thread.ID, readback.Thread.Name, started.Thread.ID, spec.SessionName,
		)
	}
	return nil
}

// codexThreadIDPattern matches the shape of a codex-native thread id (the
// same 8-4-4-4-12 hex form observed from thread/start responses against a
// real codex-cli 0.151.0 binary, e.g. "01a0548d-9a06-7a30-a72c-f7c94b8c899c").
// thread/resume's threadId parameter is a thread id, never a human-assigned
// name — see resumeExistingNamedThreadWithRequest.
var codexThreadIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrResumeRequiresThreadID is returned when an attach-to-existing request's
// SessionName is not shaped like a codex thread id. thread/resume only
// accepts a thread id (verified against a real codex-cli 0.151.0 binary: a
// non-UUID id is rejected with its own distinct "invalid session id" error,
// never treated as a name lookup) — resolving a human-assigned name to its
// thread id is not implemented here. No current producer sets
// agent.InteractiveSpec.ResumeExisting, so this is forward-compatible
// scaffolding, not a regression: whoever first wires that signal must also
// supply (or resolve) a real thread id, or extend this function with a
// name-to-id lookup as its own change.
var ErrResumeRequiresThreadID = errors.New("codex: resume-by-name is not supported; thread/resume requires a thread id")

// resumeExistingNamedThreadWithRequest proves the ATTACH target actually
// exists — via the same thread/resume JSON-RPC method the headless
// Provider.Resume already uses in production — before any PTY side effect.
//
// spec.SessionName must be shaped like a codex thread id: thread/resume
// takes a thread id, never a name, and silently probing a name-shaped
// value as though it were an id would surface the CLI's "invalid session
// id" rejection mis-mapped onto agent.ErrSessionNotFound (a "wrong shape"
// error, not a "doesn't exist" error). A non-id-shaped name instead fails
// closed with ErrResumeRequiresThreadID before any RPC is attempted.
//
// A properly-shaped id that thread/resume rejects (including one for a
// thread that has taken no turn yet — a real codex-cli 0.151.0 probe
// confirmed thread/resume itself accepts a still-live, never-turned thread
// by id; it is only the `codex resume` CLI subcommand's own separate
// rollout-file lookup, used by the fresh-session path's PTY attach, that
// requires persistence) returns agent.ErrSessionNotFound naming the
// session — never a silent fallback that could spawn a PTY against a
// different (freshly created) thread.
func resumeExistingNamedThreadWithRequest(
	ctx context.Context,
	spec agent.Spec,
	request namedThreadRequest,
	timeout time.Duration,
) error {
	if spec.SessionName == "" {
		return errors.New("codex interactive attach-to-existing requires a session name")
	}
	if !codexThreadIDPattern.MatchString(spec.SessionName) {
		return fmt.Errorf("%w: %q", ErrResumeRequiresThreadID, spec.SessionName)
	}
	if _, err := request(ctx, "thread/resume", map[string]any{
		"threadId": spec.SessionName,
	}, timeout); err != nil {
		return fmt.Errorf("%w: codex interactive session %q does not exist or has no resumable history: %w", agent.ErrSessionNotFound, spec.SessionName, err)
	}
	return nil
}

type interactiveWebSocketClient struct {
	conn      *websocket.Conn
	transport *http.Transport
	nextID    int64
}

func dialInteractiveWebSocket(ctx context.Context, socketPath string) (*interactiveWebSocketClient, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	conn, _, err := websocket.Dial(ctx, "ws://localhost", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("connect codex interactive websocket: %w", err)
	}
	return &interactiveWebSocketClient{conn: conn, transport: transport}, nil
}

func (c *interactiveWebSocketClient) request(
	ctx context.Context,
	method string,
	params map[string]any,
	timeout time.Duration,
) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	requestCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, ID: ptrInt(int(id)), Params: params})
	if err != nil {
		return nil, err
	}
	if err := c.conn.Write(requestCtx, websocket.MessageText, body); err != nil {
		return nil, err
	}
	for {
		_, raw, err := c.conn.Read(requestCtx)
		if err != nil {
			return nil, err
		}
		var inbound rpcInbound
		if err := json.Unmarshal(raw, &inbound); err != nil {
			return nil, err
		}
		var responseID int64
		if err := json.Unmarshal(inbound.ID, &responseID); err != nil || responseID != id || inbound.Method != "" {
			continue
		}
		if inbound.Error != nil {
			return nil, &RPCError{Method: method, Code: inbound.Error.Code, Message: inbound.Error.Message}
		}
		return inbound.Result, nil
	}
}

// awaitNotification blocks until an inbound notification with the given
// method arrives (discarding anything else — responses to a request the
// caller is not concurrently waiting on, or notifications for a different
// method), and returns its params. Like request, this assumes it is the
// sole active reader of the connection at the time it is called: the
// fresh-session sequence this exists for never has a request in flight
// concurrently with an awaitNotification call.
func (c *interactiveWebSocketClient) awaitNotification(
	ctx context.Context,
	method string,
	timeout time.Duration,
) (json.RawMessage, error) {
	requestCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	for {
		_, raw, err := c.conn.Read(requestCtx)
		if err != nil {
			return nil, err
		}
		var inbound rpcInbound
		if err := json.Unmarshal(raw, &inbound); err != nil {
			return nil, err
		}
		if inbound.Method == method {
			return inbound.Params, nil
		}
	}
}

func (c *interactiveWebSocketClient) notify(ctx context.Context, method string, params map[string]any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, body)
}

// close tears down this diagnostic connection with CloseNow rather than
// Close: Close performs a graceful handshake that WRITES a close frame and
// then WAITS to READ the peer's own close frame in reply (coder/websocket's
// documented behavior). A real codex-cli 0.151.0 app-server does not
// reciprocate that handshake — it drops the raw connection once it is done
// with it, regardless of how soon after the handshake this runs — so Close
// reliably surfaced as "failed to close WebSocket: failed to read frame
// header: EOF" here, which ptycli.Handle.run treats as a cleanup failure and
// turns a normal session exit into agent.ResultEvent{Success: false,
// ErrorSubtype: "cleanup_failed"} for every named session (reproduced 3/3
// against a real binary before this fix). CloseNow closes the underlying
// connection immediately without attempting that handshake, so there is
// nothing left to read and nothing to fail.
func (c *interactiveWebSocketClient) close() error {
	err := c.conn.CloseNow()
	c.transport.CloseIdleConnections()
	return err
}

func dialInteractiveAppServer(ctx context.Context, socketPath string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 100 * time.Millisecond}
	for {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect codex interactive app-server: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func appServerConfigArgs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--config":
			if i+1 < len(argv) {
				out = append(out, argv[i], argv[i+1])
				i++
			}
		case "--strict-config":
			out = append(out, argv[i])
		}
	}
	return out
}

// remoteInteractiveArgs points the interactive TUI's launch at the
// already-live bootstrap app-server. Two argv shapes reach here:
//
//   - resume-prefixed (the ATTACH path: argv[0] == "resume", carrying the
//     target name/id): --remote is inserted right after resume, preserving
//     #480's original construction byte-for-byte.
//   - everything else (the FRESH path, including the empty-name bare TUI
//     case, which this function leaves untouched because the caller only
//     invokes it for a non-empty SessionName): --remote is prepended so the
//     TUI creates its own thread on the bootstrap server instead of its own
//     private one, without asking it to resume anything by name.
func remoteInteractiveArgs(argv []string, remoteURL string) []string {
	if len(argv) > 0 && argv[0] == "resume" {
		out := []string{"resume", "--remote", remoteURL}
		return append(out, argv[1:]...)
	}
	out := []string{"--remote", remoteURL}
	return append(out, argv...)
}

// closeClient closes the diagnostic RPC connection this server opened for
// the bootstrap handshake and any pre-PTY naming/resume RPCs, and clears the
// field so a later call (including from close's teardown) is a no-op.
//
// It is called EAGERLY, right after that connection's job is done (naming
// completes for the fresh path, the existence check completes for the
// attach path) — never left open for the life of the PTY session. Two
// independent things make that matter:
//
//   - interactiveWebSocketClient.close uses CloseNow, not a graceful
//     handshake close, because a real codex-cli 0.151.0 app-server does not
//     reciprocate a close handshake at all (see that method's doc comment)
//     — so the "EOF at teardown" failure this fixes is not actually a
//     function of HOW LONG the connection sat open; it reproduced even
//     closing immediately after the handshake, before this file also
//     switched to CloseNow.
//   - Even with CloseNow in place, this connection is the only reader on
//     its socket. Leaving it open and unread for the life of a session (a
//     PTY session can run for a long time) is unnecessary exposure to
//     notification backpressure or an app-server-initiated idle close for
//     no benefit — its RPC job is done once naming/the existence check
//     completes, so closing it then, while the app-server is still fully
//     alive and mid-handshake with the PTY's own separate connection, is
//     strictly safer than holding it open.
//
// ptycli.Handle.run treats ANY non-nil cleanup error as session failure, so
// the original EOF (from graceful Close, reproduced regardless of timing)
// silently turned every named session's successful exit into
// agent.ResultEvent{Success: false, ErrorSubtype: "cleanup_failed"} —
// reproduced against a real codex-cli 0.151.0 binary before this fix, both
// via TestIntegration_RealCodexNamedInteractivePTYFreshCreateAndCleanup and,
// more simply, by closing a fresh diagnostic connection immediately after
// its own initialize/initialized handshake with nothing else in between.
func (s *namedInteractiveAppServer) closeClient() error {
	if s.client == nil {
		return nil
	}
	err := s.client.close()
	s.client = nil
	return err
}

func (s *namedInteractiveAppServer) close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.closeClient()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscallSIGTERM())
		}
		select {
		case <-s.waitCh:
		case <-time.After(interactiveNameBootstrapShutdownTimeout):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			select {
			case <-s.waitCh:
			case <-time.After(interactiveNameBootstrapShutdownTimeout):
				s.closeErr = errors.Join(s.closeErr, errors.New("codex interactive app-server process did not exit"))
			}
		}
		s.closeErr = errors.Join(s.closeErr, os.RemoveAll(s.socketDir))
	})
	return s.closeErr
}

func ambientCodexHome() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Abs(home)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve Codex home: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}
