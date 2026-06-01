package runner

// Router-learning A5b (read side) — runner-in-box edition.
//
// The in-box runner has no Redis client, so it cannot sample the donmai
// provider×workType posterior store directly to pick a model provider for a
// session. Instead, on the session-start critical path, it POSTs the work type
// + its host-registered candidate providers to the platform, which owns the
// Redis read via `@donmai/core`'s Thompson sampler over RedisPosteriorStore
// (see platform/src/app/api/sessions/[id]/routing-select/route.ts).
//
// Posture (mirrors A2 / recordRoutingFeedback):
//   - DARK by default. Gated by ROUTING_SELECTOR_ENABLED (default OFF). The
//     platform flag is the AUTHORITATIVE kill-switch; this local gate only
//     avoids a pointless round-trip when an operator hasn't opted in.
//   - The platform OWNS the explicit-choice guard — when a user pinned a
//     provider the platform returns source='explicit' and we keep the static
//     choice. We never have to reason about it here.
//   - Hard static fallback on EVERY failure mode (flag-off, missing
//     coordinates, proxy error/timeout, empty/unknown result, returned
//     provider not in candidates). Never errors out the run.
//
// Tight timeout: this sits on the session-start critical path (BEFORE the
// provider resolves and the agent spawns), unlike the terminal-tail recorder.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// routingSelectTimeout caps the session-start provider-selection round-trip.
// Kept tight (3s) because it blocks step 1 of runLoop; on any slowness we fall
// back to the statically-resolved provider rather than stall the spawn.
const routingSelectTimeout = 3 * time.Second

// routingSelectRequest mirrors the platform handler's RoutingSelectRequest at
// platform/src/app/api/sessions/[id]/routing-select/route.ts.
type routingSelectRequest struct {
	WorkType   string   `json:"workType"`
	Candidates []string `json:"candidates"`
	Project    string   `json:"project,omitempty"`
	SessionID  string   `json:"sessionId"`
}

// routingSelectResponse mirrors the platform handler's RoutingSelectResponse.
type routingSelectResponse struct {
	SelectedProvider string  `json:"selectedProvider"`
	Source           string  `json:"source"`
	ExpectedReward   float64 `json:"expectedReward,omitempty"`
	Confidence       float64 `json:"confidence,omitempty"`
}

// selectProviderByPosterior asks the platform to sample the donmai
// provider×workType posterior store for the best provider for this session's
// work type, restricted to the host-registered candidate providers.
//
// Returns (selected, true) ONLY when the platform returned a non-empty
// provider that is also one of the candidates we offered. Every failure mode
// returns ("", false) so the caller keeps the statically-resolved provider.
// It never returns an error: a routing-store blip must never affect the run.
func (r *Runner) selectProviderByPosterior(parentCtx context.Context, qw QueuedWork) (string, bool) {
	// Local DARK gate (default OFF). The platform flag is authoritative; this
	// just avoids a needless round-trip when nobody opted in.
	if os.Getenv("ROUTING_SELECTOR_ENABLED") != "true" {
		return "", false
	}

	// Coordinate guard: without the platform coordinates we cannot POST.
	if qw.SessionID == "" || qw.PlatformURL == "" || qw.AuthToken == "" || qw.WorkType == "" {
		return "", false
	}

	// Candidate set = host-registered providers, minus the deterministic
	// test-only stub provider (never a real routing target).
	candidates := make([]string, 0)
	for _, n := range r.registry.Names() {
		name := string(n)
		if name == "" || name == string(agent.ProviderStub) {
			continue
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return "", false
	}

	reqBody := routingSelectRequest{
		WorkType:   qw.WorkType,
		Candidates: candidates,
		Project:    qw.ProjectName,
		SessionID:  qw.SessionID,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		r.logger.Warn("routing-select: marshal failed (non-fatal)", "err", err)
		return "", false
	}

	// Tight timeout that still honours a hard parent cancel.
	ctx, cancel := context.WithTimeout(context.Background(), routingSelectTimeout)
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	url := strings.TrimRight(qw.PlatformURL, "/") + "/api/sessions/" + qw.SessionID + "/routing-select"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		r.logger.Warn("routing-select: build request failed (non-fatal)", "err", err)
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+qw.AuthToken)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logger.Warn("routing-select: POST failed (non-fatal, static fallback)",
			"sessionId", qw.SessionID, "err", err)
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		r.logger.Warn("routing-select: non-2xx (non-fatal, static fallback)",
			"sessionId", qw.SessionID, "status", resp.StatusCode)
		return "", false
	}

	var out routingSelectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		r.logger.Warn("routing-select: decode failed (non-fatal, static fallback)",
			"sessionId", qw.SessionID, "err", err)
		return "", false
	}

	selected := strings.TrimSpace(out.SelectedProvider)
	if selected == "" {
		// disabled / explicit / fallback — keep the static choice.
		r.logger.Debug("routing-select: no override (static fallback)",
			"sessionId", qw.SessionID, "source", out.Source)
		return "", false
	}

	// Defense-in-depth: only accept a provider we actually offered.
	for _, c := range candidates {
		if c == selected {
			r.logger.Info("routing-select: provider chosen by posterior",
				"sessionId", qw.SessionID, "provider", selected, "source", out.Source,
				"expectedReward", out.ExpectedReward, "confidence", out.Confidence)
			return selected, true
		}
	}
	r.logger.Warn("routing-select: returned provider not in candidates (static fallback)",
		"sessionId", qw.SessionID, "returned", selected)
	return "", false
}
