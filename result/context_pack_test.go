package result_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// TestPostWithOptionsCarriesContextPack pins the terminal plumbing: the
// pack report materialized before spawn must ride the /status post, and a
// run without a pack must stay byte-identical (no key).
func TestPostWithOptionsCarriesContextPack(t *testing.T) {
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
	report := &workarea.ContextPackReport{
		PackID:  "pack1",
		Version: workarea.ContextPackProtocol,
		Dir:     ".workarea/context/pack1",
		Included: []workarea.ContextPackIncluded{
			{Path: "doc.md", Bytes: 4, Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
	}
	if err := poster.PostWithOptions(context.Background(), "session-1", goodResult(), result.PostOptions{ContextPack: report}); err != nil {
		t.Fatalf("PostWithOptions: %v", err)
	}
	packBody, ok := statusBody["contextPack"].(map[string]any)
	if !ok {
		t.Fatalf("status body carries no context pack: %#v", statusBody)
	}
	if packBody["packId"] != "pack1" {
		t.Fatalf("pack body = %#v", packBody)
	}
	included, ok := packBody["included"].([]any)
	if !ok || len(included) != 1 {
		t.Fatalf("included = %#v", packBody["included"])
	}
}

// TestPrepareTerminalStatusBodyWithoutPackStaysByteIdentical pins the
// mixed-version default: no pack means no key.
func TestPrepareTerminalStatusBodyWithoutPackStaysByteIdentical(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "worker", BaseDelay: 0})
	if err != nil {
		t.Fatal(err)
	}
	body, err := poster.PrepareTerminalStatusBodyWithOptions(context.Background(), "session-1", goodResult(), result.PostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, present := envelope["contextPack"]; present {
		t.Fatalf("pack-free body carries the key: %s", body)
	}
}
