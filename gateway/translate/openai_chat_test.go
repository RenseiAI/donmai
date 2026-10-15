package translate

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

func TestDecodeEncodeRequest_RoundTrip(t *testing.T) {
	body := `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":"auto",
		"temperature":0.5,
		"reasoning_effort":"medium",
		"stream":true
	}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "gpt-4o" || !req.Stream {
		t.Fatalf("basic fields: %+v", req)
	}
	if len(req.System) != 1 || req.System[0].Text != "be terse" {
		t.Errorf("system block not decoded: %+v", req.System)
	}
	if req.Thinking.Level != ir.ThinkingMedium {
		t.Errorf("reasoning_effort → thinking level = %q, want medium", req.Thinking.Level)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Errorf("tool not decoded: %+v", req.Tools)
	}
	// Assistant tool call + tool result should survive.
	var sawToolCall, sawToolResult bool
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Kind == ir.PartToolCall && p.ToolCall.Name == "lookup" {
				sawToolCall = true
			}
			if p.Kind == ir.PartToolResult && p.ToolCallID == "c1" {
				sawToolResult = true
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Errorf("tool-call loop lost: call=%v result=%v", sawToolCall, sawToolResult)
	}

	// Encode back to the wire and confirm the shape is faithful.
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var cr ChatRequest
	if err := json.Unmarshal(out, &cr); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if cr.Model != "gpt-4o" || cr.ReasoningEffort != "medium" {
		t.Errorf("encoded request lost fields: %+v", cr)
	}
	if cr.StreamOptions == nil || !cr.StreamOptions.IncludeUsage {
		t.Error("streaming request should request include_usage")
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Function.Name != "lookup" {
		t.Errorf("encoded tools lost: %+v", cr.Tools)
	}
}

func TestDecodeEncodeResponse_RoundTrip(t *testing.T) {
	up := `{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}}`
	resp, err := DecodeResponse([]byte(up))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FinishReason != ir.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Usage.ReasoningTokens != 2 {
		t.Errorf("reasoning tokens = %d, want 2", resp.Usage.ReasoningTokens)
	}
	var sawText, sawTool bool
	for _, p := range resp.Content {
		if p.Kind == ir.PartText && p.Text == "hello" {
			sawText = true
		}
		if p.Kind == ir.PartToolCall && p.ToolCall.Name == "f" {
			sawTool = true
		}
	}
	if !sawText || !sawTool {
		t.Errorf("response content lost: text=%v tool=%v", sawText, sawTool)
	}

	out, err := EncodeResponse(resp, "id-1", 123)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var cr ChatResponse
	if err := json.Unmarshal(out, &cr); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if cr.Object != "chat.completion" || cr.ID != "id-1" {
		t.Errorf("envelope wrong: %+v", cr)
	}
	if cr.Choices[0].FinishReason == nil || *cr.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("encoded finish reason wrong")
	}
	if cr.Usage == nil || cr.Usage.CompletionDetail == nil || cr.Usage.CompletionDetail.ReasoningTokens != 2 {
		t.Errorf("encoded usage lost reasoning tokens: %+v", cr.Usage)
	}
}

func TestStreamChunk_RoundTrip(t *testing.T) {
	chunk := `{"choices":[{"index":0,"delta":{"content":"Hi","tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`
	delta, done, err := DecodeStreamChunk([]byte(chunk))
	if err != nil || done {
		t.Fatalf("decode chunk: err=%v done=%v", err, done)
	}
	if delta.TextDelta != "Hi" {
		t.Errorf("text delta = %q, want Hi", delta.TextDelta)
	}
	if delta.ToolCallDelta == nil || delta.ToolCallDelta.Name != "f" {
		t.Errorf("tool-call delta lost: %+v", delta.ToolCallDelta)
	}

	out, err := EncodeStreamChunk(delta, "id", "m", 1)
	if err != nil {
		t.Fatalf("encode chunk: %v", err)
	}
	var cr ChatResponse
	if err := json.Unmarshal(out, &cr); err != nil {
		t.Fatalf("re-decode chunk: %v", err)
	}
	if cr.Object != "chat.completion.chunk" {
		t.Errorf("chunk object = %q", cr.Object)
	}
	if cr.Choices[0].Delta == nil || cr.Choices[0].Delta.Content != "Hi" {
		t.Errorf("encoded chunk delta lost")
	}
}

func TestStreamChunk_DoneSentinel(t *testing.T) {
	_, done, err := DecodeStreamChunk([]byte("[DONE]"))
	if err != nil || !done {
		t.Fatalf("[DONE] should decode as done: done=%v err=%v", done, err)
	}
}

func TestFinishReason_Mapping(t *testing.T) {
	cases := map[string]ir.FinishReason{
		"stop": ir.FinishStop, "end_turn": ir.FinishStop,
		"tool_calls": ir.FinishToolCalls, "length": ir.FinishLength,
		"content_filter": ir.FinishContentFilter, "unknown": ir.FinishStop,
	}
	for in, want := range cases {
		if got := finishToIR(in); got != want {
			t.Errorf("finishToIR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentArrayFlattened(t *testing.T) {
	// OpenAI multimodal content arrays flatten to text on decode.
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"part-a"},{"type":"text","text":"part-b"}]}]}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 || req.Messages[0].Parts[0].Text != "part-apart-b" {
		t.Errorf("content array not flattened: %+v", req.Messages)
	}
}
