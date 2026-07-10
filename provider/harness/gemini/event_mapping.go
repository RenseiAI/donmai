package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// generateContentResponse mirrors the JSON object returned by the
// non-streaming generateContent endpoint. Only the fields the runner
// consumes are decoded; the full response is preserved as Raw on each
// emitted event.
type generateContentResponse struct {
	Candidates     []candidate     `json:"candidates"`
	UsageMetadata  *usageMetadata  `json:"usageMetadata,omitempty"`
	PromptFeedback *promptFeedback `json:"promptFeedback,omitempty"`
}

type candidate struct {
	Content      *candidateContent `json:"content,omitempty"`
	FinishReason string            `json:"finishReason,omitempty"`
}

type candidateContent struct {
	Parts []candidatePart `json:"parts,omitempty"`
	Role  string          `json:"role,omitempty"`
}

// candidatePart decodes a model-turn part: either text or a functionCall.
type candidatePart struct {
	Text         string             `json:"text,omitempty"`
	FunctionCall *candidateFuncCall `json:"functionCall,omitempty"`
}

// candidateFuncCall mirrors the functionCall part on a model turn.
// Gemini-3 always populates ID; older models may omit it.
type candidateFuncCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// turnState carries the running cost totals across turns so the terminal
// ResultEvent reflects the whole conversation, not just the last turn.
type turnState struct {
	model             string
	totalInputTokens  int64
	totalOutputTokens int64
	totalCachedTokens int64
	turnCount         int
}

// turnOutcome is the classification of a single decoded generateContent
// response. The Handle's driver loop uses it to decide whether to
// continue (function calls await results), finish (terminal), or fail.
type turnOutcome int

const (
	// outcomeContinue means the model emitted one or more functionCalls;
	// the loop pauses until the caller supplies the tool results.
	outcomeContinue turnOutcome = iota
	// outcomeFinal means the model finished its turn (text only, no
	// pending function calls); the loop emits a terminal ResultEvent.
	outcomeFinal
	// outcomeError means the response was malformed or blocked; the loop
	// emits a terminal ErrorEvent.
	outcomeError
)

// decodedTurn is the result of mapping one generateContent response.
type decodedTurn struct {
	// events are the AssistantText / ToolUse events to emit in order.
	events []agent.Event
	// funcCalls are the model's function calls (subset of events that
	// also need a functionResponse before the conversation continues).
	funcCalls []candidateFuncCall
	// modelParts are the model-turn parts to append to the contents
	// history so the next turn carries this turn's calls/text.
	modelParts []requestPart
	// outcome classifies the turn.
	outcome turnOutcome
	// result is the terminal ResultEvent / ErrorEvent when outcome is
	// final/error. nil otherwise.
	result agent.Event
	// finishReason is the candidate finishReason (diagnostics).
	finishReason string
}

// mapResponse decodes one generateContent JSON response into a
// decodedTurn. It accumulates token usage into state and computes
// TotalCostUsd from the per-model pricing table on the terminal turn.
func mapResponse(body []byte, state *turnState) decodedTurn {
	var resp generateContentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return decodedTurn{
			events: []agent.Event{agent.LlmCallEvent{
				System:       "gcp.gemini",
				Model:        state.model,
				FinishReason: "decode_error",
				UsageSource:  agent.LlmUsageProvider,
			}},
			outcome: outcomeError,
			result: agent.ErrorEvent{
				Message: fmt.Sprintf("provider/gemini: decode response: %v", err),
				Code:    "decode_response",
				Raw:     string(body),
			},
		}
	}

	// Blocked prompts are terminal failures.
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
		return decodedTurn{
			events: []agent.Event{agent.LlmCallEvent{
				System:       "gcp.gemini",
				Model:        state.model,
				FinishReason: "prompt_blocked_" + resp.PromptFeedback.BlockReason,
				UsageSource:  agent.LlmUsageProvider,
			}},
			outcome: outcomeError,
			result: agent.ErrorEvent{
				Message: "gemini: prompt blocked: " + resp.PromptFeedback.BlockReason,
				Code:    "prompt_blocked",
				Raw:     resp,
			},
		}
	}

	state.turnCount++
	accumulateUsage(resp.UsageMetadata, state)

	turn := decodedTurn{}
	for _, c := range resp.Candidates {
		if c.FinishReason != "" {
			turn.finishReason = c.FinishReason
		}
		if c.Content == nil {
			continue
		}
		for _, part := range c.Content.Parts {
			switch {
			case part.FunctionCall != nil && part.FunctionCall.Name != "":
				fc := *part.FunctionCall
				turn.funcCalls = append(turn.funcCalls, fc)
				turn.modelParts = append(turn.modelParts, requestPart{
					FunctionCall: &functionCall{ID: fc.ID, Name: fc.Name, Args: fc.Args},
				})
				turn.events = append(turn.events, agent.ToolUseEvent{
					ToolName:  fc.Name,
					ToolUseID: fc.ID,
					Input:     fc.Args,
					Raw:       resp,
				})
			case part.Text != "":
				turn.modelParts = append(turn.modelParts, requestPart{Text: part.Text})
				turn.events = append(turn.events, agent.AssistantTextEvent{
					Text: part.Text,
					Raw:  resp,
				})
			}
		}
	}
	var inputTokens, outputTokens, cachedTokens int64
	if resp.UsageMetadata != nil {
		inputTokens = int64(resp.UsageMetadata.PromptTokenCount)
		outputTokens = int64(resp.UsageMetadata.CandidatesTokenCount)
		cachedTokens = int64(resp.UsageMetadata.CachedContentTokenCount)
	}
	llm := agent.LlmCallEvent{
		System:            "gcp.gemini",
		Model:             state.model,
		InputTokens:       inputTokens,
		OutputTokens:      outputTokens,
		CachedInputTokens: cachedTokens,
		FinishReason:      turn.finishReason,
		UsageSource:       agent.LlmUsageProvider,
	}
	turn.events = append([]agent.Event{llm}, turn.events...)

	// Function calls pending → continue once the caller supplies results.
	if len(turn.funcCalls) > 0 {
		turn.outcome = outcomeContinue
		return turn
	}

	// No function calls → terminal. Build the ResultEvent with cost.
	turn.outcome = outcomeFinal
	turn.result = buildResultEvent(turn.finishReason, state, resp)
	return turn
}

// accumulateUsage folds a turn's usageMetadata into the running totals.
func accumulateUsage(u *usageMetadata, state *turnState) {
	if u == nil {
		return
	}
	state.totalInputTokens += int64(u.PromptTokenCount)
	state.totalOutputTokens += int64(u.CandidatesTokenCount)
	state.totalCachedTokens += int64(u.CachedContentTokenCount)
}

// buildResultEvent constructs the terminal ResultEvent from the finish
// reason + accumulated cost.
//
// STOP (or empty finish reason on an intermediate tool-call turn) is a
// clean success. MAX_TOKENS is a truncation signal, NOT a success: the
// response was cut short mid-generation and the runner's acceptance gate
// must not treat it as a complete result. SAFETY / RECITATION / OTHER
// are explicit content-policy failures. Every other non-empty reason is
// also treated as failure to be conservative.
func buildResultEvent(finishReason string, state *turnState, raw any) agent.ResultEvent {
	success := finishReason == "" || finishReason == "STOP"
	ev := agent.ResultEvent{
		Success: success,
		Message: "finish_reason=" + finishReason,
		Cost:    buildCost(state),
		Raw:     raw,
	}
	if !success {
		ev.Errors = []string{finishReason}
		ev.ErrorSubtype = "finish_" + finishReason
	}
	return ev
}

// buildCost assembles the CostData with TotalCostUsd computed from the
// per-model pricing table.
func buildCost(state *turnState) *agent.CostData {
	return &agent.CostData{
		InputTokens:       state.totalInputTokens,
		OutputTokens:      state.totalOutputTokens,
		CachedInputTokens: state.totalCachedTokens,
		TotalCostUsd:      calculateCostUSD(state.totalInputTokens, state.totalOutputTokens, state.model),
		NumTurns:          state.turnCount,
	}
}
