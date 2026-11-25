package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// sessionIDHeader is the Streamable HTTP session header: the server may
// assign a session id on initialize; the client echoes it on every
// subsequent request and DELETEs it on Close.
const sessionIDHeader = "Mcp-Session-Id"

// maxHTTPResponseSize caps a single JSON-RPC response body / SSE stream.
const maxHTTPResponseSize = 32 * 1024 * 1024

// httpClientTimeout bounds MCP Streamable HTTP calls that arrive without a
// caller deadline (dial handshake, tools/list, and tools/call without one):
// the transport must never inherit http.DefaultClient's unbounded wait,
// which lets one wedged server stall CLI calls for as long as the server
// stays silent.
//
// A tools/call that DOES carry a caller deadline (the Gemini bridge wraps
// every call in its 5-minute tool bound) runs under that deadline instead:
// post() routes it to the unbounded client so the transport never imposes a
// shorter bound than the harness that owns the call. A 30s transport ceiling
// over a 5-minute tool call is how a healthy 32s test run reads as a client
// timeout while the server completes it.
var httpClientTimeout = 30 * time.Second

// httpCloseTimeout bounds the best-effort session DELETE on Close so a
// wedged server cannot stall teardown either.
var httpCloseTimeout = 10 * time.Second

// httpMCPClient is the Client implementation over the MCP Streamable HTTP
// transport (2025-03-26): every JSON-RPC message is POSTed to the endpoint;
// the server answers with application/json or a text/event-stream carrying
// the response message.
type httpMCPClient struct {
	session
	conn *httpConn
	// closeOnce guards the idempotent session DELETE.
	closeOnce sync.Once
	// closeTimeout bounds the best-effort session DELETE. It rides the
	// client so tests can drive a short bound without mutating the package
	// default (mutating a package var from a parallel test races the other
	// parallel tests that dial through it).
	closeTimeout time.Duration
}

// resolveHTTPTimeouts maps one server's optional per-server bounds onto
// concrete client and close timeouts. Zero selects the package default;
// negative disables the bound (no client Timeout / no close deadline) so an
// operator with a known-slow server opts out without forking the transport.
// A non-positive client bound still leaves Background (uncancellable)
// callers bounded by the default: only an explicit per-server value or a
// caller-owned context lifts the ceiling.
func resolveHTTPTimeouts(server agent.MCPServerConfig) (clientTimeout, closeTimeout time.Duration) {
	clientTimeout = httpClientTimeout
	closeTimeout = httpCloseTimeout
	if server.RequestTimeoutSeconds > 0 {
		clientTimeout = time.Duration(server.RequestTimeoutSeconds) * time.Second
	} else if server.RequestTimeoutSeconds < 0 {
		clientTimeout = -1
	}
	if server.CloseTimeoutSeconds > 0 {
		closeTimeout = time.Duration(server.CloseTimeoutSeconds) * time.Second
	} else if server.CloseTimeoutSeconds < 0 {
		closeTimeout = -1
	}
	return clientTimeout, closeTimeout
}

// dialHTTP builds the transport and performs the handshake. The server's
// Headers (typically the platform Authorization bearer) ride on every
// request. Per-server RequestTimeoutSeconds/CloseTimeoutSeconds override the
// package defaults; a caller deadline on ctx already overrides the client
// bound for the handshake itself (see post).
func dialHTTP(ctx context.Context, server agent.MCPServerConfig) (Client, error) {
	clientTimeout, closeTimeout := resolveHTTPTimeouts(server)
	return dialHTTPTimeout(ctx, server, clientTimeout, closeTimeout)
}

// dialHTTPTimeout is dialHTTP with explicit bounds: every call runs under
// clientTimeout and Close's DELETE runs under closeTimeout. Production uses
// the package defaults via dialHTTP; tests pass short bounds to prove the
// timeouts fire without waiting out the production values (and without
// mutating package state from parallel tests).
func dialHTTPTimeout(ctx context.Context, server agent.MCPServerConfig, clientTimeout, closeTimeout time.Duration) (Client, error) {
	if server.URL == "" {
		return nil, fmt.Errorf("runtime/mcp: server %q (http) has empty URL", server.Name)
	}
	headers := make(map[string]string, len(server.Headers))
	for k, v := range server.Headers {
		headers[k] = v
	}
	hcTimeout := clientTimeout
	if hcTimeout < 0 {
		hcTimeout = 0
	}
	conn := &httpConn{
		url:     server.URL,
		headers: headers,
		hc:      &http.Client{Timeout: hcTimeout},
		// unbounded carries requests whose caller owns the bound (an
		// explicit deadline, or a cancel-only context): the caller's
		// context is the bound, so no client ceiling may cut it short.
		// Background callers (nothing to cancel, no deadline) take the
		// bounded client above so a wedged server cannot stall the CLI.
		unbounded: &http.Client{},
	}
	c := &httpMCPClient{session: session{rpc: conn}, conn: conn, closeTimeout: closeTimeout}
	if err := c.handshake(ctx); err != nil {
		return nil, fmt.Errorf("runtime/mcp: server %q: %w", server.Name, err)
	}
	return c, nil
}

// Close terminates the server-side session (best-effort DELETE with the
// session id, when one was assigned). The DELETE runs under the client's
// close bound so a server that never answers cannot stall teardown.
// Idempotent; always returns nil.
func (c *httpMCPClient) Close() error {
	return c.CloseWithContext(context.Background())
}

// CloseWithContext terminates the server-side session under ctx: an explicit
// caller deadline overrides the per-server close bound, so a caller that
// already budgeted teardown is never cut short by the transport default.
// Without a caller deadline the per-server (or package-default) close bound
// applies; a cancel-only context rides the caller's cancellation with no
// additional ceiling. Idempotent; always returns nil.
func (c *httpMCPClient) CloseWithContext(ctx context.Context) error {
	c.closeOnce.Do(func() {
		sid := c.conn.session()
		if sid == "" {
			return
		}
		if ctx == nil {
			ctx = context.Background()
		}
		if _, ok := ctx.Deadline(); ok {
			// Caller deadline wins outright.
		} else if ctx.Done() != nil {
			// Cancel-only: caller owns cancellation; no extra ceiling.
		} else if c.closeTimeout < 0 {
			// Explicit per-server opt-out: no bound beyond the caller's.
		} else if c.closeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.closeTimeout)
			defer cancel()
		} else {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, httpCloseTimeout)
			defer cancel()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.conn.url, http.NoBody)
		if err != nil {
			return
		}
		c.conn.setHeaders(req)
		// The DELETE rides ctx alone on the unbounded client: a client
		// Timeout here would add a second, shorter ceiling that the caller
		// deadline (or the per-server close bound above) cannot override.
		doClient := c.conn.unbounded
		if doClient == nil {
			doClient = c.conn.hc
		}
		resp, err := doClient.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	})
	return nil
}

// httpConn is the Streamable HTTP rpcCaller.
type httpConn struct {
	url     string
	headers map[string]string
	hc      *http.Client
	// unbounded serves tools/call when the caller set a deadline: the
	// context bounds the request, and a client Timeout would only add a
	// shorter competing ceiling. See post.
	unbounded *http.Client
	nextID    atomic.Int64

	mu        sync.Mutex
	sessionID string
}

func (c *httpConn) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// setHeaders applies the configured headers, the protocol headers, and the
// session id (when assigned) to one outbound request.
func (c *httpConn) setHeaders(req *http.Request) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := c.session(); sid != "" {
		req.Header.Set(sessionIDHeader, sid)
	}
}

// captureSession records the session id the server assigned (initialize
// response, per the Streamable HTTP spec).
func (c *httpConn) captureSession(resp *http.Response) {
	sid := resp.Header.Get(sessionIDHeader)
	if sid == "" {
		return
	}
	c.mu.Lock()
	c.sessionID = sid
	c.mu.Unlock()
}

func (c *httpConn) post(ctx context.Context, msg rpcRequest) (*http.Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", msg.Method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req)
	// A caller deadline overrides the transport ceiling for ANY method
	// (the Gemini bridge wraps every tools/call in its 5-minute tool bound,
	// and discovery callers budget initialize/tools/list the same way): the
	// transport must not impose a shorter ceiling than the harness that owns
	// the call. A cancel-only context (cancellable, no deadline) likewise
	// rides the caller's cancellation with no extra ceiling, so a healthy
	// slow tool (a 32s build with cancel-only) completes as it did before the
	// bound existed. Only a Background context — no deadline and nothing to
	// cancel — takes the explicit client timeout, so a server that never
	// answers still cannot stall the CLI. Per-server RequestTimeoutSeconds
	// tunes that fallback without forking the transport.
	hc := c.hc
	if c.unbounded != nil {
		if _, ok := ctx.Deadline(); ok {
			hc = c.unbounded
		} else if ctx.Done() != nil {
			hc = c.unbounded
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", msg.Method, err)
	}
	c.captureSession(resp)
	return resp, nil
}

func (c *httpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	resp, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s: HTTP %d: %s", method, resp.StatusCode, bytes.TrimSpace(tail))
	}

	limited := io.LimitReader(resp.Body, maxHTTPResponseSize)
	var msg *rpcMessage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msg, err = readSSEResponse(limited, id)
	} else {
		msg, err = readJSONResponse(limited)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("%s: %w", method, msg.Error)
	}
	return msg.Result, nil
}

// notify POSTs a notification; the server acknowledges with 202 (or any
// 2xx) and no JSON-RPC body.
func (c *httpConn) notify(ctx context.Context, method string, params any) error {
	resp, err := c.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// readJSONResponse decodes a plain application/json response body.
func readJSONResponse(r io.Reader) (*rpcMessage, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &msg, nil
}

// readSSEResponse scans a text/event-stream body for the JSON-RPC response
// matching id. Other messages on the stream (server notifications,
// requests) are skipped.
func readSSEResponse(r io.Reader, id int64) (*rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxHTTPResponseSize)

	var data []byte
	flush := func() (*rpcMessage, bool) {
		if len(data) == 0 {
			return nil, false
		}
		payload := data
		data = nil
		var msg rpcMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return nil, false
		}
		if msg.ID != nil && *msg.ID == id && msg.Method == "" {
			return &msg, true
		}
		return nil, false
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if msg, ok := flush(); ok {
				return msg, nil
			}
			continue
		}
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			payload = strings.TrimPrefix(payload, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read event stream: %w", err)
	}
	if msg, ok := flush(); ok {
		return msg, nil
	}
	return nil, fmt.Errorf("event stream ended without a response for id %d", id)
}
