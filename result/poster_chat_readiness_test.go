package result_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

// TestPostRunningChatReadiness_Golden pins the running chat-readiness wire:
// status running plus the optional chatReadiness object with version 1.
func TestPostRunningChatReadiness_Golden(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/status") {
			got = b
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)
	readiness := &afclient.SessionChatReadiness{
		Version: 1, Channel: "hook", State: "idle",
		LiveSupported: true, QueueSupported: true, Reason: "",
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := p.PostRunningChatReadiness(context.Background(), "sess-chat-1", readiness); err != nil {
		t.Fatalf("PostRunningChatReadiness: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("running body not JSON: %v", err)
	}
	if body["status"] != "running" {
		t.Fatalf("status = %v; want running", body["status"])
	}
	chat, ok := body["chatReadiness"].(map[string]any)
	if !ok {
		t.Fatalf("chatReadiness missing: %v", body)
	}
	if chat["version"] != float64(1) || chat["channel"] != "hook" || chat["state"] != "idle" {
		t.Fatalf("chatReadiness identity = %v; want version 1 hook idle", chat)
	}
	if chat["liveSupported"] != true || chat["queueSupported"] != true {
		t.Fatalf("chatReadiness support = %v; want live+queue true", chat)
	}
	if chat["reason"] != "" {
		t.Fatalf("reason = %v; want empty when live", chat["reason"])
	}
	observed, _ := chat["observedAt"].(string)
	if _, err := time.Parse(time.RFC3339, observed); err != nil {
		t.Fatalf("observedAt = %q not RFC3339: %v", observed, err)
	}
}

// TestPostRunningChatReadiness_EmptySessionID pins the unpinned validation
// guard: an empty session ID returns the typed error and posts nothing,
// instead of a malformed /api/sessions//status POST.
func TestPostRunningChatReadiness_EmptySessionID(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)
	readiness := &afclient.SessionChatReadiness{
		Version: 1, Channel: "hook", State: "idle",
		LiveSupported: true, QueueSupported: true,
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
	}
	err := p.PostRunningChatReadiness(context.Background(), "", readiness)
	if err == nil || !strings.Contains(err.Error(), "sessionID is required") {
		t.Fatalf("err = %v; want sessionID is required", err)
	}
	if calls != 0 {
		t.Fatalf("posts = %d; want 0 for empty session ID", calls)
	}
}

// TestPosterPost_TerminalOmitsChatReadiness proves terminal posts never carry
// the field: the terminal wire stays byte-identical for old receivers.
func TestPosterPost_TerminalOmitsChatReadiness(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/status") {
			got = b
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p := newPoster(t, srv.URL, 0)
	if err := p.Post(context.Background(), "sess-chat-2", goodResult()); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if strings.Contains(string(got), "chatReadiness") {
		t.Fatalf("terminal status carries chatReadiness: %s", got)
	}
}
