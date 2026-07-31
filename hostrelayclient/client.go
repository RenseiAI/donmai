// Package hostrelayclient runs one outbound host-relay-v1 tunnel for a local
// code-intelligence host.
package hostrelayclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/hostrelay"
	"github.com/coder/websocket"
)

const (
	responseDeadlineExceeded = 408
	responseCancelled        = 499
	responseOverloaded       = 503
	responseBadGateway       = 502
)

// TokenProvider returns a tunnel bearer for one connection attempt. It must
// return the raw token only, without a "Bearer " prefix. Client never logs or
// retains the returned token after the WebSocket handshake.
type TokenProvider func(context.Context) (string, error)

// EnvironmentTokenProvider reads a raw tunnel bearer from name on every dial.
// It reports only the environment variable name on failure, never its value.
func EnvironmentTokenProvider(name string) TokenProvider {
	return func(context.Context) (string, error) {
		value, ok := os.LookupEnv(name)
		if !ok || value == "" {
			return "", fmt.Errorf("hostrelayclient: tunnel bearer environment %q is empty", name)
		}
		return value, nil
	}
}

// FileTokenProvider reads a raw tunnel bearer from path on every dial. The file
// must be regular and owner-readable only; its contents are never logged.
func FileTokenProvider(path string) TokenProvider {
	return func(context.Context) (string, error) {
		root, err := os.OpenRoot(filepath.Dir(path))
		if err != nil {
			return "", fmt.Errorf("hostrelayclient: open tunnel bearer directory for %q: %w", path, err)
		}
		defer root.Close() //nolint:errcheck // read outcome is authoritative
		file, err := root.Open(filepath.Base(path))
		if err != nil {
			return "", fmt.Errorf("hostrelayclient: open tunnel bearer file %q: %w", path, err)
		}
		defer file.Close() //nolint:errcheck // read outcome is authoritative
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("hostrelayclient: stat tunnel bearer file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("hostrelayclient: tunnel bearer file %q must be owner-readable-only", path)
		}
		contents, err := io.ReadAll(io.LimitReader(file, 8193))
		if err != nil {
			return "", fmt.Errorf("hostrelayclient: read tunnel bearer file %q: %w", path, err)
		}
		if len(contents) > 8192 {
			return "", fmt.Errorf("hostrelayclient: tunnel bearer file %q exceeds maximum size", path)
		}
		return string(contents), nil
	}
}

// Config describes one workload's outbound tunnel. RelayURL may be either the
// relay origin or the exact hostrelay.TunnelPath. LocalURL must point at a
// loopback code host; the client never dials a non-loopback local target.
type Config struct {
	RelayURL       string
	LocalURL       string
	TokenProvider  TokenProvider
	Workload       hostrelay.Workload
	Generation     uint64
	MaxInFlight    int
	HTTPClient     *http.Client
	DialTimeout    time.Duration
	ReconnectDelay time.Duration
}

// Client maintains the one workload connection described by Config.
type Client struct {
	config      Config
	relayURL    string
	localURL    *url.URL
	httpClient  *http.Client
	dialTimeout time.Duration
	reconnect   time.Duration
}

// New validates cfg and constructs a host-relay-v1 client.
func New(cfg Config) (*Client, error) {
	if cfg.TokenProvider == nil {
		return nil, errors.New("hostrelayclient: token provider is required")
	}
	if cfg.MaxInFlight == 0 {
		cfg.MaxInFlight = hostrelay.DefaultMaxInFlight
	}
	if cfg.MaxInFlight < 1 || cfg.MaxInFlight > hostrelay.DefaultMaxInFlight {
		return nil, fmt.Errorf("hostrelayclient: max in-flight must be 1..%d", hostrelay.DefaultMaxInFlight)
	}
	if _, err := hostrelay.Encode(hostrelay.Hello{
		Workload: cfg.Workload, Version: hostrelay.Version, Generation: cfg.Generation,
		LocalRoute: hostrelay.LocalRoute, MaxInFlight: cfg.MaxInFlight,
	}); err != nil {
		return nil, fmt.Errorf("hostrelayclient: invalid workload configuration: %w", err)
	}
	relayURL, err := tunnelURL(cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	localURL, err := loopbackURL(cfg.LocalURL)
	if err != nil {
		return nil, err
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = time.Second
	}
	return &Client{
		config: cfg, relayURL: relayURL, localURL: localURL, httpClient: cfg.HTTPClient,
		dialTimeout: cfg.DialTimeout, reconnect: cfg.ReconnectDelay,
	}, nil
}

// Run dials exactly one tunnel connection at a time. A disconnected leg cancels
// every in-flight local call and reconnects after ReconnectDelay. Requests are
// never retained or replayed across legs.
func (c *Client) Run(ctx context.Context) error {
	for {
		err := c.runLeg(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		timer := time.NewTimer(c.reconnect)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) runLeg(parent context.Context) error {
	token, err := c.config.TokenProvider(parent)
	if err != nil {
		return fmt.Errorf("hostrelayclient: tunnel bearer: %w", err)
	}
	if err := validateToken(token); err != nil {
		return err
	}
	dialCtx, cancelDial := context.WithTimeout(parent, c.dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.relayURL, &websocket.DialOptions{
		HTTPClient:   c.httpClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		Subprotocols: []string{hostrelay.Subprotocol},
	})
	cancelDial()
	if err != nil {
		return fmt.Errorf("hostrelayclient: dial relay: %w", err)
	}
	defer conn.CloseNow() //nolint:errcheck // connection teardown is best effort
	if conn.Subprotocol() != hostrelay.Subprotocol {
		got := conn.Subprotocol()
		_ = conn.Close(websocket.StatusProtocolError, "host relay subprotocol required")
		return fmt.Errorf("hostrelayclient: relay did not negotiate %q (got %q)", hostrelay.Subprotocol, got)
	}
	conn.SetReadLimit(hostrelay.MaxFrameBytes)

	legCtx, cancel := context.WithCancel(parent)
	defer cancel()
	writer := &lockedWriter{conn: conn}
	if !c.ready(legCtx) {
		return errors.New("hostrelayclient: local code host is not ready")
	}
	if err := writer.write(legCtx, hostrelay.Hello{
		Workload: c.config.Workload, Version: hostrelay.Version, Generation: c.config.Generation,
		LocalRoute: hostrelay.LocalRoute, MaxInFlight: c.config.MaxInFlight, Ready: true,
	}); err != nil {
		return fmt.Errorf("hostrelayclient: write hello: %w", err)
	}

	leg := newLeg(legCtx, c, writer)
	go leg.maintainLiveness()
	return leg.read()
}

func (c *Client) ready(ctx context.Context) bool {
	readyURL := *c.localURL
	readyURL.Path = "/readyz"
	readyURL.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyURL.String(), nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck // readiness body is deliberately discarded
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

type lockedWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *lockedWriter) write(ctx context.Context, message hostrelay.Message) error {
	data, err := hostrelay.Encode(message)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Write(ctx, websocket.MessageBinary, data)
}

type leg struct {
	client *Client
	writer *lockedWriter
	ctx    context.Context

	mu       sync.Mutex
	calls    map[string]context.CancelFunc
	inFlight chan struct{}
	lastPong time.Time
}

func newLeg(ctx context.Context, client *Client, writer *lockedWriter) *leg {
	return &leg{
		client: client, writer: writer, ctx: ctx, lastPong: time.Now(),
		calls: make(map[string]context.CancelFunc), inFlight: make(chan struct{}, client.config.MaxInFlight),
	}
}

func (l *leg) read() error {
	for {
		typ, data, err := l.writer.conn.Read(l.ctx)
		if err != nil {
			l.cancelAll()
			return err
		}
		if typ != websocket.MessageBinary {
			l.cancelAll()
			_ = l.writer.conn.Close(websocket.StatusUnsupportedData, "binary host relay frames required")
			return fmt.Errorf("hostrelayclient: non-binary relay frame")
		}
		message, err := hostrelay.Decode(data)
		if err != nil {
			l.cancelAll()
			_ = l.writer.conn.Close(websocket.StatusProtocolError, "invalid host relay frame")
			return fmt.Errorf("hostrelayclient: decode relay frame: %w", err)
		}
		switch message := message.(type) {
		case *hostrelay.Request:
			l.admit(*message)
		case *hostrelay.Cancel:
			l.cancel(message.RequestID)
		case *hostrelay.Ping:
			if err := l.writer.write(l.ctx, hostrelay.Pong{Nonce: message.Nonce}); err != nil {
				l.cancelAll()
				return err
			}
		case *hostrelay.Pong:
			l.mu.Lock()
			l.lastPong = time.Now()
			l.mu.Unlock()
		default:
			l.cancelAll()
			return fmt.Errorf("hostrelayclient: relay sent %s to host", message.Type())
		}
	}
}

func (l *leg) maintainLiveness() {
	ticker := time.NewTicker(hostrelay.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case now := <-ticker.C:
			l.mu.Lock()
			stale := now.Sub(l.lastPong) >= hostrelay.DeadPeerTimeout
			l.mu.Unlock()
			if stale || l.writer.write(l.ctx, hostrelay.Ping{Nonce: uint64(now.UnixNano())}) != nil {
				l.cancelAll()
				_ = l.writer.conn.CloseNow()
				return
			}
		}
	}
}

func (l *leg) admit(request hostrelay.Request) {
	select {
	case l.inFlight <- struct{}{}:
	default:
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: responseOverloaded})
		return
	}
	if time.Now().UnixMilli() >= request.DeadlineUnixMilli {
		<-l.inFlight
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: responseDeadlineExceeded})
		return
	}
	callCtx, cancel := context.WithCancel(l.ctx)
	l.mu.Lock()
	if _, exists := l.calls[request.RequestID]; exists {
		l.mu.Unlock()
		cancel()
		<-l.inFlight
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: http.StatusConflict})
		return
	}
	l.calls[request.RequestID] = cancel
	l.mu.Unlock()
	go func() {
		defer func() {
			l.mu.Lock()
			delete(l.calls, request.RequestID)
			l.mu.Unlock()
			cancel()
			<-l.inFlight
		}()
		l.forward(callCtx, request)
	}()
}

func (l *leg) cancel(requestID string) {
	l.mu.Lock()
	cancel := l.calls[requestID]
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (l *leg) cancelAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, cancel := range l.calls {
		cancel()
	}
}

func (l *leg) forward(ctx context.Context, request hostrelay.Request) {
	deadline := time.UnixMilli(request.DeadlineUnixMilli)
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(callCtx, request.Method, l.client.localURL.String(), bytes.NewReader(request.Body))
	if err != nil {
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: responseBadGateway})
		return
	}
	for _, header := range request.Headers {
		for _, value := range header.Values {
			httpRequest.Header.Add(header.Name, value)
		}
	}
	httpResponse, err := l.client.httpClient.Do(httpRequest)
	if err != nil {
		status := responseBadGateway
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			status = responseDeadlineExceeded
		} else if errors.Is(callCtx.Err(), context.Canceled) {
			status = responseCancelled
		}
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: status})
		return
	}
	defer httpResponse.Body.Close() //nolint:errcheck // response body is consumed below
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, hostrelay.MaxResponseBodyBytes+1))
	if err != nil || len(body) > hostrelay.MaxResponseBodyBytes {
		status := responseBadGateway
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			status = responseDeadlineExceeded
		} else if errors.Is(callCtx.Err(), context.Canceled) {
			status = responseCancelled
		}
		_ = l.writer.write(l.ctx, hostrelay.Response{RequestID: request.RequestID, Status: status})
		return
	}
	response := hostrelay.Response{RequestID: request.RequestID, Status: httpResponse.StatusCode, Body: body}
	for _, name := range []string{"Content-Type", "WWW-Authenticate"} {
		if values := httpResponse.Header.Values(name); len(values) > 0 {
			response.Headers = append(response.Headers, hostrelay.Header{Name: name, Values: values})
		}
	}
	_ = l.writer.write(l.ctx, response)
}

func tunnelURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("hostrelayclient: relay URL must be an absolute ws or wss URL")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", errors.New("hostrelayclient: relay URL scheme must be ws or wss")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = hostrelay.TunnelPath
	}
	if parsed.Path != hostrelay.TunnelPath || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("hostrelayclient: relay URL must target %s without query or fragment", hostrelay.TunnelPath)
	}
	return parsed.String(), nil
}

func loopbackURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, errors.New("hostrelayclient: local URL must be an absolute http URL")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("hostrelayclient: local URL must not include path, query, or fragment")
	}
	host := parsed.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("hostrelayclient: local URL must target loopback")
		}
	}
	parsed.Path = hostrelay.LocalRoute
	return parsed, nil
}

func validateToken(token string) error {
	if token == "" || len(token) > 8192 || strings.HasPrefix(token, "Bearer ") || strings.ContainsAny(token, " \t\r\n\x00") {
		return errors.New("hostrelayclient: tunnel bearer must be a non-empty raw token without whitespace")
	}
	return nil
}
