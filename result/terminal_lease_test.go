package result_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func TestNewPosterRejectsNoncanonicalReceiverKey(t *testing.T) {
	t.Parallel()

	_, err := result.NewPoster(result.Options{
		PlatformURL: "https://example.invalid",
		ReceiverKey: "rcv_ABCDEF11111111111111111111111111",
	})
	if err == nil {
		t.Fatal("uppercase receiver key accepted")
	}
}

func TestPostWithOptionsAttachesPathFreeTerminalLeaseToStatusOnly(t *testing.T) {
	t.Parallel()

	var statusBody, completionBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if r.URL.Path == "/api/sessions/session-1/status" {
			statusBody = body
		} else {
			completionBody = body
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	desc := &workarea.TerminalLeaseProjection{
		LeaseID:          "twl_11111111111111111111111111111111",
		TerminalResultID: "tr_11111111111111111111111111111111",
		WorkareaID:       "wa_11111111111111111111111111111111",
		ExpiresAt:        now.Add(30 * time.Minute),
	}
	if err := poster.PostWithOptions(context.Background(), "session-1", goodResult(), result.PostOptions{
		TerminalWorkareaLease: desc,
	}); err != nil {
		t.Fatalf("PostWithOptions: %v", err)
	}
	if statusBody["terminalWorkareaLease"] == nil {
		t.Fatalf("status body = %#v", statusBody)
	}
	if statusBody["worktreePath"] != nil {
		t.Fatalf("status body leaked host-local worktree path: %#v", statusBody)
	}
	leaseBody := statusBody["terminalWorkareaLease"].(map[string]any)
	if leaseBody["workareaId"] != "wa_11111111111111111111111111111111" || leaseBody["workareaPath"] != nil || len(leaseBody) != 4 {
		t.Fatalf("lease body = %#v", leaseBody)
	}
	if completionBody["terminalWorkareaLease"] != nil {
		t.Fatalf("completion body leaked descriptor: %#v", completionBody)
	}
}
