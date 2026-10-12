package muse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

// TestClient_RequestResponseCorrelation drives the production Client
// entry point (NewClient + Request) against the fake host: three
// interleaved requests each receive the response bearing their own id.
func TestClient_RequestResponseCorrelation(t *testing.T) {
	t.Parallel()
	cr, cw := io.Pipe()
	sr, sw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close() })
	defer func() { _ = cw.Close(); _ = sw.Close() }()
	c := NewClient(cw, sr)
	defer c.Stop(nil)

	go func() {
		decoder := json.NewDecoder(cr)
		for i := 0; i < 3; i++ {
			var req map[string]any
			if err := decoder.Decode(&req); err != nil {
				return
			}
			id := req["id"]
			buf, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"echo": req["method"]}})
			_, _ = sw.Write(append(buf, '\n'))
		}
	}()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, err := c.Request(ctx, "probe/method", map[string]any{"n": i}, 5*time.Second)
		cancel()
		if err != nil {
			t.Fatalf("Request %d: %v", i, err)
		}
		var res map[string]any
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if res["echo"] != "probe/method" {
			t.Fatalf("Request %d: echo=%v", i, res["echo"])
		}
	}
}

// TestClient_SessionRouting drives the production dispatch entry point:
// a notification carrying params.sessionId reaches only the subscriber
// for that session, and the global handler sees the rest.
func TestClient_SessionRouting(t *testing.T) {
	t.Parallel()
	_, cw := io.Pipe()
	sr, sw := io.Pipe()
	defer func() { _ = cw.Close(); _ = sw.Close() }()
	c := NewClient(cw, sr)
	defer c.Stop(nil)

	gotA := make(chan notification, 4)
	gotGlobal := make(chan notification, 4)
	c.Subscribe("sess-a", func(n notification) { gotA <- n })
	c.SubscribeGlobal(func(n notification) { gotGlobal <- n })

	write := func(body any) {
		buf, _ := json.Marshal(body)
		_, _ = sw.Write(append(buf, '\n'))
	}
	write(map[string]any{"jsonrpc": "2.0", "method": "item/started", "params": map[string]any{"sessionId": "sess-a"}})
	write(map[string]any{"jsonrpc": "2.0", "method": "item/started", "params": map[string]any{"sessionId": "sess-b"}})
	write(map[string]any{"jsonrpc": "2.0", "method": "session/started", "params": map[string]any{"session": map[string]any{}}})

	select {
	case n := <-gotA:
		if n.Method != "item/started" {
			t.Fatalf("sess-a got %q", n.Method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sess-a subscriber saw nothing")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-gotGlobal:
		case <-time.After(5 * time.Second):
			t.Fatalf("global missed notification %d", i)
		}
	}
	select {
	case n := <-gotA:
		t.Fatalf("sess-a leaked a foreign notification: %q", n.Method)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestClient_ServerRequestReceipt drives the production server-request
// path: approval/request and userInput/request shapes classify as
// server-requests and the `{}` presentation receipt answers them.
func TestClient_ServerRequestReceipt(t *testing.T) {
	t.Parallel()
	cr, cw := io.Pipe()
	sr, sw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close() })
	defer func() { _ = cw.Close(); _ = sw.Close() }()
	c := NewClient(cw, sr)
	defer c.Stop(nil)

	// Drain the client's writes so the receipt send never blocks on
	// the unbuffered pipe: the readLoop's scanner owns sr, but
	// nothing reads cr unless this goroutine does.
	replies := make(chan map[string]any, 4)
	go func() {
		decoder := json.NewDecoder(cr)
		for {
			var reply map[string]any
			if err := decoder.Decode(&reply); err != nil {
				return
			}
			replies <- reply
		}
	}()

	seen := make(chan notification, 2)
	c.SubscribeGlobal(func(n notification) { seen <- n })

	buf, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "approval/request",
		"params": map[string]any{"sessionId": "s", "approvalId": "a"},
	})
	_, _ = sw.Write(append(buf, '\n'))

	var n notification
	select {
	case n = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no server-request delivered")
	}
	if len(n.ServerRequestID) == 0 {
		t.Fatal("server-request arrived without its id")
	}
	if err := c.RespondToServerRequest(n.ServerRequestID); err != nil {
		t.Fatalf("receipt: %v", err)
	}

	var reply map[string]any
	select {
	case reply = <-replies:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt never arrived")
	}
	res, _ := reply["result"].(map[string]any)
	if reply["id"] == nil || res == nil || len(res) != 0 {
		t.Fatalf("receipt is not the {} presentation ack: %v", reply)
	}
}

// TestClient_SessionGoneKindsArePermanent drives the production retry
// classifier: sessionNotFound/notFound errors never retry.
func TestClient_SessionGoneKindsArePermanent(t *testing.T) {
	t.Parallel()
	for _, kind := range []ErrorKind{ErrorKindSessionNotFound, ErrorKindNotFound, ""} {
		err := &RPCError{Method: "session/resume", Code: -32020, Kind: kind}
		if kind == "" && isTransient(err) != true {
			t.Fatal("plain retryable code must stay transient")
		}
		if kind != "" {
			if isTransient(err) {
				t.Fatalf("kind %q must be permanent", kind)
			}
			if !IsSessionNotFound(err) {
				t.Fatalf("kind %q must classify as session-gone", kind)
			}
		}
	}
	if IsSessionNotFound(errors.New("other")) {
		t.Fatal("non-RPC error must not classify as session-gone")
	}
}
