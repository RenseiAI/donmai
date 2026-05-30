package runner

// Router-learning A2 (write side) — runner-in-box edition.
//
// The in-box runner has no Redis client (work arrives over the daemon's local
// control API; results go out over HTTP via the result.Poster). So instead of
// writing the donmai provider×workType posterior store directly, the runner
// POSTs a routing observation to the platform, which owns the Redis write via
// `@donmai/server`'s RedisPosteriorStore/observation store (see
// platform/src/app/api/sessions/[id]/routing-feedback/route.ts).
//
// Best-effort by design: gated by ROUTING_RECORDER_ENABLED (default on), and a
// failure never affects the session's terminal status.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	routingFeedbackTimeout = 10 * time.Second
	// maxExpectedCostUsd mirrors MAX_EXPECTED_COST in
	// donmai-libraries/packages/core/src/routing/reward.ts.
	maxExpectedCostUsd = 5.0
)

// routingFeedbackBody mirrors the platform handler's RoutingFeedbackRequest at
// platform/src/app/api/sessions/[id]/routing-feedback/route.ts.
type routingFeedbackBody struct {
	Provider        string  `json:"provider"`
	WorkType        string  `json:"workType"`
	Reward          float64 `json:"reward"`
	IssueIdentifier string  `json:"issueIdentifier,omitempty"`
	TaskCompleted   bool    `json:"taskCompleted"`
	PRCreated       bool    `json:"prCreated"`
	QAResult        string  `json:"qaResult"`
	TotalCostUsd    float64 `json:"totalCostUsd"`
	WallClockMs     int64   `json:"wallClockMs"`
}

// computeRoutingReward ports computeReward() from
// donmai-libraries/packages/core/src/routing/reward.ts so the Go runner and the
// legacy TS recorder produce comparable rewards in [0,1].
func computeRoutingReward(taskCompleted, prCreated bool, qaResult string, totalCostUsd float64) float64 {
	reward := 0.0
	if taskCompleted {
		reward += 0.5
	}
	if prCreated {
		reward += 0.2
	}
	if qaResult == "passed" {
		reward += 0.3
	}
	reward -= 0.1 * math.Min(totalCostUsd/maxExpectedCostUsd, 1)
	return math.Max(0, math.Min(1, reward))
}

// recordRoutingFeedback POSTs a routing observation for a finished session so
// the platform updates the donmai provider×workType posterior store. Best-effort
// and non-blocking by contract — it must never fail the terminal flow.
func (r *Runner) recordRoutingFeedback(parentCtx context.Context, qw QueuedWork, res *Result) {
	if os.Getenv("ROUTING_RECORDER_ENABLED") == "false" {
		return
	}
	provider := string(qw.resolvedProvider())
	workType := qw.WorkType
	// The arm key is (provider, workType); skip when either is missing, or when
	// we lack the platform coordinates to post.
	if provider == "" || workType == "" || qw.SessionID == "" || qw.PlatformURL == "" || qw.AuthToken == "" {
		return
	}

	qaResult := res.WorkResult
	if qaResult == "" {
		qaResult = "unknown"
	}
	var totalCost float64
	if res.Cost != nil {
		totalCost = res.Cost.TotalCostUsd
	}
	var wallClockMs int64
	if res.FinishedAt > 0 && res.StartedAt > 0 && res.FinishedAt >= res.StartedAt {
		wallClockMs = res.FinishedAt - res.StartedAt
	}
	taskCompleted := res.Status == "completed"
	prCreated := res.PullRequestURL != ""

	body := routingFeedbackBody{
		Provider:        provider,
		WorkType:        workType,
		Reward:          computeRoutingReward(taskCompleted, prCreated, qaResult, totalCost),
		IssueIdentifier: res.IssueIdentifier,
		TaskCompleted:   taskCompleted,
		PRCreated:       prCreated,
		QAResult:        qaResult,
		TotalCostUsd:    totalCost,
		WallClockMs:     wallClockMs,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		r.logger.Warn("routing-feedback: marshal failed (non-fatal)", "err", err)
		return
	}

	// Detached timeout that still honours a hard parent cancel.
	ctx, cancel := context.WithTimeout(context.Background(), routingFeedbackTimeout)
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	url := strings.TrimRight(qw.PlatformURL, "/") + "/api/sessions/" + qw.SessionID + "/routing-feedback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		r.logger.Warn("routing-feedback: build request failed (non-fatal)", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+qw.AuthToken)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logger.Warn("routing-feedback: POST failed (non-fatal)", "sessionId", qw.SessionID, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		r.logger.Warn("routing-feedback: non-2xx (non-fatal)",
			"sessionId", qw.SessionID, "status", resp.StatusCode)
		return
	}
	r.logger.Debug("routing-feedback: recorded",
		"sessionId", qw.SessionID, "provider", provider, "workType", workType, "reward", body.Reward)
}
