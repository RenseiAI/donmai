package hostrelayclient

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/hostrelay"
	"github.com/coder/websocket"
)

func TestClientForwardsRoundTripAndPreservesAuthorization(t *testing.T) {
	var localCalls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		localCalls.Add(1)
		if r.Method != hostrelay.Method || r.URL.Path != hostrelay.LocalRoute {
			t.Errorf("local request = %s %s, want %s %s", r.Method, r.URL.Path, hostrelay.Method, hostrelay.LocalRoute)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer warm.host.token"; got != want {
			t.Errorf("Authorization = %q, want exact %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer local.Close()

	response := make(chan hostrelay.Response, 1)
	relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
		mustHello(t, ctx, conn)
		writeMessage(t, ctx, conn, hostrelay.Request{
			RequestID: "request-1", Method: hostrelay.Method, Path: hostrelay.LocalRoute,
			Headers: []hostrelay.Header{{Name: "Authorization", Values: []string{"Bearer warm.host.token"}}},
			Body:    []byte(`{"tool":"repo_map"}`), DeadlineUnixMilli: time.Now().Add(time.Second).UnixMilli(),
		})
		message := readMessage(t, ctx, conn)
		got, ok := message.(*hostrelay.Response)
		if !ok {
			t.Errorf("response type = %T, want *hostrelay.Response", message)
			return
		}
		response <- *got
		<-ctx.Done()
	})
	defer relay.Close()

	stop := runClient(t, relay.URL, local.URL, 1)
	defer stop()
	select {
	case got := <-response:
		if got.Status != http.StatusOK || string(got.Body) != `{"ok":true}` {
			t.Errorf("response = status %d body %q, want 200 and JSON body", got.Status, got.Body)
		}
		if got.Headers[0].Name != "Content-Type" || got.Headers[0].Values[0] != "application/json" {
			t.Errorf("response headers = %#v, want selected Content-Type", got.Headers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive forwarded response")
	}
	if got := localCalls.Load(); got != 1 {
		t.Errorf("local calls = %d, want 1", got)
	}
}

func TestClientDeadlineAndCancellation(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		var calls atomic.Int32
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusOK)
				return
			}
			calls.Add(1)
		}))
		defer local.Close()
		response := make(chan hostrelay.Response, 1)
		relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
			mustHello(t, ctx, conn)
			writeMessage(t, ctx, conn, request("expired", time.Now().Add(-time.Second)))
			response <- *readMessage(t, ctx, conn).(*hostrelay.Response)
			<-ctx.Done()
		})
		defer relay.Close()
		stop := runClient(t, relay.URL, local.URL, 1)
		defer stop()
		select {
		case got := <-response:
			if got.Status != responseDeadlineExceeded {
				t.Errorf("deadline response = %d, want %d", got.Status, responseDeadlineExceeded)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not receive deadline response")
		}
		if got := calls.Load(); got != 0 {
			t.Errorf("local calls = %d, want 0 after expired deadline", got)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		started := make(chan struct{})
		cancelled := make(chan struct{})
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusOK)
				return
			}
			close(started)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(cancelled)
		}))
		defer local.Close()
		response := make(chan hostrelay.Response, 1)
		relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
			mustHello(t, ctx, conn)
			writeMessage(t, ctx, conn, request("cancelled", time.Now().Add(time.Second)))
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
			writeMessage(t, ctx, conn, hostrelay.Cancel{RequestID: "cancelled"})
			message := readMessage(t, ctx, conn)
			got, ok := message.(*hostrelay.Response)
			if !ok {
				t.Errorf("cancel response type = %T", message)
				return
			}
			response <- *got
			<-ctx.Done()
		})
		defer relay.Close()
		stop := runClient(t, relay.URL, local.URL, 1)
		defer stop()
		select {
		case <-cancelled:
		case <-time.After(3 * time.Second):
			t.Fatal("loopback request did not observe cancellation")
		}
		select {
		case got := <-response:
			if got.Status != responseCancelled {
				t.Errorf("cancel response = %d, want %d", got.Status, responseCancelled)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not receive cancel response")
		}
	})
}

func TestClientBackpressureAndOversizeRejection(t *testing.T) {
	t.Run("backpressure", func(t *testing.T) {
		block := make(chan struct{})
		var calls atomic.Int32
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusOK)
				return
			}
			calls.Add(1)
			<-block
		}))
		defer func() { close(block); local.Close() }()
		response := make(chan hostrelay.Response, 1)
		relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
			mustHello(t, ctx, conn)
			writeMessage(t, ctx, conn, request("one", time.Now().Add(time.Second)))
			writeMessage(t, ctx, conn, request("two", time.Now().Add(time.Second)))
			writeMessage(t, ctx, conn, request("three", time.Now().Add(time.Second)))
			for {
				message := readMessage(t, ctx, conn)
				got, ok := message.(*hostrelay.Response)
				if ok && got.RequestID == "three" {
					response <- *got
					<-ctx.Done()
					return
				}
			}
		})
		defer relay.Close()
		stop := runClient(t, relay.URL, local.URL, 2)
		defer stop()
		select {
		case got := <-response:
			if got.Status != responseOverloaded {
				t.Errorf("overload response = %d, want %d", got.Status, responseOverloaded)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not receive overload response")
		}
		if got := calls.Load(); got > 2 {
			t.Errorf("local calls = %d, want at most 2", got)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		var calls atomic.Int32
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				w.WriteHeader(http.StatusOK)
				return
			}
			calls.Add(1)
		}))
		defer local.Close()
		delivered := make(chan struct{})
		var deliveredOnce sync.Once
		relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
			first := false
			deliveredOnce.Do(func() { first = true })
			if !first {
				<-ctx.Done()
				return
			}
			mustHello(t, ctx, conn)
			body := base64.StdEncoding.EncodeToString(make([]byte, hostrelay.MaxRequestBodyBytes+1))
			frame := fmt.Sprintf(`{"type":"request","payload":{"requestId":"oversize","method":"POST","path":"/v1/tools/call","headers":[],"body":"%s","deadlineUnixMilli":%d}}`, body, time.Now().Add(time.Second).UnixMilli())
			if err := conn.Write(ctx, websocket.MessageBinary, []byte(frame)); err != nil {
				t.Errorf("write oversize frame: %v", err)
			}
			close(delivered)
		})
		defer relay.Close()
		stop := runClient(t, relay.URL, local.URL, 1)
		defer stop()
		select {
		case <-delivered:
		case <-time.After(3 * time.Second):
			t.Fatal("relay did not deliver oversize frame")
		}
		time.Sleep(100 * time.Millisecond)
		if got := calls.Load(); got != 0 {
			t.Errorf("local calls = %d, want 0 for oversized request", got)
		}
	})
}

func TestClientDisconnectCancelsWithoutReplay(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		close(started)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer local.Close()

	secondLeg := make(chan struct{})
	var secondLegOnce sync.Once
	var accepts atomic.Int32
	relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
		leg := accepts.Add(1)
		mustHello(t, ctx, conn)
		if leg == 1 {
			writeMessage(t, ctx, conn, request("no-replay", time.Now().Add(time.Second)))
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
			conn.CloseNow()
			return
		}
		secondLegOnce.Do(func() { close(secondLeg) })
		replayed := make(chan struct{}, 1)
		go func() {
			if _, _, err := conn.Read(ctx); err == nil {
				replayed <- struct{}{}
			}
		}()
		select {
		case <-replayed:
			t.Error("second tunnel leg received a replayed request response")
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
		}
		<-ctx.Done()
	})
	defer relay.Close()
	stop := runClient(t, relay.URL, local.URL, 1)
	defer stop()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel loopback request")
	}
	select {
	case <-secondLeg:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not reconnect after disconnect")
	}
}

func TestClientLivenessKeepsHealthyTunnelConnected(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected local path %q", r.URL.Path)
	}))
	defer local.Close()

	ponged := make(chan struct{})
	relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
		mustHello(t, ctx, conn)
		readCtx, cancel := context.WithTimeout(ctx, hostrelay.PingInterval+2*time.Second)
		defer cancel()
		typ, data, err := conn.Read(readCtx)
		if err != nil {
			t.Errorf("read client liveness ping: %v", err)
			return
		}
		if typ != websocket.MessageBinary {
			t.Errorf("liveness frame type = %v, want binary", typ)
			return
		}
		message, err := hostrelay.Decode(data)
		if err != nil {
			t.Errorf("decode liveness frame: %v", err)
			return
		}
		ping, ok := message.(*hostrelay.Ping)
		if !ok {
			t.Errorf("liveness message = %T, want *hostrelay.Ping", message)
			return
		}
		writeMessage(t, ctx, conn, hostrelay.Pong{Nonce: ping.Nonce})
		close(ponged)
		<-ctx.Done()
	})
	defer relay.Close()

	stop := runClient(t, relay.URL, local.URL, 1)
	defer stop()
	select {
	case <-ponged:
	case <-time.After(hostrelay.PingInterval + 3*time.Second):
		t.Fatal("client did not complete a five-second liveness exchange")
	}
}

func TestClientDoesNotAttachBeforeLocalReadiness(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t.Errorf("unexpected local path %q", r.URL.Path)
	}))
	defer local.Close()

	hello := make(chan struct{}, 1)
	relay := newRelay(t, "tunnel-secret", func(ctx context.Context, conn *websocket.Conn) {
		readCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if typ, data, err := conn.Read(readCtx); err == nil && typ == websocket.MessageBinary {
			if message, decodeErr := hostrelay.Decode(data); decodeErr == nil {
				if _, ok := message.(*hostrelay.Hello); ok {
					hello <- struct{}{}
				}
			}
		}
	})
	defer relay.Close()

	stop := runClient(t, relay.URL, local.URL, 1)
	defer stop()
	select {
	case <-hello:
		t.Fatal("unready local host sent a tunnel hello")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEnvironmentTokenProviderKeepsSecretsIsolated(t *testing.T) {
	t.Setenv("CODE_INTEL_HOST_JWT_SECRET", "host-signing-secret")
	t.Setenv("HOST_RELAY_TUNNEL_TOKEN", "relay-tunnel-token")
	provider := EnvironmentTokenProvider("HOST_RELAY_TUNNEL_TOKEN")
	got, err := provider(context.Background())
	if err != nil {
		t.Fatalf("provider() error = %v", err)
	}
	if got != "relay-tunnel-token" {
		t.Errorf("provider() = %q, want tunnel token", got)
	}
	if got == "host-signing-secret" {
		t.Error("provider returned the warm-host signing secret")
	}
}

func request(id string, deadline time.Time) hostrelay.Request {
	return hostrelay.Request{
		RequestID: id, Method: hostrelay.Method, Path: hostrelay.LocalRoute,
		Headers: []hostrelay.Header{{Name: "Authorization", Values: []string{"Bearer warm.host.token"}}},
		Body:    []byte(`{}`), DeadlineUnixMilli: deadline.UnixMilli(),
	}
}

func newRelay(t *testing.T, tunnelToken string, serve func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hostrelay.TunnelPath {
			t.Errorf("tunnel path = %q, want %q", r.URL.Path, hostrelay.TunnelPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+tunnelToken; got != want {
			t.Errorf("tunnel Authorization = %q, want %q", got, want)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{hostrelay.Subprotocol}})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		serve(r.Context(), conn)
	}))
}

func runClient(t *testing.T, relayURL, localURL string, maxInFlight int) func() {
	t.Helper()
	client, err := New(Config{
		RelayURL: strings.Replace(relayURL, "http://", "ws://", 1), LocalURL: localURL,
		TokenProvider: func(context.Context) (string, error) { return "tunnel-secret", nil },
		Workload:      hostrelay.Workload{OrgID: "org", PoolID: "pool", WorkerHostID: "host", WorkloadID: "workload"},
		MaxInFlight:   maxInFlight, ReconnectDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && err != context.Canceled {
				t.Errorf("Run() error = %v, want context cancellation", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run() did not return after cancellation")
		}
	}
}

func mustHello(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	message := readMessage(t, ctx, conn)
	hello, ok := message.(*hostrelay.Hello)
	if !ok {
		t.Fatalf("first message = %T, want *hostrelay.Hello", message)
	}
	if hello.Version != hostrelay.Version || hello.LocalRoute != hostrelay.LocalRoute || !hello.Ready {
		t.Errorf("hello = %#v, want ready v1 local route", hello)
	}
}

func writeMessage(t *testing.T, ctx context.Context, conn *websocket.Conn, message hostrelay.Message) {
	t.Helper()
	data, err := hostrelay.Encode(message)
	if err != nil {
		t.Fatalf("encode %T: %v", message, err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatalf("write %T: %v", message, err)
	}
}

func readMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) hostrelay.Message {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	typ, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read websocket: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("websocket type = %v, want binary", typ)
	}
	message, err := hostrelay.Decode(data)
	if err != nil {
		t.Fatalf("decode websocket frame: %v", err)
	}
	return message
}
