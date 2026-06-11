package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// thinkingLevelEnum is the documented accepted-value set for Gemini 3.x
// thinkingConfig.thinkingLevel, captured as a fixture from the Google AI
// for Developers documentation (https://ai.google.dev/gemini-api/docs/thinking,
// re-verified 2026-06-10): the REST example shows "thinkingLevel": "low"
// and the accepted values are minimal | low | medium | high — all
// lowercase. No uppercase variants are documented.
var thinkingLevelEnum = map[string]bool{
	"minimal": true,
	"low":     true,
	"medium":  true,
	"high":    true,
}

// Every effort mapping (including the default branch) must emit a member
// of the documented enum, byte-exact lowercase. An uppercase or
// mixed-case value would be rejected by the live API.
func TestThinkingLevelForEffort_DocumentedEnumCasing(t *testing.T) {
	t.Parallel()
	efforts := []agent.EffortLevel{
		agent.EffortLow,
		agent.EffortMedium,
		agent.EffortHigh,
		agent.EffortXHigh,
		agent.EffortLevel("unknown-tier"), // default branch
	}
	for _, e := range efforts {
		got := thinkingLevelForEffort(e)
		if !thinkingLevelEnum[got] {
			t.Errorf("thinkingLevelForEffort(%q) = %q, not in the documented enum (minimal|low|medium|high)", e, got)
		}
		if got != strings.ToLower(got) {
			t.Errorf("thinkingLevelForEffort(%q) = %q, must be lowercase on the wire", e, got)
		}
	}
}

// The wire payload must carry the camelCase key and lowercase value
// byte-exact: `"thinkingLevel":"low"`. This pins the full serialization
// path (Spec.Effort → thinkingConfigFor → JSON) against the documented
// REST shape so a struct-tag or mapping regression cannot ship silently.
func TestThinkingLevel_WireShapeFixture(t *testing.T) {
	t.Parallel()
	plan, err := buildSpawnPlan(agent.Spec{Prompt: "x", Effort: agent.EffortLow}, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("buildSpawnPlan: %v", err)
	}
	body := requestBody{
		Contents:         plan.initialContents,
		GenerationConfig: plan.generationConfig,
	}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	if !bytes.Contains(wire, []byte(`"thinkingLevel":"low"`)) {
		t.Fatalf("wire payload must carry \"thinkingLevel\":\"low\" (camelCase key, lowercase value), got %s", wire)
	}
}

// TestLive_ThinkingLevelCasing verifies the casing claim against the REAL
// generateContent API: lowercase "low" is accepted, the uppercase variant
// is rejected. It SKIPS unless GEMINI_LIVE=1 is set and an API key is
// available (GEMINI_API_KEY, then GOOGLE_API_KEY). It consumes a tiny
// amount of quota, so it is never part of the default `go test` run. Run
// manually:
//
//	GEMINI_LIVE=1 GEMINI_API_KEY=... GOWORK=off go test -run TestLive_ThinkingLevelCasing ./provider/harness/gemini/ -v
//
// Override the probed model with GEMINI_LIVE_MODEL (default
// gemini-3.5-flash; must be a 3.x model — 2.5 uses thinkingBudget).
func TestLive_ThinkingLevelCasing(t *testing.T) {
	if os.Getenv("GEMINI_LIVE") == "" {
		t.Skip("set GEMINI_LIVE=1 to probe the real generateContent API (consumes quota)")
	}
	apiKey := os.Getenv(EnvAPIKeyPrimary)
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}
	if apiKey == "" {
		t.Skip("no GEMINI_API_KEY / GOOGLE_API_KEY in env")
	}
	model := os.Getenv("GEMINI_LIVE_MODEL")
	if model == "" {
		model = "gemini-3.5-flash"
	}
	if !is3xModel(model) {
		t.Fatalf("GEMINI_LIVE_MODEL %q is not a 3.x model; thinkingLevel only applies to the 3.x family", model)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	post := func(level string) (int, string) {
		t.Helper()
		body := requestBody{
			Contents: []requestContent{{
				Role:  "user",
				Parts: []requestPart{{Text: "Reply with the single word OK"}},
			}},
			GenerationConfig: &requestGenerationConfig{
				MaxOutputTokens: 50,
				ThinkingConfig:  &thinkingConfig{ThinkingLevel: level},
			},
		}
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", DefaultEndpoint, model)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload)) //nolint:gosec // G704: gated live test; host is the constant DefaultEndpoint, model comes from operator env
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", apiKey)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: same gated live probe
		if err != nil {
			t.Fatalf("POST generateContent: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(respBody)
	}

	// Lowercase (our wire value) must be accepted.
	if status, body := post("low"); status != http.StatusOK {
		t.Errorf("thinkingLevel \"low\": want 200, got %d: %s", status, body)
	}

	// The uppercase variant must be rejected — the claim behind the
	// "do NOT change to uppercase" note in tools.go. If this starts
	// passing, the API became case-insensitive and the note is stale.
	if status, body := post("LOW"); status != http.StatusBadRequest {
		t.Errorf("thinkingLevel \"LOW\": want 400 (API rejects uppercase), got %d: %s", status, body)
	}
}
