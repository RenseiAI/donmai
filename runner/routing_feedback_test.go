package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

func TestComputeRoutingReward(t *testing.T) {
	// Unknown work types preserve the original formula.
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
			got := computeRoutingReward("unknown-work-type", "", tt.taskCompleted, tt.prCreated, tt.qaResult, tt.cost)
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
	res.ProviderName = agent.ProviderClaude
	res.Status = "completed"
	res.WorkResult = "passed"
	res.PullRequestURL = "https://github.com/x/y/pull/1"
	res.IssueIdentifier = "ENG-1"
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

func TestComputeRoutingReward_WorkTypes(t *testing.T) {
	for _, workType := range AllWorkTypes {
		t.Run(workType, func(t *testing.T) {
			if _, ok := routingRewardByWorkType[workType]; !ok {
				t.Fatalf("missing reward definition for %s", workType)
			}
			wantCompletion, wantPR, wantQA := 1.0, 0.0, 0.0
			switch workType {
			case WorkTypeDevelopmentStr, WorkTypeInflight, WorkTypeMerge:
				wantCompletion, wantPR, wantQA = 0.5, 0.2, 0.3
			case WorkTypeQAStr, WorkTypeAcceptance:
				wantCompletion, wantQA = 0.7, 0.3
			}
			for _, completed := range []bool{false, true} {
				for _, pr := range []bool{false, true} {
					for _, qa := range []string{"unknown", "failed", "passed"} {
						for _, cost := range []float64{0, 2.5, 5, 10} {
							got := computeRoutingReward(workType, "", completed, pr, qa, cost)
							if math.IsNaN(got) || got < 0 || got > 1 {
								t.Errorf("reward(%t, %t, %s, %v) = %v, outside [0,1]", completed, pr, qa, cost, got)
							}
						}
					}
				}
			}
			for _, tc := range []struct {
				completed bool
				pr        bool
				qa        string
				want      float64
			}{
				{true, false, "unknown", wantCompletion},
				{false, true, "unknown", wantPR},
				{false, false, "passed", wantQA},
			} {
				if got := computeRoutingReward(workType, "", tc.completed, tc.pr, tc.qa, 0); math.Abs(got-tc.want) > 1e-9 {
					t.Errorf("reward(%t, %t, %s) = %v, want %v", tc.completed, tc.pr, tc.qa, got, tc.want)
				}
			}
		})
	}
}

func TestComputeRoutingReward_ResearchAndDevelopment(t *testing.T) {
	if got := computeRoutingReward(WorkTypeResearch, "", true, false, "unknown", 0); got < 0.8 {
		t.Errorf("completed research reward = %v, want >= 0.8", got)
	}
	withoutPR := computeRoutingReward(WorkTypeDevelopmentStr, "", true, false, "unknown", 0)
	withPR := computeRoutingReward(WorkTypeDevelopmentStr, "", true, true, "unknown", 0)
	if withPR <= withoutPR {
		t.Errorf("development with PR = %v, must exceed without PR = %v", withPR, withoutPR)
	}
}

// TestComputeRoutingReward_CustomNameFollowsRole pins the role fallback
// for organisation-defined names: a custom review name scores exactly
// like qa, a custom intake name like research, a custom inflight name
// like development, and a role-less or terminal custom name like the
// historical default — across the full input grid so no input shape
// escapes the role mapping.
func TestComputeRoutingReward_CustomNameFollowsRole(t *testing.T) {
	t.Parallel()
	roles := []struct {
		role           string
		representative string
	}{
		{"review", WorkTypeQAStr},
		{"intake", WorkTypeResearch},
		{"inflight", WorkTypeDevelopmentStr},
		// Terminal and absent/invalid roles take the default weights,
		// which byte-match the development row's historical values.
		{"terminal", WorkTypeDevelopmentStr},
		{"", WorkTypeDevelopmentStr},
		{"not-a-role", WorkTypeDevelopmentStr},
	}
	for _, r := range roles {
		t.Run("role="+r.role, func(t *testing.T) {
			t.Parallel()
			for _, completed := range []bool{false, true} {
				for _, pr := range []bool{false, true} {
					for _, qa := range []string{"unknown", "failed", "passed"} {
						for _, cost := range []float64{0, 2.5, 5, 10} {
							got := computeRoutingReward("custom-type", r.role, completed, pr, qa, cost)
							want := computeRoutingReward(r.representative, "", completed, pr, qa, cost)
							if diff := got - want; diff > 1e-9 || diff < -1e-9 {
								t.Errorf("reward(custom-type/%s)(%t, %t, %s, %v) = %v, want representative %s = %v",
									r.role, completed, pr, qa, cost, got, r.representative, want)
							}
						}
					}
				}
			}
		})
	}
}

// TestComputeRoutingReward_BuiltinNameWinsOverRole pins the lookup order in
// computeRoutingReward: a built-in name's own weights row wins over any
// stamped role. For every AllWorkTypes name x every role x the input grid,
// the reward with the role must equal the reward with no role. Without
// this pin, looking up the role representative first would stay GREEN
// while silently re-scoring built-ins whose role representative carries a
// different row (for example security scored as development).
func TestComputeRoutingReward_BuiltinNameWinsOverRole(t *testing.T) {
	t.Parallel()
	roles := []string{
		"",
		string(WorkTypeRoleInflight),
		string(WorkTypeRoleReview),
		string(WorkTypeRoleIntake),
		string(WorkTypeRoleTerminal),
		"not-a-role",
	}
	for _, workType := range AllWorkTypes {
		for _, role := range roles {
			t.Run(workType+"/role="+role, func(t *testing.T) {
				t.Parallel()
				for _, completed := range []bool{false, true} {
					for _, pr := range []bool{false, true} {
						for _, qa := range []string{"unknown", "failed", "passed"} {
							for _, cost := range []float64{0, 2.5, 5, 10} {
								got := computeRoutingReward(workType, role, completed, pr, qa, cost)
								want := computeRoutingReward(workType, "", completed, pr, qa, cost)
								if diff := got - want; diff > 1e-9 || diff < -1e-9 {
									t.Errorf("reward(%s/%s)(%t, %t, %s, %v) = %v, want role-less %v",
										workType, role, completed, pr, qa, cost, got, want)
								}
							}
						}
					}
				}
			})
		}
	}
}

func TestRecordRoutingFeedback_RunUsesWorkType(t *testing.T) {
	t.Setenv("ROUTING_RECORDER_ENABLED", "true")
	cases := []struct {
		workType string
		role     string
		want     float64
	}{
		{WorkTypeResearch, "", 1},
		{WorkTypeQAStr, "", 0.7},
		{WorkTypeDevelopmentStr, "", 0.5},
		{"unknown-work-type", "", 0.5},
		// A custom review name scores exactly like the qa row: the
		// production wiring passes the stamped role through to the
		// reward computation. Spelled through the canonical constant
		// so a renamed wire literal without a matching routing update
		// goes RED.
		{"custom-review-desk", string(WorkTypeRoleReview), 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.workType, func(t *testing.T) {
			h := newRunnerHarness(t)
			observations := make(chan routingFeedbackBody, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/routing-feedback") {
					var body routingFeedbackBody
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode observation: %v", err)
					} else {
						observations <- body
					}
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
			}))
			defer srv.Close()
			qw := h.queuedWork("routing-reward")
			qw.WorkType = tc.workType
			qw.WorkTypeRole = tc.role
			qw.PlatformURL = srv.URL
			// This stub turn completes without a PR, verdict or cost.
			qw.ResolvedProfile.ProviderConfig = map[string]any{"stub.behavior": string(stub.BehaviorResumeSteer)}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != "completed" || res.PullRequestURL != "" || res.WorkResult == "passed" {
				t.Fatalf("unexpected session result: status=%s PR=%s verdict=%s", res.Status, res.PullRequestURL, res.WorkResult)
			}
			select {
			case got := <-observations:
				if got.WorkType != tc.workType || math.Abs(got.Reward-tc.want) > 1e-9 {
					t.Errorf("observation workType=%s reward=%v, want %s reward=%v", got.WorkType, got.Reward, tc.workType, tc.want)
				}
			default:
				t.Fatal("Run did not post routing feedback")
			}
		})
	}
}
