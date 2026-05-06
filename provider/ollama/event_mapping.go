package ollama

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// chatChunk is the wire shape of one NDJSON line from POST /api/chat.
// Each chunk has either an incremental message (Done=false) or the
// terminal usage roll-up (Done=true). The provider maps incremental
// chunks to AssistantTextEvent and the terminal chunk to ResultEvent.
//
// Reference: https://github.com/ollama/ollama/blob/main/docs/api.md#response
type chatChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason,omitempty"`

	// Counters present only on the terminal chunk.
	TotalDuration      int64 `json:"total_duration,omitempty"`
	LoadDuration       int64 `json:"load_duration,omitempty"`
	PromptEvalCount    int64 `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount          int64 `json:"eval_count,omitempty"`
	EvalDuration       int64 `json:"eval_duration,omitempty"`

	// Server-emitted error, if the model load or generation failed
	// mid-stream. Ollama uses both `{"error":...}` and HTTP 4xx; this
	// field captures the inline form.
	Error string `json:"error,omitempty"`
}

// mapLine decodes one NDJSON line into one or more agent.Event values.
// Returns nil + a non-nil error when the line is not valid JSON; the
// caller decides whether to surface the error on the events channel or
// drop the malformed line.
//
// Mapping rules:
//
//   - Lines with non-empty Error → ErrorEvent.
//   - Lines with Done=false      → AssistantTextEvent (the incremental
//     content chunk).
//   - Lines with Done=true       → ResultEvent (Success=true unless
//     DoneReason or Error indicates otherwise) plus a CostData carrying
//     prompt_eval_count / eval_count translated into InputTokens /
//     OutputTokens. Ollama is local — TotalCostUsd is always 0.
func mapLine(line []byte) ([]agent.Event, error) {
	if len(line) == 0 {
		return nil, nil
	}
	var c chatChunk
	if err := json.Unmarshal(line, &c); err != nil {
		return nil, fmt.Errorf("decode ollama chunk: %w", err)
	}
	if c.Error != "" {
		return []agent.Event{agent.ErrorEvent{
			Message: c.Error,
			Code:    "ollama_stream_error",
			Raw:     c,
		}}, nil
	}
	if !c.Done {
		// Incremental content. Ollama emits empty content chunks at the
		// very start of generation for some models — drop those so the
		// runner's text accumulator does not see noise.
		if c.Message.Content == "" {
			return nil, nil
		}
		return []agent.Event{agent.AssistantTextEvent{
			Text: c.Message.Content,
			Raw:  c,
		}}, nil
	}
	// Terminal chunk. The success flag is true unless DoneReason
	// indicates a non-stop termination ("length", "load_failed", etc.);
	// Ollama's stop-on-EOS reason is "stop".
	success := c.DoneReason == "" || c.DoneReason == "stop"
	cost := &agent.CostData{
		InputTokens:  c.PromptEvalCount,
		OutputTokens: c.EvalCount,
		// Ollama is local — no dollar cost. Leave TotalCostUsd zero.
		NumTurns: 1,
	}
	res := agent.ResultEvent{
		Success: success,
		Message: c.DoneReason,
		Cost:    cost,
		Raw:     c,
	}
	return []agent.Event{res}, nil
}
