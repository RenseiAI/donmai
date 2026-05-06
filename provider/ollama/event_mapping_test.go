package ollama

import (
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestMapLine_incrementalContent(t *testing.T) {
	t.Parallel()
	line := []byte(`{"model":"llama3.3","message":{"role":"assistant","content":"Hello"},"done":false}`)
	evs, err := mapLine(line)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	at, ok := evs[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("expected AssistantTextEvent, got %T", evs[0])
	}
	if at.Text != "Hello" {
		t.Errorf("text: got %q want Hello", at.Text)
	}
}

func TestMapLine_terminalChunk(t *testing.T) {
	t.Parallel()
	line := []byte(`{"model":"llama3.3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":12,"eval_count":34}`)
	evs, err := mapLine(line)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	res, ok := evs[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("expected ResultEvent, got %T", evs[0])
	}
	if !res.Success {
		t.Errorf("success: got false want true")
	}
	if res.Cost == nil {
		t.Fatal("cost: nil")
	}
	if res.Cost.InputTokens != 12 || res.Cost.OutputTokens != 34 {
		t.Errorf("cost: got in=%d out=%d want in=12 out=34", res.Cost.InputTokens, res.Cost.OutputTokens)
	}
	if res.Cost.TotalCostUsd != 0 {
		t.Errorf("cost: TotalCostUsd should be 0 for local Ollama; got %v", res.Cost.TotalCostUsd)
	}
	if res.Cost.NumTurns != 1 {
		t.Errorf("cost: NumTurns: got %d want 1", res.Cost.NumTurns)
	}
}

func TestMapLine_terminalChunk_lengthIsFailure(t *testing.T) {
	t.Parallel()
	line := []byte(`{"done":true,"done_reason":"length"}`)
	evs, err := mapLine(line)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	res := evs[0].(agent.ResultEvent)
	if res.Success {
		t.Errorf("success: got true want false (done_reason=length)")
	}
}

func TestMapLine_errorChunk(t *testing.T) {
	t.Parallel()
	line := []byte(`{"error":"model llama3.3 not found"}`)
	evs, err := mapLine(line)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	er, ok := evs[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("expected ErrorEvent, got %T", evs[0])
	}
	if er.Code != "ollama_stream_error" {
		t.Errorf("code: got %q want ollama_stream_error", er.Code)
	}
	if er.Message != "model llama3.3 not found" {
		t.Errorf("message: got %q", er.Message)
	}
}

func TestMapLine_emptyContentDropped(t *testing.T) {
	t.Parallel()
	line := []byte(`{"message":{"role":"assistant","content":""},"done":false}`)
	evs, err := mapLine(line)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("expected 0 events for empty content, got %d", len(evs))
	}
}

func TestMapLine_invalidJSON(t *testing.T) {
	t.Parallel()
	_, err := mapLine([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestMapLine_emptyLine(t *testing.T) {
	t.Parallel()
	evs, err := mapLine(nil)
	if err != nil {
		t.Fatalf("mapLine: %v", err)
	}
	if evs != nil {
		t.Errorf("expected nil events for empty line, got %v", evs)
	}
}
