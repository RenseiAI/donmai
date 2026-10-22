package afcli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// restoreServer answers POST /api/daemon/workareas/<id>/restore with the
// given status and body and counts the mutating restore requests it saw.
func restoreServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/workareas/wa-src/restore" {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &posts
}

// TestWorkareaRestore_NeverResubmitsTheMutatingPost pins that a failed
// restore is reported, not re-submitted. The daemon may already have
// restored when the response is lost or unreadable, so a second POST would
// materialise a duplicate workarea (or report a misleading session
// conflict when --into-session-id was given).
func TestWorkareaRestore_NeverResubmitsTheMutatingPost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"truncated_body_after_restore", http.StatusOK, `{"workarea":`},
		{"server_error", http.StatusInternalServerError, `{"error":"boom"}`},
		{"forbidden", http.StatusForbidden, `{"error":"denied"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, posts := restoreServer(t, tc.status, tc.body)
			client := afclient.NewDaemonClientFromURL(srv.URL)
			var out bytes.Buffer
			err := runWorkareaRestore(t.Context(), &out, client, "wa-src",
				afclient.WorkareaRestoreRequest{IntoSessionID: "sess-into"}, false)
			if err == nil {
				t.Fatalf("restore succeeded on %s:\n%s", tc.name, out.String())
			}
			if got := posts.Load(); got != 1 {
				t.Fatalf("restore POSTs = %d, want exactly 1 (err=%v)", got, err)
			}
		})
	}
}

// TestWorkareaRestore_OlderDaemonBodyIsOnePost pins that an older daemon,
// whose restore body carries only the legacy fields, is served by the one
// versioned decode: the restore succeeds with a single POST, and the JSON
// output is byte-identical to the legacy encoding of the same body.
func TestWorkareaRestore_OlderDaemonBodyIsOnePost(t *testing.T) {
	t.Parallel()
	legacy := afclient.WorkareaRestoreResult{Workarea: afclient.Workarea{
		ID: "wa-new", Kind: afclient.WorkareaKindActive, ProviderID: "local-pool",
		Status: afclient.WorkareaStatusReady, SessionID: "sess-into", Path: "/tmp/wa-new",
	}}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, jsonOut := range []bool{false, true} {
		srv, posts := restoreServer(t, http.StatusOK, string(body))
		client := afclient.NewDaemonClientFromURL(srv.URL)
		var out bytes.Buffer
		if err := runWorkareaRestore(t.Context(), &out, client, "wa-src", afclient.WorkareaRestoreRequest{}, jsonOut); err != nil {
			t.Fatalf("restore (json=%v): %v", jsonOut, err)
		}
		if got := posts.Load(); got != 1 {
			t.Fatalf("restore POSTs (json=%v) = %d, want 1", jsonOut, got)
		}
		if !jsonOut {
			if !strings.Contains(out.String(), "Workarea restored") || !strings.Contains(out.String(), "wa-new") {
				t.Fatalf("legacy render missing restore lineage:\n%s", out.String())
			}
			continue
		}
		var want bytes.Buffer
		if err := encodeWorkareaJSON(&want, &legacy); err != nil {
			t.Fatal(err)
		}
		if out.String() != want.String() {
			t.Fatalf("JSON output drifted from the legacy shape:\n got %s\nwant %s", out.String(), want.String())
		}
	}
}
