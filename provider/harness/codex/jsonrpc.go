package codex

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

// JSON-RPC 2.0 message shapes. See ../agentfactory/packages/core/src/
// providers/codex-app-server-provider.ts for the authoritative legacy
// TS shapes — the Go equivalents are deliberately small subsets that
// the codex app-server actually exercises today.

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

// rpcError mirrors the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcInbound is the union type used while reading the stdio stream.
// Codex multiplexes responses, notifications, and server-requests on
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
	// inboundNotification has a method but no id — server-pushed event.
	inboundNotification
	// inboundServerRequest has both id and method — Codex requesting
	// something from us (typically an approval) and expecting a reply.
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
// must respond via Client.RespondToServerRequest before continuing.
type notification struct {
	// Method is the JSON-RPC method name (e.g. "thread/started",
	// "item/started", "applyPatchApproval").
	Method string

	// Params is the raw JSON params body, decoded lazily by handlers
	// so we do not pay for shapes we never read.
	Params json.RawMessage

	// ServerRequestID is non-nil when this notification represents a
	// JSON-RPC server-request expecting a reply. Handlers must call
	// Client.RespondToServerRequest(*ServerRequestID, ...) exactly
	// once.
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
// The legacy TS `AppServerProcessManager` is the reference; this Go
// port keeps the same responsibilities (request/response correlation,
// notification routing, server-request delivery) but pulls them into a
// dedicated type so handle.go can focus on session semantics.
//
// A Client is created with NewClient(stdin, stdout) and Stop()ped on
// shutdown. Concurrent calls to Request and Notify are safe; the
// underlying writer is mutex-protected.
type Client struct {
	w  io.Writer
	r  io.Reader
	mu sync.Mutex // serialises writes to w

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]*pendingRequest

	subsMu sync.RWMutex
	// threadSubs maps threadId → handler. Notifications carrying a
	// matching threadId are delivered to the corresponding handler.
	threadSubs map[string]notificationHandler
	// global is fired for notifications without a threadId.
	global notificationHandler
	// onClose fires once when the read loop exits. It is the
	// Provider's hook to mark every live Handle as failed.
	onClose func(error)

	closeOnce sync.Once
	closeErr  error
	doneCh    chan struct{}
}

// NewClient wires a Client to a pair of stdio streams.
//
// w is typically the codex app-server child's stdin; r its stdout. The
// caller is responsible for spawning the child process and managing
// its lifecycle outside the Client (codex.go owns that).
func NewClient(w io.Writer, r io.Reader) *Client {
	c := &Client{
		w:          w,
		r:          r,
		pending:    make(map[int64]*pendingRequest),
		threadSubs: make(map[string]notificationHandler),
		doneCh:     make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// SetOnClose registers a hook fired once when the read loop exits.
// The hook receives the terminal error (or nil for clean close). The
// Provider uses this to mark every live Handle as failed when the
// shared app-server crashes.
func (c *Client) SetOnClose(fn func(error)) {
	c.subsMu.Lock()
	c.onClose = fn
	c.subsMu.Unlock()
}

// Subscribe registers a notification handler for a specific threadId.
//
// Handlers are invoked synchronously on the Client's read goroutine.
// Subscribing twice for the same thread replaces the prior handler.
func (c *Client) Subscribe(threadID string, h notificationHandler) {
	c.subsMu.Lock()
	c.threadSubs[threadID] = h
	c.subsMu.Unlock()
}

// Unsubscribe removes a thread handler. Safe to call after Stop.
func (c *Client) Unsubscribe(threadID string) {
	c.subsMu.Lock()
	delete(c.threadSubs, threadID)
	c.subsMu.Unlock()
}

// SubscribeGlobal registers a fall-through handler invoked for
// notifications that do not carry a threadId or whose threadId has no
// dedicated subscriber. Used by codex.go for the post-initialize
// `initialized` notification and similar.
func (c *Client) SubscribeGlobal(h notificationHandler) {
	c.subsMu.Lock()
	c.global = h
	c.subsMu.Unlock()
}

// Request sends a JSON-RPC request and waits for the matching response.
//
// timeout caps how long we wait for the response; a zero timeout
// leaves it bounded only by ctx. The error includes ctx and timeout
// reasons separately so callers can implement backoff.
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
		return nil, fmt.Errorf("codex jsonrpc: write request %s: %w", method, err)
	}

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutCh = t.C
	}

	// On termination paths (ctx cancel, timeout, client close) we still
	// re-check pending.ch before returning the failure. dispatchResponse
	// races readLoop's exit: a response can be delivered to pending.ch
	// in the same instant that c.doneCh closes, and an unguarded select
	// would pick the doneCh case 50% of the time and lose a perfectly
	// good reply, surfacing as a spurious "client stopped" error
	// (REN-1460 follow-up).
	checkResp := func() (json.RawMessage, error, bool) {
		select {
		case resp := <-pending.ch:
			if resp.Error != nil {
				return nil, &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}, true
			}
			return resp.Result, nil, true
		default:
			return nil, nil, false
		}
	}

	select {
	case resp := <-pending.ch:
		if resp.Error != nil {
			return nil, &RPCError{Method: method, Code: resp.Error.Code, Message: resp.Error.Message}
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
		return nil, fmt.Errorf("codex jsonrpc: request %s timed out after %s", method, timeout)
	case <-c.doneCh:
		if res, err, ok := checkResp(); ok {
			return res, err
		}
		err := c.closeErr
		if err == nil {
			err = errors.New("codex jsonrpc: client closed")
		}
		return nil, err
	}
}

// RequestWithRetry wraps Request with the F.1.1 §5.1 3-attempt
// exponential backoff (1s, 2s, 4s) for transient JSON-RPC errors.
//
// "Transient" here means: timeouts, write errors, and JSON-RPC errors
// whose code is not -32601 (Method not found) and not -32700/-32600
// (Parse / Invalid request — those are programmer errors, not worth
// retrying). Permanent errors return immediately.
func (c *Client) RequestWithRetry(ctx context.Context, method string, params map[string]any, perAttempt time.Duration) (json.RawMessage, error) {
	const maxAttempts = 3
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res, err := c.Request(ctx, method, params, perAttempt)
		if err == nil {
			return res, nil
		}
		last = err
		if !isTransient(err) {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		backoff := time.Duration(1<<(attempt-1)) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("codex jsonrpc: %s after %d attempts: %w", method, maxAttempts, last)
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) Notify(method string, params map[string]any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	if err := c.write(req); err != nil {
		return fmt.Errorf("codex jsonrpc: notify %s: %w", method, err)
	}
	return nil
}

// RespondToServerRequest sends a successful JSON-RPC response to a
// server-request previously delivered to a notification handler.
//
// id MUST be the raw json.RawMessage captured from the inbound message
// (Codex sends ints in some versions, strings in others — preserve the
// original encoding to avoid mismatch).
func (c *Client) RespondToServerRequest(id json.RawMessage, result map[string]any) error {
	body := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  map[string]any  `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result}
	if err := c.write(body); err != nil {
		return fmt.Errorf("codex jsonrpc: respond to server request: %w", err)
	}
	return nil
}

// RespondToServerRequestWithError replies with a JSON-RPC error to a
// server-request we cannot satisfy (typically -32601 "Method not
// found"). Mirrors the legacy TS respondToServerRequestWithError; the
// codex side hangs the agent if a server-request never gets a reply,
// so we always send something.
func (c *Client) RespondToServerRequestWithError(id json.RawMessage, code int, message string) error {
	body := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: rpcError{Code: code, Message: message}}
	if err := c.write(body); err != nil {
		return fmt.Errorf("codex jsonrpc: respond with error: %w", err)
	}
	return nil
}

// Stop releases pending requests and closes the read loop.
//
// After Stop, no new Request calls succeed. Pending requests fail with
// the supplied cause (or "client stopped" when nil).
func (c *Client) Stop(cause error) {
	c.closeOnce.Do(func() {
		if cause == nil {
			cause = errors.New("codex jsonrpc: client stopped")
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

		c.subsMu.RLock()
		hook := c.onClose
		c.subsMu.RUnlock()
		if hook != nil {
			hook(cause)
		}
	})
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
//
// Termination protocol (REN-1460): the scanner error is captured into
// a local and passed to Stop as the cause, NOT written to c.closeErr
// directly. Stop's closeOnce serializes the closeErr write with
// CloseErr / Request readers via doneCh's happens-before, so this is
// the only safe path to set the field.
func (c *Client) readLoop() {
	var loopErr error
	defer func() { c.Stop(loopErr) }()

	scanner := bufio.NewScanner(c.r)
	// Codex notifications can be large (full diff payloads, MCP tool
	// results). Bump the buffer ceiling well above the default 64 KiB.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcInbound
		if err := json.Unmarshal(line, &msg); err != nil {
			// Skip non-JSON lines (codex occasionally writes
			// human-readable banners on startup). The legacy TS
			// does the same — silently drop and keep reading.
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
		loopErr = fmt.Errorf("codex jsonrpc: read loop: %w", err)
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
	threadID := extractThreadID(msg.Params)
	notif := notification{
		Method: msg.Method,
		Params: msg.Params,
	}
	if isServerRequest {
		notif.ServerRequestID = msg.ID
	}

	c.subsMu.RLock()
	defer c.subsMu.RUnlock()

	if threadID != "" {
		if h, ok := c.threadSubs[threadID]; ok {
			h(notif)
			return
		}
	}
	// Fall through to the global handler. If there is none and this
	// is a server-request, reply with method-not-found so codex does
	// not hang waiting for us.
	if c.global != nil {
		c.global(notif)
		return
	}
	if isServerRequest {
		_ = c.RespondToServerRequestWithError(msg.ID, -32601, fmt.Sprintf("no handler for %s", msg.Method))
	}
}

// extractThreadID reads the "threadId" field from a JSON-RPC params
// payload without fully decoding it. Codex consistently keys its
// per-thread notifications by this field; the lookup is hot enough to
// justify a focused decoder rather than a generic map[string]any.
func extractThreadID(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return ""
	}
	return probe.ThreadID
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
// JSON-RPC error object. Callers can errors.As to unwrap.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex jsonrpc: %s failed (code=%d): %s", e.Method, e.Code, e.Message)
}

// isTransient classifies whether an error is worth retrying.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// ctx errors are not transient — the caller asked us to stop.
		return false
	}
	var rpc *RPCError
	if errors.As(err, &rpc) {
		switch rpc.Code {
		case -32700, -32600, -32601, -32602:
			// Parse / Invalid request / Method not found / Invalid
			// params — programmer errors. Do not retry.
			return false
		default:
			return true
		}
	}
	// I/O errors (write failures, scanner errors, timeouts) are
	// transient by default.
	return true
}

func ptrInt(i int) *int { return &i }
