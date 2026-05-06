package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// streamChunk mirrors the JSON shape emitted by
// streamGenerateContent?alt=sse on the data: lines. Only the fields
// the v0.1 runner consumes are decoded; the full chunk is preserved as
// Raw on each emitted event.
type streamChunk struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	// PromptFeedback flags a blocked prompt (safety, recitation).
	// Treated as a fatal error event when blockReason is non-empty.
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

type candidatePart struct {
	Text string `json:"text,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// mapChunk decodes one SSE data line and returns the agent.Events it
// produces. Returns:
//
//   - one or more AssistantTextEvents when the chunk carries text parts.
//   - a single ResultEvent when finishReason or usageMetadata signals
//     completion. The caller marks the stream terminal and stops
//     reading further chunks.
//   - a single ErrorEvent when promptFeedback.blockReason is set or
//     the JSON is malformed.
//
// The boolean second return is true iff the returned events include a
// terminal Result/Error; the reader uses it to suppress the synthetic
// "no terminal" diagnostic on EOF.
func mapChunk(line []byte) ([]agent.Event, bool) {
	var chunk streamChunk
	if err := json.Unmarshal(line, &chunk); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/gemini: decode chunk: %v", err),
			Code:    "decode_chunk",
			Raw:     string(line),
		}}, true
	}

	// Blocked prompts are terminal failures.
	if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
		return []agent.Event{agent.ErrorEvent{
			Message: "gemini: prompt blocked: " + chunk.PromptFeedback.BlockReason,
			Code:    "prompt_blocked",
			Raw:     chunk,
		}}, true
	}

	out := make([]agent.Event, 0, 2)
	for _, c := range chunk.Candidates {
		if c.Content == nil {
			continue
		}
		for _, part := range c.Content.Parts {
			if part.Text == "" {
				continue
			}
			out = append(out, agent.AssistantTextEvent{
				Text: part.Text,
				Raw:  chunk,
			})
		}
	}

	terminal := false
	for _, c := range chunk.Candidates {
		if c.FinishReason == "" {
			continue
		}
		terminal = true
		ev := agent.ResultEvent{
			Success: c.FinishReason == "STOP" || c.FinishReason == "MAX_TOKENS",
			Message: "finish_reason=" + c.FinishReason,
			Raw:     chunk,
		}
		if !ev.Success {
			ev.Errors = []string{c.FinishReason}
			ev.ErrorSubtype = "finish_" + c.FinishReason
		}
		if chunk.UsageMetadata != nil {
			ev.Cost = &agent.CostData{
				InputTokens:  int64(chunk.UsageMetadata.PromptTokenCount),
				OutputTokens: int64(chunk.UsageMetadata.CandidatesTokenCount),
			}
		}
		out = append(out, ev)
		break // one Result per stream
	}

	return out, terminal
}
