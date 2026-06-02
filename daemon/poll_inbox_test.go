package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestPollService_RoutesInboxUserTurn verifies the REN-1563 inbox-routing
// path: a kind="user" inbox message is decoded from the poll response and
// forwarded to OnInbox, keyed by its session id (the daemon then injects it
// into the running session's runner).
func TestPollService_RoutesInboxUserTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResponse{
			Work:             []PollWorkItem{},
			HasInboxMessages: true,
			InboxMessages: map[string][]InboxMessage{
				"sess-itvw": {
					{DeliveryID: "dlv-1", Text: "I want a scheduling app", Kind: "user", TurnID: "turn-1"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	var (
		mu      sync.Mutex
		routed  []InboxMessage
		routeID string
	)
	gotOne := make(chan struct{}, 1)

	p := NewPollService(PollOptions{
		WorkerID:        "wkr_itvw",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		OnInbox: func(sessionID string, msg InboxMessage) error {
			mu.Lock()
			routed = append(routed, msg)
			routeID = sessionID
			mu.Unlock()
			select {
			case gotOne <- struct{}{}:
			default:
			}
			return nil
		},
	})
	p.Start()
	defer p.Stop()

	select {
	case <-gotOne:
	case <-time.After(3 * time.Second):
		t.Fatal("OnInbox never invoked within 3s")
	}

	mu.Lock()
	defer mu.Unlock()
	if routeID != "sess-itvw" {
		t.Fatalf("routed sessionID = %q; want sess-itvw", routeID)
	}
	if len(routed) == 0 {
		t.Fatal("expected at least one routed inbox message")
	}
	m := routed[0]
	if m.Kind != "user" || m.Text != "I want a scheduling app" || m.TurnID != "turn-1" {
		t.Fatalf("routed message mismatch: %+v", m)
	}
}

// TestPollService_NilOnInboxNoPanic verifies a nil OnInbox is back-compat:
// inbox messages are decoded but not routed (the heartbeat lock-refresh
// piggyback remains the primary user-turn transport), and nothing panics.
func TestPollService_NilOnInboxNoPanic(t *testing.T) {
	var served sync.Once
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResponse{
			Work: []PollWorkItem{},
			InboxMessages: map[string][]InboxMessage{
				"sess-x": {{DeliveryID: "d", Text: "hi", Kind: "user", TurnID: "t"}},
			},
		})
		served.Do(func() { close(done) })
	}))
	t.Cleanup(srv.Close)

	p := NewPollService(PollOptions{
		WorkerID:        "wkr_nil",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		// OnInbox intentionally nil.
	})
	p.Start()
	defer p.Stop()

	select {
	case <-done:
		// Served at least once without panicking — give the routeInbox
		// nil-guard a beat to run.
		time.Sleep(50 * time.Millisecond)
	case <-time.After(3 * time.Second):
		t.Fatal("poll endpoint never served within 3s")
	}
}

// TestPollService_SkipsEmptyInboxText verifies whitespace-only inbox
// messages are not routed.
func TestPollService_SkipsEmptyInboxText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PollResponse{
			Work: []PollWorkItem{},
			InboxMessages: map[string][]InboxMessage{
				"sess-y": {
					{DeliveryID: "blank", Text: "   \n\t ", Kind: "user", TurnID: "t1"},
					{DeliveryID: "real", Text: "actual reply", Kind: "user", TurnID: "t2"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	var (
		mu     sync.Mutex
		routed []InboxMessage
	)
	gotReal := make(chan struct{}, 1)
	p := NewPollService(PollOptions{
		WorkerID:        "wkr_blank",
		OrchestratorURL: srv.URL,
		RuntimeJWT:      "rt",
		IntervalSeconds: 1,
		OnWork:          func(PollWorkItem) error { return nil },
		OnInbox: func(_ string, msg InboxMessage) error {
			mu.Lock()
			routed = append(routed, msg)
			mu.Unlock()
			if msg.DeliveryID == "real" {
				select {
				case gotReal <- struct{}{}:
				default:
				}
			}
			return nil
		},
	})
	p.Start()
	defer p.Stop()

	select {
	case <-gotReal:
	case <-time.After(3 * time.Second):
		t.Fatal("real inbox message never routed within 3s")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range routed {
		if m.DeliveryID == "blank" {
			t.Fatal("whitespace-only inbox message must not be routed")
		}
	}
}
