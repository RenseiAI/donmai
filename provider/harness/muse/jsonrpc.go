package muse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// JSON-RPC 2.0 message shapes for the MSP `muse serve` host. Cloned from
// the codex harness shape (provider/harness/codex/jsonrpc.go) with the
// routing key changed: MSP multiplexes notifications by `sessionId`
// (params.sessionId), not threadId.
//
// Differences from codex:
//   - Route by `sessionId` (extractSessionID), with client-minted UUIDv7 ids.
//   - Server-initiated `approval/request` and `userInput/request` MUST be
//     answered `{}` (the presentation receipt); the decision travels as a
//     separate `approval/decide` command.

// rpcRequest is a JSON-RPC 2.0 request emitted by this client.
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc,omitempty"`
	Method  string         `json:"method"`
	ID      *int           `json:"id,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response received by this client.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError mirrors the JSON-RPC 2.0 error object, with the MSP typed
// error data (error.data.kind) hosts attach.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// rpcInbound is the union type used while reading the stdio stream.
// The host multiplexes responses, notifications, and server-requests on
// one stream; we discriminate via the presence of id/method.
type rpcInbound struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// inboundKind classifies an inbound JSON-RPC message.
type inboundKind int

const (
	inboundUnknown inboundKind = iota
	// inboundResponse has an id and no method — a reply to one of our
	// outgoing requests.
	inboundResponse
	// inboundNotification has a method but no id — host-pushed event.
	inboundNotification
	// inboundServerRequest has both id and method — the host requesting
	// something from us (approval/request, userInput/request) and
	// expecting a reply.
	inboundServerRequest
)

func (m *rpcInbound) classify() inboundKind {
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	hasMethod := m.Method != ""
	switch {
	case hasID && hasMethod:
		return inboundServerRequest
	case hasID:
		return inboundResponse
	case hasMethod:
		return inboundNotification
	}
	return inboundUnknown
}

// notification is the parsed shape we hand to subscribers.
//
// Server-requests are also delivered through the same notification
// channel — handlers detect them via the non-nil ServerRequestID and
// must respond (the handle answers approval/request and
// userInput/request with `{}`).
type notification struct {
	// Method is the JSON-RPC method name (e.g. "session/started",
	// "item/started", "approval/requested").
	Method string

	// Params is the raw JSON params body, decoded lazily by handlers
	// so we do not pay for shapes we never read.
	Params json.RawMessage

	// ServerRequestID is non-nil when this notification represents a
	// JSON-RPC server-request expecting a reply. Handlers must reply
	// exactly once.
	ServerRequestID json.RawMessage
}

// notificationHandler routes inbound notifications to a subscriber.
//
// The Client invokes the handler synchronously on its read goroutine;
// handlers MUST be cheap (push to a channel, register a flag, etc.)
// and never block on the network or another handler's output.
type notificationHandler func(notification)

// pendingRequest tracks a JSON-RPC request awaiting its response.
type pendingRequest struct {
	ch  chan rpcResponse
	ctx context.Context
}

// Client is a bidirectional JSON-RPC 2.0 client over stdio.
//
// One Client serves exactly one `muse serve` child (one Handle's
// session). Concurrent calls to Request and Notify are safe; the
// underlying writer is mutex-protected.
type Client struct {
	w  io.Writer
	r  io.Reader
	mu sync.Mutex // serialises writes to w

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]*pendingRequest

	subsMu sync.RWMutex
	// sessionSubs maps sessionId → handler. Notifications carrying a
	// matching sessionId are delivered to the corresponding handler.
	sessionSubs map[string]notificationHandler
	// global is fired for notifications without a sessionId.
	global notificationHandler
	// onClose fires once when the read loop exits. It is the
	// Handle's hook to mark the session as failed.
	onClose func(error)

	closeOnce sync.Once
	closeErr  error
	doneCh    chan struct{}
}

// NewClient wires a Client to a pair of stdio streams.
//
// w is typically the `muse serve` child's stdin; r its stdout. The
// caller is responsible for spawning the child process and managing
// its lifecycle outside the Client (muse.go owns that).
func NewClient(w io.Writer, r io.Reader) *Client {
	c := &Client{
		w:           w,
		r:           r,
		pending:     make(map[int64]*pendingRequest),
		sessionSubs: make(map[string]notificationHandler),
		doneCh:      make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// SetOnClose registers a hook fired once when the read loop exits.
// The hook receives the terminal error (or nil for clean close).
func (c *Client) SetOnClose(fn func(error)) {
	c.subsMu.Lock()
	c.onClose = fn
	c.subsMu.Unlock()
}

// Subscribe registers a notification handler for a specific sessionId.
//
// Handlers are invoked synchronously on the Client's read goroutine.
// Subscribing twice for the same session replaces the prior handler.
func (c *Client) Subscribe(sessionID string, h notificationHandler) {
	c.subsMu.Lock()
	c.sessionSubs[sessionID] = h
	c.subsMu.Unlock()
}

// Unsubscribe removes a session handler. Safe to call after Stop.
func (c *Client) Unsubscribe(sessionID string) {
	c.subsMu.Lock()
	delete(c.sessionSubs, sessionID)
	c.subsMu.Unlock()
}

// SubscribeGlobal registers a fall-through handler invoked for
// notifications that do not carry a sessionId or whose sessionId has no
// dedicated subscriber.
func (c *Client) SubscribeGlobal(h notificationHandler) {
	c.subsMu.Lock()
	c.global = h
	c.subsMu.Unlock()
}

// Request sends a JSON-RPC request and waits for the matching response.
//
// timeout caps how long we wait for the response; a zero timeout
// leaves it bounded only by ctx.
func (c *Client) Request(ctx context.Context, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		ID:      ptrInt(int(id)),
		Params:  params,
	}

	pending := &pendingRequest{ch: make(chan rpcResponse, 1), ctx: ctx}
	c.pendingMu.Lock()
	c.pending[id] = pending
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.write(req); err != nil {
		return nil, fmt.Errorf("muse jsonrpc: write request %s: %w", method, err)
	}

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutCh = t.C
	}

	// On termination paths (ctx cancel, timeout, client close) we still
	// re-check pending.ch before returning the failure: a response can be
	// delivered to pending.ch in the same instant that c.doneCh closes,
	// and an unguarded select would lose a perfectly good reply.
	checkResp := func() (json.RawMessage, error, bool) {
		select {
		case resp := <-pending.ch:
			if resp.Error != nil {
				return nil, &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}, true
			}
			return resp.Result, nil, true
		default:
			return nil, nil, false
		}
	}

	select {
	case resp := <-pending.ch:
		if resp.Error != nil {
			return nil, &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
		}
		return resp.Result, nil
	case <-ctx.Done():
		if res, err, ok := checkResp(); ok {
			return res, err
		}
		return nil, ctx.Err()
	case <-timeoutCh:
		if res, err, ok := checkResp(); ok {
			return res, err
		}
		return nil, fmt.Errorf("muse jsonrpc: request %s timed out after %s", method, timeout)
	case <-c.doneCh:
		if res, err, ok := checkResp(); ok {
			return res, err
		}
		err := c.closeErr
		if err == nil {
			err = errors.New("muse jsonrpc: client closed")
		}
		return nil, err
	}
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) Notify(method string, params map[string]any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	if err := c.write(req); err != nil {
		return fmt.Errorf("muse jsonrpc: notify %s: %w", method, err)
	}
	return nil
}

// RespondToServerRequest sends a successful JSON-RPC response to a
// server-request previously delivered to a notification handler.
//
// id MUST be the raw json.RawMessage captured from the inbound message
// (preserve the original encoding to avoid mismatch).
func (c *Client) RespondToServerRequest(id json.RawMessage, result map[string]any) error {
	body := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]any  `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result}
	if err := c.write(body); err != nil {
		return fmt.Errorf("muse jsonrpc: respond to server request: %w", err)
	}
	return nil
}

// RespondToServerRequestWithError replies with a JSON-RPC error to a
// server-request we cannot satisfy. The host hangs the agent if a
// server-request never gets a reply, so we always send something.
func (c *Client) RespondToServerRequestWithError(id json.RawMessage, code int, message string) error {
	body := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: message}}
	if err := c.write(body); err != nil {
		return fmt.Errorf("muse jsonrpc: respond with error: %w", err)
	}
	return nil
}

// Stop releases pending requests and closes the read loop.
//
// After Stop, no new Request calls succeed. Pending requests fail with
// the supplied cause (or "client stopped" when nil).
func (c *Client) Stop(cause error) {
	fired := false
	c.closeOnce.Do(func() {
		fired = true
		if cause == nil {
			cause = errors.New("muse jsonrpc: client stopped")
		}
		c.closeErr = cause
		close(c.doneCh)

		// Reject all pending requests so callers do not block forever.
		c.pendingMu.Lock()
		for _, p := range c.pending {
			select {
			case p.ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: cause.Error()}}:
			default:
			}
		}
		c.pendingMu.Unlock()
	})
	if !fired {
		return
	}
	// The hook runs outside closeOnce: it may tear the handle down,
	// which stops this client again, and a re-entrant Once.Do
	// deadlocks.
	c.subsMu.RLock()
	hook := c.onClose
	c.subsMu.RUnlock()
	if hook != nil {
		hook(c.closeErr)
	}
}

// Done returns a channel that's closed when the client has stopped.
func (c *Client) Done() <-chan struct{} { return c.doneCh }

// CloseErr returns the cause of client close, or nil before close.
func (c *Client) CloseErr() error {
	select {
	case <-c.doneCh:
		return c.closeErr
	default:
		return nil
	}
}

// readLoop reads JSON-RPC messages from r line-by-line and dispatches
// them. Exits when r returns EOF or a hard error.
func (c *Client) readLoop() {
	var loopErr error
	defer func() { c.Stop(loopErr) }()

	scanner := bufio.NewScanner(c.r)
	// Notifications can be large (full tool results, MCP payloads).
	// Bump the buffer ceiling well above the default 64 KiB.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcInbound
		if err := json.Unmarshal(line, &msg); err != nil {
			// Skip non-JSON lines (human-readable banners on
			// startup). Silently drop and keep reading.
			continue
		}
		switch msg.classify() {
		case inboundResponse:
			c.dispatchResponse(msg)
		case inboundNotification:
			c.dispatchNotification(msg, false)
		case inboundServerRequest:
			c.dispatchNotification(msg, true)
		default:
			// Unknown shape — ignore.
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		loopErr = fmt.Errorf("muse jsonrpc: read loop: %w", err)
	}
}

func (c *Client) dispatchResponse(msg rpcInbound) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		// Non-numeric id — we never emit those, so drop.
		return
	}
	c.pendingMu.Lock()
	p, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	resp := rpcResponse{ID: msg.ID, Result: msg.Result, Error: msg.Error}
	select {
	case p.ch <- resp:
	default:
	}
}

func (c *Client) dispatchNotification(msg rpcInbound, isServerRequest bool) {
	sessionID := extractSessionID(msg.Params)
	notif := notification{
		Method: msg.Method,
		Params: msg.Params,
	}
	if isServerRequest {
		notif.ServerRequestID = msg.ID
	}

	c.subsMu.RLock()
	defer c.subsMu.RUnlock()

	if sessionID != "" {
		if h, ok := c.sessionSubs[sessionID]; ok {
			h(notif)
			return
		}
	}
	// Fall through to the global handler. If there is none and this
	// is a server-request, reply with an empty receipt so the host
	// does not hang waiting for us.
	if c.global != nil {
		c.global(notif)
		return
	}
	if isServerRequest {
		_ = c.RespondToServerRequest(msg.ID, map[string]any{})
	}
}

// extractSessionID reads the "sessionId" field from a JSON-RPC params
// payload without fully decoding it. MSP routes every session-scoped
// notification by this field.
func extractSessionID(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return ""
	}
	return probe.SessionID
}

func (c *Client) write(v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(buf, '\n')); err != nil {
		return err
	}
	return nil
}

// RPCError is returned by Request when the server replied with a
// JSON-RPC error object. Callers can errors.As to unwrap. Data carries
// the MSP typed error body (error.data.kind) when the host sent one.
type RPCError struct {
	Method  string
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("muse jsonrpc: %s failed (code=%d): %s", e.Method, e.Code, e.Message)
}

// ErrorKindOf extracts the MSP error.data.kind from an RPCError, or ""
// when the error carries no typed kind.
func ErrorKindOf(err error) string {
	var rpc *RPCError
	if !errors.As(err, &rpc) || len(rpc.Data) == 0 {
		return ""
	}
	var data ErrorData
	if json.Unmarshal(rpc.Data, &data) != nil {
		return ""
	}
	return string(data.Kind)
}

// IsSessionGone reports whether err is a session/resume failure meaning
// the session no longer exists on the host (sessionNotFound / notFound).
func IsSessionGone(err error) bool {
	switch ErrorKindOf(err) {
	case string(ErrorKindSessionNotFound), string(ErrorKindNotFound):
		return true
	}
	return false
}

func ptrInt(i int) *int { return &i }
