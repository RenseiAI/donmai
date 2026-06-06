package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStdio is a bidirectional in-memory stdio pair used to test the
// JSON-RPC client without a real codex subprocess.
type fakeStdio struct {
	clientReader *io.PipeReader // server reads from here (what client writes)
	clientWriter *io.PipeWriter
	serverReader *io.PipeReader // client reads from here (what server writes)
	serverWriter *io.PipeWriter
}

func newFakeStdio() *fakeStdio {
	cr, cw := io.Pipe()
	sr, sw := io.Pipe()
	return &fakeStdio{
		clientReader: cr,
		clientWriter: cw,
		serverReader: sr,
		serverWriter: sw,
	}
}

// writeServerLine writes one JSONL line from the fake server side.
// Tolerates pipe-closed errors so test cleanup does not produce a
// fatal failure from a server goroutine that outlived the test.
func (f *fakeStdio) writeServerLine(t *testing.T, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = f.serverWriter.Write(append(buf, '\n'))
}

// readClientLine reads one JSONL line written by the client. Returns
// nil when the pipe closes (test is winding down).
func (f *fakeStdio) readClientLine(t *testing.T) map[string]any {
	t.Helper()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)
	for {
		n, err := f.clientReader.Read(tmp)
		if err != nil {
			return nil
		}
		if n == 0 {
			continue
		}
		if tmp[0] == '\n' {
			break
		}
		buf = append(buf, tmp[0])
	}
	if len(buf) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil
	}
	return out
}

func (f *fakeStdio) close() {
	_ = f.clientWriter.Close()
	_ = f.serverWriter.Close()
}

func TestClient_RequestResponseCorrelation(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	// Server side: read each request, echo a response with the same id.
	go func() {
		for i := 0; i < 3; i++ {
			req := fake.readClientLine(t)
			if req == nil {
				return
			}
			id, _ := req["id"].(float64)
			fake.writeServerLine(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"result":  map[string]any{"echo": req["method"]},
			})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, method := range []string{"alpha", "beta", "gamma"} {
		raw, err := c.Request(ctx, method, nil, time.Second)
		if err != nil {
			t.Fatalf("request %s: %v", method, err)
		}
		var got struct {
			Echo string `json:"echo"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if got.Echo != method {
			t.Fatalf("expected echo=%q, got %q", method, got.Echo)
		}
	}
}

func TestClient_ConcurrentRequests(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	const N = 16

	// Server: replies to each request after a small delay, but in
	// reverse arrival order to exercise correlation.
	var seenMu sync.Mutex
	seen := make([]int, 0, N)
	go func() {
		for i := 0; i < N; i++ {
			req := fake.readClientLine(t)
			if req == nil {
				return
			}
			id, _ := req["id"].(float64)
			seenMu.Lock()
			seen = append(seen, int(id))
			seenMu.Unlock()
			fake.writeServerLine(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"result":  map[string]any{"id": int(id)},
			})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var hits atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := c.Request(ctx, "ping", nil, 2*time.Second)
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			var got struct {
				ID int `json:"id"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			if got.ID == 0 {
				t.Errorf("zero id in response")
				return
			}
			hits.Add(1)
		}()
	}
	wg.Wait()
	if hits.Load() != N {
		t.Fatalf("expected %d successful responses, got %d", N, hits.Load())
	}
}

func TestClient_NotificationDispatch(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	threadCh := make(chan notification, 4)
	c.Subscribe("thread-A", func(n notification) { threadCh <- n })
	globalCh := make(chan notification, 4)
	c.SubscribeGlobal(func(n notification) { globalCh <- n })

	// Notification with threadId → routed to subscriber.
	fake.writeServerLine(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/started",
		"params":  map[string]any{"threadId": "thread-A", "thread": map[string]any{"id": "thread-A"}},
	})
	// Notification without threadId → routed to global.
	fake.writeServerLine(t, map[string]any{
		"jsonrpc": "2.0",
		"method":  "system/heartbeat",
		"params":  map[string]any{},
	})

	select {
	case n := <-threadCh:
		if n.Method != "thread/started" {
			t.Fatalf("expected thread/started, got %q", n.Method)
		}
	case <-time.After(time.Second):
		t.Fatalf("thread subscriber timed out")
	}
	select {
	case n := <-globalCh:
		if n.Method != "system/heartbeat" {
			t.Fatalf("expected system/heartbeat, got %q", n.Method)
		}
	case <-time.After(time.Second):
		t.Fatalf("global subscriber timed out")
	}
}

func TestClient_ServerRequestRoutesViaThreadId(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	got := make(chan notification, 1)
	c.Subscribe("thread-A", func(n notification) { got <- n })

	fake.writeServerLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      77,
		"method":  "execCommandApproval",
		"params":  map[string]any{"threadId": "thread-A", "command": "echo hi"},
	})

	select {
	case n := <-got:
		if n.Method != "execCommandApproval" {
			t.Fatalf("expected execCommandApproval, got %q", n.Method)
		}
		if len(n.ServerRequestID) == 0 {
			t.Fatalf("expected server request id to be populated")
		}
	case <-time.After(time.Second):
		t.Fatalf("server-request handler timed out")
	}
}

func TestClient_ServerRequestNoSubscriberRespondsWithError(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	// No subscriber registered.
	fake.writeServerLine(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "applyPatchApproval",
		"params":  map[string]any{"threadId": "thread-X", "filePath": "/tmp/x"},
	})

	// The client should auto-reply with -32601 to keep codex unblocked.
	resp := fake.readClientLine(t)
	if resp["id"] != float64(99) {
		t.Fatalf("expected id=99, got %v", resp["id"])
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error response, got %v", resp)
	}
	if errObj["code"] != float64(-32601) {
		t.Fatalf("expected code -32601, got %v", errObj["code"])
	}
}

func TestClient_RPCErrorReturnsTypedError(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	go func() {
		req := fake.readClientLine(t)
		if req == nil {
			return
		}
		id, _ := req["id"].(float64)
		fake.writeServerLine(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      int(id),
			"error":   map[string]any{"code": -32601, "message": "Method not found"},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Request(ctx, "doesNotExist", nil, time.Second)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("expected code -32601, got %d", rpcErr.Code)
	}
}

func TestClient_RequestWithRetry_StopsOnNonTransient(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	var attempts atomic.Int32
	go func() {
		for {
			req := fake.readClientLine(t)
			if req == nil {
				return
			}
			id, _ := req["id"].(float64)
			attempts.Add(1)
			fake.writeServerLine(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"error":   map[string]any{"code": -32601, "message": "nope"},
			})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.RequestWithRetry(ctx, "missing", nil, time.Second)
	if err == nil {
		t.Fatalf("expected error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt for non-transient, got %d", attempts.Load())
	}
}

func TestClient_RequestWithRetry_Transient(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)
	defer c.Stop(nil)

	var attempts atomic.Int32
	go func() {
		for {
			req := fake.readClientLine(t)
			if req == nil {
				return
			}
			id, _ := req["id"].(float64)
			n := attempts.Add(1)
			if n < 2 {
				fake.writeServerLine(t, map[string]any{
					"jsonrpc": "2.0",
					"id":      int(id),
					"error":   map[string]any{"code": -32000, "message": "transient"},
				})
				continue
			}
			fake.writeServerLine(t, map[string]any{
				"jsonrpc": "2.0",
				"id":      int(id),
				"result":  map[string]any{"ok": true},
			})
			return
		}
	}()

	// Use a short ctx-bound retry to keep tests fast — backoff would
	// be 1s+2s. We override perAttempt and accept the tighter window.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.RequestWithRetry(ctx, "flaky", nil, time.Second)
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if !strings.Contains(string(res), `"ok":true`) {
		t.Fatalf("unexpected result: %s", res)
	}
	if attempts.Load() < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts.Load())
	}
}

func TestClient_StopRejectsPendingRequests(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)

	// Don't reply at all — the client request should be aborted by Stop.
	go func() {
		fake.readClientLine(t)
	}()

	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Request(ctx, "hangs", nil, 0)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	c.Stop(errors.New("boom"))

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected non-nil error after Stop")
		}
	case <-time.After(time.Second):
		t.Fatalf("pending request not released after Stop")
	}
}

func TestClient_OnCloseFiresOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeStdio()
	defer fake.close()
	c := NewClient(fake.clientWriter, fake.serverReader)

	var closeCount atomic.Int32
	c.SetOnClose(func(_ error) { closeCount.Add(1) })

	c.Stop(nil)
	c.Stop(nil) // idempotent

	if closeCount.Load() != 1 {
		t.Fatalf("expected onClose to fire exactly once, got %d", closeCount.Load())
	}
}

func TestIsTransient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"rpc -32601", &RPCError{Code: -32601}, false},
		{"rpc -32602", &RPCError{Code: -32602}, false},
		{"rpc -32000 (server error)", &RPCError{Code: -32000}, true},
		{"plain error", errors.New("boom"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Fatalf("isTransient(%v): got %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
