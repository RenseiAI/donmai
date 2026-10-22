package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

// TestReasoningEffort_AllTranslators covers every documented effort string
// across all three translator hops the gateway implements: the OpenAI
// inbound decoders (chat completions reasoning_effort and responses
// reasoning.effort), the Anthropic/Gemini budget-shaped outbound hop
// (BudgetForBudgetUpstream), and the OpenAI outbound hop (EffortForOpenAI,
// including the max-to-high clamp). Unknown values are refused with the
// value named on both inbound paths.
func TestReasoningEffort_AllTranslators(t *testing.T) {
	cases := []struct {
		effort       string
		level        ir.ThinkingLevel
		budget       int    // Anthropic/Gemini-shaped hop
		openAIEffort string // OpenAI-shaped hop ("" = field omitted)
	}{
		{"off", ir.ThinkingOff, 0, ""},
		{"none", ir.ThinkingOff, 0, ""},
		{"minimal", ir.ThinkingMinimal, 1024, "minimal"},
		{"low", ir.ThinkingLow, 4096, "low"},
		{"medium", ir.ThinkingMedium, 8192, "medium"},
		{"high", ir.ThinkingHigh, 16384, "high"},
		{"xhigh", ir.ThinkingMax, 32768, "high"}, // max clamps to high toward OpenAI
		{"max", ir.ThinkingMax, 32768, "high"},   // max clamps to high toward OpenAI
	}
	if len(cases) != len(ir.DocumentedEfforts()) {
		t.Fatalf("%d cases, want one per documented effort (%d)", len(cases), len(ir.DocumentedEfforts()))
	}
	seen := map[string]bool{}
	for _, effort := range ir.DocumentedEfforts() {
		seen[effort] = true
	}
	for _, tc := range cases {
		if !seen[tc.effort] {
			t.Fatalf("case %q is not a documented effort", tc.effort)
		}
		// OpenAI chat inbound.
		chatReq, err := DecodeRequest([]byte(`{"model":"m","messages":[],"reasoning_effort":"` + tc.effort + `"}`))
		if err != nil {
			t.Errorf("chat DecodeRequest(%q) error = %v", tc.effort, err)
			continue
		}
		if chatReq.Thinking.Level != tc.level {
			t.Errorf("chat DecodeRequest(%q) level = %q, want %q", tc.effort, chatReq.Thinking.Level, tc.level)
		}
		// OpenAI responses inbound.
		respReq, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","reasoning":{"effort":"` + tc.effort + `"}}`))
		if err != nil {
			t.Errorf("responses DecodeResponsesRequest(%q) error = %v", tc.effort, err)
			continue
		}
		if respReq.Thinking.Level != tc.level {
			t.Errorf("responses DecodeResponsesRequest(%q) level = %q, want %q", tc.effort, respReq.Thinking.Level, tc.level)
		}
		// Anthropic/Gemini-shaped outbound hop.
		if budget := chatReq.Thinking.BudgetForBudgetUpstream(); budget != tc.budget {
			t.Errorf("BudgetForBudgetUpstream(%q) = %d, want %d", tc.effort, budget, tc.budget)
		}
		// OpenAI-shaped outbound hop.
		if got := chatReq.Thinking.EffortForOpenAI(); got != tc.openAIEffort {
			t.Errorf("EffortForOpenAI(%q) = %q, want %q", tc.effort, got, tc.openAIEffort)
		}
		// The encoded chat wire carries exactly the OpenAI-shaped effort.
		out, err := EncodeRequest(chatReq)
		if err != nil {
			t.Errorf("EncodeRequest(%q) error = %v", tc.effort, err)
			continue
		}
		var cr ChatRequest
		if err := json.Unmarshal(out, &cr); err != nil {
			t.Errorf("re-decode(%q): %v", tc.effort, err)
			continue
		}
		if cr.ReasoningEffort != tc.openAIEffort {
			t.Errorf("EncodeRequest(%q) reasoning_effort = %q, want %q", tc.effort, cr.ReasoningEffort, tc.openAIEffort)
		}
	}
}

func TestReasoningEffort_BothInboundsRefuseUnknown(t *testing.T) {
	if _, err := DecodeRequest([]byte(`{"model":"m","messages":[],"reasoning_effort":"ultra"}`)); err == nil {
		t.Error("chat DecodeRequest(ultra) = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "ultra") {
		t.Errorf("chat error %q should name the rejected value", err.Error())
	}
	if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","reasoning":{"effort":"ultra"}}`)); err == nil {
		t.Error("responses DecodeResponsesRequest(ultra) = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "ultra") {
		t.Errorf("responses error %q should name the rejected value", err.Error())
	}
}
