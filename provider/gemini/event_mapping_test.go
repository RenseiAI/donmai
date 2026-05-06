package gemini

import (
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

func TestMapChunk_TextOnly(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)
	got, terminal := mapChunk(in)
	if terminal {
		t.Error("terminal: want false on text-only chunk")
	}
	if len(got) != 1 {
		t.Fatalf("events: want 1, got %d", len(got))
	}
	ev, ok := got[0].(agent.AssistantTextEvent)
	if !ok {
		t.Fatalf("event[0]: want AssistantTextEvent, got %T", got[0])
	}
	if ev.Text != "hello" {
		t.Errorf("Text: want %q, got %q", "hello", ev.Text)
	}
}

func TestMapChunk_FinishReasonStop(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}`)
	got, terminal := mapChunk(in)
	if !terminal {
		t.Fatal("terminal: want true on finishReason chunk")
	}
	if len(got) != 1 {
		t.Fatalf("events: want 1, got %d", len(got))
	}
	res, ok := got[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event[0]: want ResultEvent, got %T", got[0])
	}
	if !res.Success {
		t.Error("ResultEvent.Success: want true for STOP")
	}
	if res.Cost == nil {
		t.Fatal("ResultEvent.Cost: want non-nil with usageMetadata")
	}
	if res.Cost.InputTokens != 10 {
		t.Errorf("InputTokens: want %d, got %d", 10, res.Cost.InputTokens)
	}
	if res.Cost.OutputTokens != 4 {
		t.Errorf("OutputTokens: want %d, got %d", 4, res.Cost.OutputTokens)
	}
}

func TestMapChunk_FinishReasonSafety(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"finishReason":"SAFETY"}]}`)
	got, terminal := mapChunk(in)
	if !terminal {
		t.Fatal("terminal: want true")
	}
	res, ok := got[0].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event[0]: want ResultEvent, got %T", got[0])
	}
	if res.Success {
		t.Error("ResultEvent.Success: want false for SAFETY")
	}
	if !strings.Contains(res.ErrorSubtype, "SAFETY") {
		t.Errorf("ErrorSubtype: want SAFETY mention, got %q", res.ErrorSubtype)
	}
}

func TestMapChunk_PromptBlocked(t *testing.T) {
	t.Parallel()
	in := []byte(`{"promptFeedback":{"blockReason":"OTHER"}}`)
	got, terminal := mapChunk(in)
	if !terminal {
		t.Fatal("terminal: want true on blockReason")
	}
	if len(got) != 1 {
		t.Fatalf("events: want 1, got %d", len(got))
	}
	errEv, ok := got[0].(agent.ErrorEvent)
	if !ok {
		t.Fatalf("event[0]: want ErrorEvent, got %T", got[0])
	}
	if errEv.Code != "prompt_blocked" {
		t.Errorf("Code: want %q, got %q", "prompt_blocked", errEv.Code)
	}
}

func TestMapChunk_MalformedJSONReturnsError(t *testing.T) {
	t.Parallel()
	in := []byte(`{not-json`)
	got, terminal := mapChunk(in)
	if !terminal {
		t.Fatal("terminal: want true for parse error")
	}
	if _, ok := got[0].(agent.ErrorEvent); !ok {
		t.Fatalf("event[0]: want ErrorEvent on bad JSON, got %T", got[0])
	}
}

func TestMapChunk_TextThenFinish(t *testing.T) {
	t.Parallel()
	in := []byte(`{"candidates":[{"content":{"parts":[{"text":"chunk"}]},"finishReason":"STOP"}]}`)
	got, terminal := mapChunk(in)
	if !terminal {
		t.Fatal("terminal: want true")
	}
	if len(got) != 2 {
		t.Fatalf("events: want 2 (text+result), got %d", len(got))
	}
	if _, ok := got[0].(agent.AssistantTextEvent); !ok {
		t.Errorf("event[0]: want AssistantTextEvent, got %T", got[0])
	}
	if _, ok := got[1].(agent.ResultEvent); !ok {
		t.Errorf("event[1]: want ResultEvent, got %T", got[1])
	}
}
