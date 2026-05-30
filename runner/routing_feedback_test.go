package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestComputeRoutingReward(t *testing.T) {
	// Mirrors donmai-libraries/packages/core/src/routing/reward.ts.
	tests := []struct {
		name          string
		taskCompleted bool
		prCreated     bool
		qaResult      string
		cost          float64
		want          float64
	}{
		{"clean success, no cost", true, true, "passed", 0, 1.0},
		{"completed only", true, false, "unknown", 0, 0.5},
		{"nothing", false, false, "failed", 0, 0.0},
		{"full minus max cost penalty", true, true, "passed", 5, 0.9},
		{"failure with cost clamps to 0", false, false, "failed", 10, 0.0},
		{"completed+pr, half cost", true, true, "unknown", 2.5, 0.65}, // 0.7 - 0.1*0.5
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRoutingReward(tt.taskCompleted, tt.prCreated, tt.qaResult, tt.cost)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("computeRoutingReward = %v, want %v", got, tt.want)
			}
		})
	}
}

func newFeedbackRunner(client *http.Client) *Runner {
	return &Runner{httpClient: client, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func feedbackWork(srvURL string) QueuedWork {
	qw := QueuedWork{}
	qw.SessionID = "sess-1"
	qw.WorkType = "development"
	qw.PlatformURL = srvURL
	qw.AuthToken = "tok"
	qw.ResolvedProfile.Provider = agent.ProviderClaude
	return qw
}

func completedResult() *Result {
	res := &Result{}
	res.Status = "completed"
	res.WorkResult = "passed"
	res.PullRequestURL = "https://github.com/x/y/pull/1"
	res.IssueIdentifier = "REN-1"
	res.Cost = &agent.CostData{TotalCostUsd: 1.0}
	res.StartedAt = 1000
	res.FinishedAt = 4000
	return res
}

func TestRecordRoutingFeedback_PostsObservation(t *testing.T) {
	var hits atomic.Int32
	var gotPath, gotAuth string
	var gotBody routingFeedbackBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"recorded":true}`))
	}))
	defer srv.Close()

	r := newFeedbackRunner(srv.Client())
	r.recordRoutingFeedback(context.Background(), feedbackWork(srv.URL), completedResult())

	if hits.Load() != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", hits.Load())
	}
	if gotPath != "/api/sessions/sess-1/routing-feedback" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.Provider != "claude" || gotBody.WorkType != "development" {
		t.Errorf("arm = %s/%s", gotBody.Provider, gotBody.WorkType)
	}
	// completed 0.5 + pr 0.2 + passed 0.3 = 1.0; penalty 0.1*min(1/5,1)=0.02 → 0.98
	if diff := gotBody.Reward - 0.98; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("reward = %v, want 0.98", gotBody.Reward)
	}
	if !gotBody.TaskCompleted || !gotBody.PRCreated || gotBody.QAResult != "passed" {
		t.Errorf("observation flags wrong: %+v", gotBody)
	}
	if gotBody.WallClockMs != 3000 {
		t.Errorf("wallClockMs = %d, want 3000", gotBody.WallClockMs)
	}
}

func TestRecordRoutingFeedback_Gating(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	r := newFeedbackRunner(srv.Client())

	t.Run("disabled by env", func(t *testing.T) {
		t.Setenv("ROUTING_RECORDER_ENABLED", "false")
		r.recordRoutingFeedback(context.Background(), feedbackWork(srv.URL), completedResult())
	})
	t.Run("missing workType", func(t *testing.T) {
		t.Setenv("ROUTING_RECORDER_ENABLED", "")
		qw := feedbackWork(srv.URL)
		qw.WorkType = ""
		r.recordRoutingFeedback(context.Background(), qw, completedResult())
	})
	t.Run("missing platform url", func(_ *testing.T) {
		qw := feedbackWork("")
		r.recordRoutingFeedback(context.Background(), qw, completedResult())
	})

	if hits.Load() != 0 {
		t.Fatalf("expected 0 POSTs when gated, got %d", hits.Load())
	}
}
