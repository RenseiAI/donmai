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
	desc := &workarea.TerminalLeaseDescriptor{
		SchemaVersion:      workarea.TerminalLeaseSchemaV1,
		LeaseID:            "twl_1",
		SessionID:          "session-1",
		TerminalResultID:   "tr_1",
		WorkareaID:         "wa_1",
		AcquiredAt:         now,
		ExpiresAt:          now.Add(30 * time.Minute),
		SettlementBudgetMS: (17 * time.Minute).Milliseconds(),
	}
	if err := poster.PostWithOptions(context.Background(), "session-1", goodResult(), result.PostOptions{
		TerminalWorkareaLease: desc,
	}); err != nil {
		t.Fatalf("PostWithOptions: %v", err)
	}
	if statusBody["terminalWorkareaLease"] == nil {
		t.Fatalf("status body = %#v", statusBody)
	}
	leaseBody := statusBody["terminalWorkareaLease"].(map[string]any)
	if leaseBody["workareaId"] != "wa_1" || leaseBody["workareaPath"] != nil {
		t.Fatalf("lease body = %#v", leaseBody)
	}
	if completionBody["terminalWorkareaLease"] != nil {
		t.Fatalf("completion body leaked descriptor: %#v", completionBody)
	}
}
