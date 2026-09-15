package attachclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

func TestV2HostCandidateDoneAndErrOnIdleCarrierClose(t *testing.T) {
	t.Parallel()
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck // fixture owns the connection
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			serverErr <- err
			return
		}
		// Close without sending any HostFrame: the candidate's idle read loop
		// must still publish terminal state to its embedding host.
		serverErr <- conn.Close(websocket.StatusInternalError, "fixture carrier closed")
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		DurableHighWater: 4,
	})
	if err != nil {
		t.Fatalf("DialV2HostCandidate: %v", err)
	}
	select {
	case <-candidate.Done():
	case <-ctx.Done():
		t.Fatal("Done did not close after idle carrier close")
	}
	if err := candidate.Err(); err == nil {
		t.Fatal("Err is nil after idle carrier close")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fixture server: %v", err)
	}
}

func TestV2HostCandidateClosePublishesStableTerminalState(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // fixture owns the connection
		_, _, _ = conn.Read(r.Context())
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{
		AttachURL:        strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/session-v2",
		TokenSource:      func(context.Context) (string, error) { return v2TestToken(t, nil), nil },
		DurableHighWater: 4,
	})
	if err != nil {
		t.Fatalf("DialV2HostCandidate: %v", err)
	}
	// Close retains its existing wire-close return value. A peer that does not
	// acknowledge the close may time out; terminal observability must still be
	// published regardless.
	_ = candidate.Close()
	<-candidate.Done()
	first := candidate.Err()
	if first == nil {
		t.Fatal("Err is nil after Close")
	}
	if candidate.Err() != first {
		t.Fatal("Err did not retain the first explicit-close cause")
	}
}
