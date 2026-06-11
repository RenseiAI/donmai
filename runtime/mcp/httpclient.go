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

	"github.com/RenseiAI/donmai/agent"
)

// sessionIDHeader is the Streamable HTTP session header: the server may
// assign a session id on initialize; the client echoes it on every
// subsequent request and DELETEs it on Close.
const sessionIDHeader = "Mcp-Session-Id"

// maxHTTPResponseSize caps a single JSON-RPC response body / SSE stream.
const maxHTTPResponseSize = 32 * 1024 * 1024

// httpMCPClient is the Client implementation over the MCP Streamable HTTP
// transport (2025-03-26): every JSON-RPC message is POSTed to the endpoint;
// the server answers with application/json or a text/event-stream carrying
// the response message.
type httpMCPClient struct {
	session
	conn      *httpConn
	closeOnce sync.Once
}

// dialHTTP builds the transport and performs the handshake. The server's
// Headers (typically the platform Authorization bearer) ride on every
// request.
func dialHTTP(ctx context.Context, server agent.MCPServerConfig) (Client, error) {
	if server.URL == "" {
		return nil, fmt.Errorf("runtime/mcp: server %q (http) has empty URL", server.Name)
	}
	headers := make(map[string]string, len(server.Headers))
	for k, v := range server.Headers {
		headers[k] = v
	}
	conn := &httpConn{
		url:     server.URL,
		headers: headers,
		hc:      http.DefaultClient,
	}
	c := &httpMCPClient{session: session{rpc: conn}, conn: conn}
	if err := c.handshake(ctx); err != nil {
		return nil, fmt.Errorf("runtime/mcp: server %q: %w", server.Name, err)
	}
	return c, nil
}

// Close terminates the server-side session (best-effort DELETE with the
// session id, when one was assigned). Idempotent; always returns nil.
func (c *httpMCPClient) Close() error {
	c.closeOnce.Do(func() {
		sid := c.conn.session()
		if sid == "" {
			return
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, c.conn.url, http.NoBody)
		if err != nil {
			return
		}
		c.conn.setHeaders(req)
		resp, err := c.conn.hc.Do(req)
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
	nextID  atomic.Int64

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
	resp, err := c.hc.Do(req)
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
