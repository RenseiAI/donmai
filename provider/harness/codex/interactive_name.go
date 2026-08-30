package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

const interactiveNameBootstrapShutdownTimeout = 5 * time.Second

var errNamedInteractiveTUIExited = errors.New("codex remote TUI exited before its thread could be named")

type namedInteractiveAppServer struct {
	remoteURL string
	cmd       *exec.Cmd
	client    *interactiveWebSocketClient
	waitDone  chan struct{}
	waitMu    sync.Mutex
	waitErr   error
	diagMu    sync.Mutex
	diagLines []string
	socketDir string
	closeOnce sync.Once
	closeErr  error
}

// startNamedInteractiveAppServer starts one bounded Unix-socket app-server in
// the same CODEX_HOME the TUI will use and holds an initialized control client.
// The TUI must create the new durable thread itself: Codex does not materialize
// an app-server-created no-turn thread enough for a second TUI connection to
// resume it. After the TUI starts, waitAndNameInteractiveThread identifies the
// one new thread, applies the requested name, and proves exact readback before
// the initial prompt is delivered through the PTY.
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
	server := &namedInteractiveAppServer{
		remoteURL: remoteURL, cmd: cmd, waitDone: make(chan struct{}), socketDir: socketDir,
	}
	go server.captureStderr(stderr)
	go func() {
		waitErr := cmd.Wait()
		server.waitMu.Lock()
		server.waitErr = waitErr
		server.waitMu.Unlock()
		close(server.waitDone)
	}()
	probe, err := dialInteractiveAppServer(setupCtx, socketPath)
	if err != nil {
		return nil, errors.Join(err, server.exitedErrorIfDone(), server.close())
	}
	_ = probe.Close()
	client, err := dialInteractiveWebSocket(setupCtx, socketPath)
	if err != nil {
		return nil, errors.Join(err, server.exitedErrorIfDone(), server.close())
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
	if opts.interactiveNameServerStarted != nil {
		opts.interactiveNameServerStarted(server.remoteURL)
	}
	return server, nil
}

func (s *namedInteractiveAppServer) waitAndNameInteractiveThread(
	ctx context.Context,
	spec agent.Spec,
	rpcTimeout time.Duration,
	tuiDone <-chan struct{},
) (string, error) {
	if spec.SessionName == "" {
		return "", errors.New("codex interactive name bootstrap requires a session name")
	}
	if rpcTimeout <= 0 {
		rpcTimeout = 30 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	for {
		// thread/started is server-pushed on the control connection. A small
		// thread/list request pumps that connection while the TUI owns its own
		// websocket; the list body is deliberately not a second identity source.
		if _, err := s.client.request(waitCtx, "thread/list", map[string]any{
			"cwd":         spec.Cwd,
			"limit":       1,
			"sourceKinds": []string{},
		}, rpcTimeout); err != nil {
			return "", fmt.Errorf("thread/list while waiting for remote TUI thread/start: %w", err)
		}
		created := s.client.startedThreadIDs()
		switch len(created) {
		case 0:
		case 1:
			if err := setAndVerifyNamedThreadWithRequest(waitCtx, created[0], spec.SessionName, s.client.request, rpcTimeout); err != nil {
				return "", err
			}
			return created[0], nil
		default:
			return "", fmt.Errorf("remote TUI emitted %d thread/started notifications; refusing an ambiguous name target", len(created))
		}
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("wait for remote TUI thread/start: %w", waitCtx.Err())
		case <-s.waitDone:
			return "", s.processExitError("codex interactive app-server exited before the remote TUI thread became visible")
		case <-tuiDone:
			return "", errNamedInteractiveTUIExited
		case <-time.After(25 * time.Millisecond):
		}
	}
}

type namedThreadRequest func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error)

func setAndVerifyNamedThreadWithRequest(
	ctx context.Context,
	threadID string,
	sessionName string,
	request namedThreadRequest,
	rpcTimeout time.Duration,
) error {
	if _, err := request(ctx, "thread/name/set", map[string]any{
		"threadId": threadID,
		"name":     sessionName,
	}, rpcTimeout); err != nil {
		return fmt.Errorf("thread/name/set: %w", err)
	}
	readRaw, err := request(ctx, "thread/read", map[string]any{
		"threadId": threadID,
	}, rpcTimeout)
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
	if readback.Thread.ID != threadID || readback.Thread.Name != sessionName {
		return fmt.Errorf(
			"thread/read after name set: got id %q name %q, want id %q name %q",
			readback.Thread.ID, readback.Thread.Name, threadID, sessionName,
		)
	}
	return nil
}

type interactiveWebSocketClient struct {
	conn      *websocket.Conn
	transport *http.Transport
	nextID    int64
	startedMu sync.Mutex
	started   map[string]struct{}
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
	return &interactiveWebSocketClient{conn: conn, transport: transport, started: make(map[string]struct{})}, nil
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
		if inbound.Method == "thread/started" {
			var notification struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if err := json.Unmarshal(inbound.Params, &notification); err != nil {
				return nil, fmt.Errorf("decode thread/started notification: %w", err)
			}
			if notification.Thread.ID == "" {
				return nil, errors.New("decode thread/started notification: empty thread id")
			}
			c.startedMu.Lock()
			c.started[notification.Thread.ID] = struct{}{}
			c.startedMu.Unlock()
			continue
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

func (c *interactiveWebSocketClient) startedThreadIDs() []string {
	c.startedMu.Lock()
	defer c.startedMu.Unlock()
	ids := make([]string, 0, len(c.started))
	for id := range c.started {
		ids = append(ids, id)
	}
	return ids
}

func (c *interactiveWebSocketClient) notify(ctx context.Context, method string, params map[string]any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, body)
}

func (c *interactiveWebSocketClient) close() error {
	err := c.conn.Close(websocket.StatusNormalClosure, "setup complete")
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

func remoteInteractiveArgs(argv []string, remoteURL string, spec agent.Spec) ([]string, error) {
	if len(argv) == 0 || argv[0] != "resume" {
		return nil, errors.New("codex remote interactive attach requires resume argv")
	}
	targetIndex := len(argv) - 1
	if spec.Prompt != "" {
		targetIndex--
	}
	if targetIndex < 1 || argv[targetIndex] != spec.SessionName {
		return nil, errors.New("codex remote interactive attach could not locate the prepared session target")
	}
	// Start a fresh TUI against the prepared remote app-server, without the
	// name or prompt positional arguments. The TUI owns thread/start and thus
	// materializes a resumable thread. Donmai names that exact new thread and
	// proves readback before delivering the prompt through the PTY.
	out := []string{"--remote", remoteURL}
	out = append(out, argv[1:targetIndex]...)
	return out, nil
}

func (s *namedInteractiveAppServer) processExitError(message string) error {
	s.waitMu.Lock()
	waitErr := s.waitErr
	s.waitMu.Unlock()
	diagnostic := s.stderrDiagnostic()
	if diagnostic != "" {
		message += "; stderr: " + diagnostic
	}
	if waitErr != nil {
		return fmt.Errorf("%s: %w", message, waitErr)
	}
	return errors.New(message)
}

func (s *namedInteractiveAppServer) exitedErrorIfDone() error {
	select {
	case <-s.waitDone:
		return s.processExitError("codex interactive app-server exited during setup")
	default:
		return nil
	}
}

func (s *namedInteractiveAppServer) captureStderr(stderr io.ReadCloser) {
	defer func() { _ = stderr.Close() }()
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		s.appendStderrDiagnostic(sanitizeInteractiveAppServerDiagnostic(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		s.appendStderrDiagnostic(sanitizeInteractiveAppServerDiagnostic("stderr read failed: " + err.Error()))
	}
}

func (s *namedInteractiveAppServer) appendStderrDiagnostic(line string) {
	if line == "" {
		return
	}
	s.diagMu.Lock()
	s.diagLines = append(s.diagLines, line)
	if len(s.diagLines) > 4 {
		s.diagLines = s.diagLines[len(s.diagLines)-4:]
	}
	s.diagMu.Unlock()
}

func sanitizeInteractiveAppServerDiagnostic(line string) string {
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	lower := strings.ToLower(line)
	for _, sensitive := range []string{"authorization", "bearer", "api_key", "api-key", "x-api-key", "cookie", "secret", "access_token", "refresh_token", "token:", "token="} {
		if strings.Contains(lower, sensitive) {
			return "[credential-bearing diagnostic redacted]"
		}
	}
	return truncateUTF8Detail(line, 512)
}

func truncateUTF8Detail(detail string, maxBytes int) string {
	if len(detail) <= maxBytes {
		return detail
	}
	suffix := "…"
	detail = detail[:maxBytes-len(suffix)]
	for !utf8.ValidString(detail) {
		detail = detail[:len(detail)-1]
	}
	return detail + suffix
}

func (s *namedInteractiveAppServer) stderrDiagnostic() string {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	return strings.Join(s.diagLines, " | ")
}

func (s *namedInteractiveAppServer) close() error {
	s.closeOnce.Do(func() {
		if s.client != nil {
			_ = s.client.close()
		}
		unexpectedExit := false
		select {
		case <-s.waitDone:
			unexpectedExit = true
		default:
		}
		if !unexpectedExit && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscallSIGTERM())
		}
		select {
		case <-s.waitDone:
		case <-time.After(interactiveNameBootstrapShutdownTimeout):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			select {
			case <-s.waitDone:
			case <-time.After(interactiveNameBootstrapShutdownTimeout):
				s.closeErr = errors.New("codex interactive app-server process did not exit")
			}
		}
		if unexpectedExit {
			s.waitMu.Lock()
			waitErr := s.waitErr
			s.waitMu.Unlock()
			if waitErr != nil {
				s.closeErr = errors.Join(s.closeErr, s.processExitError("codex interactive app-server exited unexpectedly"))
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
