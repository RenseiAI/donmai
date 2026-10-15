package result_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestPosterPost_StatusSeatStateSerialized pins the terminal seat-state
// receipt on the /status body: the session axis is exited (the receipt is
// the harness's last word), activity is unknown (the terminal record
// carries no arbiter evidence — never a reconstructed guess), and
// resumability follows the checkpoint evidence the runner already owns.
func TestPosterPost_StatusSeatStateSerialized(t *testing.T) {
	t.Parallel()

	checkpoint := &agent.ResumeCheckpoint{Branch: "wip/sess-9", CommitSHA: "feedfacefeedfacefeedfacefeedfacefeedface"}
	tests := []struct {
		name          string
		result        agent.Result
		wantResumable string
	}{
		{
			name:          "checkpoint is resumable",
			result:        agent.Result{Status: "failed", Resumable: true, ResumeCheckpoint: checkpoint},
			wantResumable: "resumable",
		},
		{
			name:          "resumable flag without checkpoint is resumable",
			result:        agent.Result{Status: "failed", Resumable: true},
			wantResumable: "resumable",
		},
		{
			name:          "failed with nothing preserved is fresh only",
			result:        agent.Result{Status: "failed"},
			wantResumable: "fresh_only",
		},
		{
			name:          "completed without checkpoint is unknown",
			result:        agent.Result{Status: "completed"},
			wantResumable: "unknown",
		},
		{
			name:          "completed resumable stays resumable",
			result:        agent.Result{Status: "completed", Resumable: true},
			wantResumable: "resumable",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var statusBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if len(r.URL.Path) >= 7 && r.URL.Path[len(r.URL.Path)-7:] == "/status" {
					mu.Lock()
					statusBody = body
					mu.Unlock()
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			p := newPoster(t, srv.URL, 0)

			if err := p.Post(context.Background(), "sess-seat", tc.result); err != nil {
				t.Fatalf("Post: %v", err)
			}
			mu.Lock()
			raw := statusBody
			mu.Unlock()
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("status body not JSON: %v", err)
			}
			rawSeat, ok := body["seatState"]
			if !ok {
				t.Fatalf("seatState missing in %s", raw)
			}
			var seat struct {
				Session      string `json:"session"`
				Activity     string `json:"activity"`
				DecidedBy    string `json:"decidedBy"`
				Resumability string `json:"resumability"`
			}
			if err := json.Unmarshal(rawSeat, &seat); err != nil {
				t.Fatalf("seatState not decodable: %v", err)
			}
			if seat.Session != "exited" {
				t.Errorf("seatState.session = %q, want exited", seat.Session)
			}
			if seat.Activity != "unknown" {
				t.Errorf("seatState.activity = %q, want unknown (no arbiter evidence at terminal time)", seat.Activity)
			}
			if seat.DecidedBy != "none" {
				t.Errorf("seatState.decidedBy = %q, want none", seat.DecidedBy)
			}
			if seat.Resumability != tc.wantResumable {
				t.Errorf("seatState.resumability = %q, want %q", seat.Resumability, tc.wantResumable)
			}
			rawSummary, ok := body["seatSummary"]
			if !ok {
				t.Fatalf("seatSummary missing in %s", raw)
			}
			var summary map[string]any
			if err := json.Unmarshal(rawSummary, &summary); err != nil {
				t.Fatalf("seatSummary not decodable: %v", err)
			}
			if summary["total"] != float64(1) {
				t.Errorf("seatSummary.total = %v, want 1", summary["total"])
			}
			if summary["sessionExited"] != float64(1) {
				t.Errorf("seatSummary.sessionExited = %v, want 1", summary["sessionExited"])
			}
		})
	}
}
