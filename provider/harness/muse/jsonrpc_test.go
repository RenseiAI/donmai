package muse

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestClient_RequestResponseCorrelation(t *testing.T) {
	// clientW (client writes) feeds hostR (host reads); hostW (host
	// writes) feeds clientR (client reads).
	clientR, hostW := io.Pipe()
	hostR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = hostR.Close()
		_ = hostW.Close()
	})
	c := NewClient(clientW, clientR)
	t.Cleanup(func() { c.Stop(nil) })

	// Fake host: answer every request with {"ok":true}.
	go func() {
		dec := json.NewDecoder(hostR)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			id, hasID := msg["id"]
			if !hasID {
				continue
			}
			buf, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"ok": true}})
			_, _ = hostW.Write(append(buf, '\n'))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := c.Request(ctx, "session/start", map[string]any{"sessionId": "s"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Errorf("result=%s", raw)
	}
}

func TestClient_SessionRoutingAndServerRequestReceipt(t *testing.T) {
	clientR, hostW := io.Pipe()
	hostR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = hostR.Close()
		_ = hostW.Close()
	})
	c := NewClient(clientW, clientR)
	t.Cleanup(func() { c.Stop(nil) })
	// Drain the client's writes (the `{}` receipt) so Respond does
	// not block on the pipe.
	go func() { _, _ = io.Copy(io.Discard, hostR) }()

	got := make(chan notification, 4)
	c.Subscribe("sess-A", func(n notification) { got <- n })

	// Push a session notification + a server-request on the read side.
	_, _ = hostW.Write([]byte(`{"jsonrpc":"2.0","method":"item/delta","params":{"sessionId":"sess-A","itemId":"i","delta":"x","viewCursor":"c"}}` + "\n"))
	_, _ = hostW.Write([]byte(`{"jsonrpc":"2.0","method":"approval/request","id":7,"params":{"sessionId":"sess-A"}}` + "\n"))

	timeout := time.After(5 * time.Second)
	seenNotif, seenReq := false, false
	for !(seenNotif && seenReq) {
		select {
		case n := <-got:
			if len(n.ServerRequestID) > 0 {
				seenReq = true
				if n.Method != "approval/request" {
					t.Errorf("server-request method=%q", n.Method)
				}
				// The must-answer receipt is `{}`.
				if err := c.RespondToServerRequest(n.ServerRequestID, map[string]any{}); err != nil {
					t.Errorf("receipt: %v", err)
				}
			} else {
				seenNotif = true
			}
		case <-timeout:
			t.Fatalf("seenNotif=%v seenReq=%v", seenNotif, seenReq)
		}
	}
}

func TestClient_RPCErrorCarriesKind(t *testing.T) {
	clientR, hostW := io.Pipe()
	hostR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = hostR.Close()
		_ = hostW.Close()
	})
	c := NewClient(clientW, clientR)
	t.Cleanup(func() { c.Stop(nil) })

	go func() {
		dec := json.NewDecoder(hostR)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
				return
			}
			id, hasID := msg["id"]
			if !hasID {
				continue
			}
			buf, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32000, "message": "gone", "data": map[string]any{"kind": "sessionNotFound"}},
			})
			_, _ = hostW.Write(append(buf, '\n'))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Request(ctx, "session/resume", map[string]any{}, 5*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if ErrorKindOf(err) != "sessionNotFound" {
		t.Errorf("kind=%q, want sessionNotFound", ErrorKindOf(err))
	}
	if !IsSessionGone(err) {
		t.Errorf("IsSessionGone=false for sessionNotFound: %v", err)
	}
}

func TestProvider_CapabilitiesProjectManifest(t *testing.T) {
	p, err := New(Options{skipProcess: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mf := p.Manifest()
	caps := p.Capabilities()
	if mf.Caps.SupportsMessageInjection != caps.SupportsMessageInjection ||
		mf.Caps.SupportsSessionResume != caps.SupportsSessionResume ||
		mf.Caps.SupportsToolPlugins != caps.SupportsToolPlugins ||
		mf.Caps.AcceptsMcpServerSpec != caps.AcceptsMcpServerSpec ||
		mf.Caps.AcceptsAllowedToolsList != caps.AcceptsAllowedToolsList ||
		mf.Caps.EmitsSubagentEvents != caps.EmitsSubagentEvents ||
		mf.Caps.SupportsReasoningEffort != caps.SupportsReasoningEffort ||
		mf.Caps.ToolPermissionFormat != caps.ToolPermissionFormat {
		t.Errorf("manifest does not project Capabilities: %+v vs %+v", mf.Caps, caps)
	}
	if mf.Name != agent.HarnessMuse {
		t.Errorf("manifest name=%q, want muse", mf.Name)
	}
	if p.Name() != agent.ProviderMuse {
		t.Errorf("provider name=%q, want muse", p.Name())
	}
}

func TestNew_MissingBinaryUnavailable(t *testing.T) {
	_, err := New(Options{MuseBin: "this-binary-does-not-exist-muse-12345"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isUnavailable(err) {
		t.Errorf("expected ErrProviderUnavailable, got %v", err)
	}
}

func isUnavailable(err error) bool {
	for err != nil {
		if err == agent.ErrProviderUnavailable {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
