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

func TestPostWithOptionsAttachesArchiveReferenceBesideFourFieldLease(t *testing.T) {
	t.Parallel()
	var statusBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if r.URL.Path == "/api/sessions/session-1/status" {
			statusBody = body
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	lease := &workarea.TerminalLeaseProjection{
		LeaseID:          "twl_11111111111111111111111111111111",
		TerminalResultID: "tr_11111111111111111111111111111111",
		WorkareaID:       "wa_11111111111111111111111111111111",
		ExpiresAt:        now.Add(30 * time.Minute),
	}
	reference := &workarea.TerminalArchiveReference{
		SchemaVersion: workarea.TerminalArchiveReferenceSchemaV1, ArchiveID: "wa_11111111111111111111111111111111",
		ArchiveDigest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:       "11111111-1111-4111-8111-111111111111",
		WorkareaID:      "wa_11111111111111111111111111111111",
		TerminalResultID: "tr_11111111111111111111111111111111",
		CommittedAt:     now,
	}
	if err := poster.PostWithOptions(context.Background(), "session-1", goodResult(), result.PostOptions{
		TerminalWorkareaLease: lease, TerminalArchiveReference: reference,
	}); err != nil {
		t.Fatalf("PostWithOptions: %v", err)
	}
	leaseBody, ok := statusBody["terminalWorkareaLease"].(map[string]any)
	if !ok || len(leaseBody) != 4 {
		t.Fatalf("lease projection changed: %#v", statusBody["terminalWorkareaLease"])
	}
	refBody, ok := statusBody["terminalArchiveReference"].(map[string]any)
	if !ok {
		t.Fatalf("status body carries no archive reference: %#v", statusBody)
	}
	if refBody["schemaVersion"] != workarea.TerminalArchiveReferenceSchemaV1 ||
		refBody["archiveId"] != "wa_11111111111111111111111111111111" ||
		refBody["archiveDigest"] != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		refBody["sessionId"] != "11111111-1111-4111-8111-111111111111" ||
		refBody["workareaId"] != "wa_11111111111111111111111111111111" ||
		refBody["terminalResultId"] != "tr_11111111111111111111111111111111" ||
		len(refBody) != 7 {
		t.Fatalf("archive reference body = %#v", refBody)
	}
}

func TestPrepareTerminalStatusBodyWithoutReferenceStaysByteIdentical(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	body, err := poster.PrepareTerminalStatusBody(context.Background(), "session-1", goodResult(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, present := envelope["terminalArchiveReference"]; present {
		t.Fatalf("reference-free body carries the key: %s", body)
	}
}
