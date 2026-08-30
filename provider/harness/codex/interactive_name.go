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
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/coder/websocket"
)

const interactiveNameBootstrapShutdownTimeout = 5 * time.Second

// createAndVerifyNamedThread performs the native app-server sequence required
// before an interactive TUI can attach by name. The caller owns an initialized
// client. A successful return proves the persisted thread reads back the exact
// canonical name; no first turn is started here.
func createAndVerifyNamedThread(
	ctx context.Context,
	client *Client,
	spec agent.Spec,
	rpcTimeout time.Duration,
) (string, error) {
	return createAndVerifyNamedThreadWithRequest(ctx, spec, client.RequestWithRetry, rpcTimeout)
}

type namedInteractiveAppServer struct {
	remoteURL string
	cmd       *exec.Cmd
	waitCh    <-chan error
	socketDir string
	closeOnce sync.Once
	closeErr  error
}

// startNamedInteractiveAppServer starts one bounded Unix-socket app-server in
// the same CODEX_HOME the TUI will use. The server remains alive while the TUI
// attaches with `codex resume --remote <socket> <name>`; this is required
// because a no-turn thread is not materialized across an app-server restart.
// The first TUI turn therefore lands on the already-named live thread and makes
// that same id/name pair durable. No terminal keystrokes or timing guesses are
// involved.
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
	defer func() { _ = client.close() }()

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
	if _, err := createAndVerifyNamedThreadWithRequest(setupCtx, spec, client.request, rpcTimeout); err != nil {
		return nil, errors.Join(err, server.close())
	}
	if opts.interactiveNameServerStarted != nil {
		opts.interactiveNameServerStarted(server.remoteURL)
	}
	return server, nil
}

type namedThreadRequest func(context.Context, string, map[string]any, time.Duration) (json.RawMessage, error)

func createAndVerifyNamedThreadWithRequest(
	ctx context.Context,
	spec agent.Spec,
	request namedThreadRequest,
	rpcTimeout time.Duration,
) (string, error) {
	if spec.SessionName == "" {
		return "", errors.New("codex interactive name bootstrap requires a session name")
	}
	plan := NewSpawnPlan(spec)
	raw, err := request(ctx, "thread/start", plan.ThreadStart, rpcTimeout)
	if err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		return "", fmt.Errorf("thread/start: decode response: %w", err)
	}
	if started.Thread.ID == "" {
		return "", errors.New("thread/start: empty thread id in response")
	}
	if _, err := request(ctx, "thread/name/set", map[string]any{
		"threadId": started.Thread.ID,
		"name":     spec.SessionName,
	}, rpcTimeout); err != nil {
		return "", fmt.Errorf("thread/name/set: %w", err)
	}
	readRaw, err := request(ctx, "thread/read", map[string]any{
		"threadId": started.Thread.ID,
	}, rpcTimeout)
	if err != nil {
		return "", fmt.Errorf("thread/read after name set: %w", err)
	}
	var readback struct {
		Thread struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(readRaw, &readback); err != nil {
		return "", fmt.Errorf("thread/read after name set: decode response: %w", err)
	}
	if readback.Thread.ID != started.Thread.ID || readback.Thread.Name != spec.SessionName {
		return "", fmt.Errorf(
			"thread/read after name set: got id %q name %q, want id %q name %q",
			readback.Thread.ID, readback.Thread.Name, started.Thread.ID, spec.SessionName,
		)
	}
	return started.Thread.ID, nil
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

func remoteInteractiveArgs(argv []string, remoteURL string) []string {
	if len(argv) == 0 || argv[0] != "resume" {
		return argv
	}
	out := []string{"resume", "--remote", remoteURL}
	return append(out, argv[1:]...)
}

func (s *namedInteractiveAppServer) close() error {
	s.closeOnce.Do(func() {
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
				s.closeErr = errors.New("codex interactive app-server process did not exit")
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
