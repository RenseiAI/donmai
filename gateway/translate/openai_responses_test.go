package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

func TestDecodeResponsesRequest_StringInput(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("model = %q, want m", req.Model)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
		t.Fatalf("messages = %+v, want one user message", req.Messages)
	}
	if len(req.Messages[0].Parts) != 1 || req.Messages[0].Parts[0].Text != "hello" {
		t.Errorf("parts = %+v, want the input text", req.Messages[0].Parts)
	}
}

func TestDecodeResponsesRequest_FullShape(t *testing.T) {
	body := `{
		"model": "m",
		"instructions": "be brief",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"developer","content":"sys"},
			{"type":"function_call","call_id":"call-1","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"sunny"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}
		],
		"tools": [{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}}],
		"tool_choice": "auto",
		"reasoning": {"effort":"high"},
		"temperature": 0.5,
		"max_output_tokens": 100
	}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.System) != 2 {
		t.Fatalf("system parts = %+v, want instructions + developer message", req.System)
	}
	if req.System[0].Text != "be brief" || req.System[1].Text != "sys" {
		t.Errorf("system = %+v", req.System)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %+v, want user + replayed call + tool result + reasoning", req.Messages)
	}
	if req.Messages[0].Role != ir.RoleUser {
		t.Errorf("first message role = %q, want user", req.Messages[0].Role)
	}
	replayed := req.Messages[1].Parts[0]
	if replayed.Kind != ir.PartToolCall || replayed.ToolCall.Name != "get_weather" || replayed.ToolCall.ID != "call-1" {
		t.Errorf("replayed call = %+v, want the tool_call part", replayed)
	}
	result := req.Messages[2].Parts[0]
	if result.Kind != ir.PartToolResult || result.Text != "sunny" || result.ToolCallID != "call-1" {
		t.Errorf("tool result = %+v, want linked result", result)
	}
	thinking := req.Messages[3].Parts[0]
	if thinking.Kind != ir.PartThinking || thinking.Text != "thought" {
		t.Errorf("reasoning = %+v, want thinking part", thinking)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v", req.Tools)
	}
	if req.ToolChoice != ir.ToolChoiceAuto {
		t.Errorf("tool_choice = %q, want auto", req.ToolChoice)
	}
	if req.Thinking.Level != ir.ThinkingHigh {
		t.Errorf("thinking level = %q, want high", req.Thinking.Level)
	}
	if req.Sampling.Temperature == nil || *req.Sampling.Temperature != 0.5 {
		t.Errorf("temperature = %+v", req.Sampling.Temperature)
	}
	if req.Sampling.MaxTokens == nil || *req.Sampling.MaxTokens != 100 {
		t.Errorf("max tokens = %+v", req.Sampling.MaxTokens)
	}
}

func TestDecodeResponsesRequest_Refusals(t *testing.T) {
	cases := map[string]string{
		"previous_response_id": `{"model":"m","input":"x","previous_response_id":"resp-1"}`,
		"background":           `{"model":"m","input":"x","background":true}`,
		"logprobs":             `{"model":"m","input":"x","logprobs":true}`,
		"top_logprobs":         `{"model":"m","input":"x","top_logprobs":3}`,
		"include":              `{"model":"m","input":"x","include":["reasoning.encrypted_content"]}`,
		"serial_tools":         `{"model":"m","input":"x","parallel_tool_calls":false}`,
		"audio_modality":       `{"model":"m","input":"x","modalities":["text","audio"]}`,
		"non_function_tool":    `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`,
		"unknown_item":         `{"model":"m","input":[{"type":"compaction"}]}`,
		"item_reference":       `{"model":"m","input":[{"type":"item_reference","id":"x"}]}`,
		"unknown_part":         `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_audio","x":1}]}]}`,
		"malformed":            `{not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeResponsesRequest([]byte(body)); err == nil {
				t.Fatalf("DecodeResponsesRequest(%s) = nil error, want a loud refusal", name)
			}
		})
	}
}

func TestDecodeResponsesRequest_ParallelToolCallsTrueAllowed(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"x","parallel_tool_calls":true}`))
	if err != nil {
		t.Fatalf("explicit parallel_tool_calls=true should be accepted: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Errorf("messages = %+v", req.Messages)
	}
}

func TestEncodeResponsesResponse_TextToolUsage(t *testing.T) {
	ids := ResponsesIDs{Response: "resp-1", CreatedAt: 1700000000}
	resp := ir.Response{
		Model:        "m",
		FinishReason: ir.FinishToolCalls,
		Usage:        ir.Usage{TokensIn: 10, TokensOut: 5, ReasoningTokens: 2, CacheReadTokens: 3},
		Content: []ir.Part{
			{Kind: ir.PartText, Text: "hi"},
			{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "call-9", Name: "f", Arguments: `{"a":1}`}},
			{Kind: ir.PartThinking, Text: "hmm"},
		},
	}
	obj := EncodeResponsesResponse(resp, ids)
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if wire["id"] != "resp-1" || wire["object"] != "response" || wire["status"] != "completed" {
		t.Errorf("envelope = %v", wire)
	}
	output, _ := wire["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output has %d items, want 3 (reasoning, message, function_call): %v", len(output), wire)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("first item should be reasoning, got %v", output[0])
	}
	msg := output[1].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("second item = %v, want the assistant message", msg)
	}
	call := output[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call-9" || call["name"] != "f" {
		t.Errorf("third item = %v, want the function call", call)
	}
	usage := wire["usage"].(map[string]any)
	if usage["input_tokens"] != 10.0 || usage["output_tokens"] != 5.0 || usage["total_tokens"] != 15.0 {
		t.Errorf("usage = %v", usage)
	}
	outDet := usage["output_tokens_details"].(map[string]any)
	if outDet["reasoning_tokens"] != 2.0 {
		t.Errorf("reasoning tokens = %v", usage)
	}
	inDet := usage["input_tokens_details"].(map[string]any)
	if inDet["cached_tokens"] != 3.0 {
		t.Errorf("cached tokens = %v", usage)
	}
}

func TestEncodeResponsesResponse_IncompleteLength(t *testing.T) {
	obj := EncodeResponsesResponse(ir.Response{Model: "m", FinishReason: ir.FinishLength}, ResponsesIDs{Response: "r"})
	if obj.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", obj.Status)
	}
	if obj.IncompleteDetails == nil || obj.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details = %+v", obj.IncompleteDetails)
	}
	if len(obj.Output) != 0 {
		t.Errorf("empty content should encode an empty output array, got %+v", obj.Output)
	}
}

func TestEncodeResponsesResponse_ErrorIsFailed(t *testing.T) {
	obj := EncodeResponsesResponse(ir.Response{Model: "m", FinishReason: ir.FinishError}, ResponsesIDs{Response: "r"})
	if obj.Status != "failed" {
		t.Errorf("status = %q, want failed", obj.Status)
	}
}

func TestResponsesTerminal_MatchesStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"completed", "response.completed"},
		{"incomplete", "response.incomplete"},
		{"failed", "response.failed"},
	} {
		ev := ResponsesTerminal(ResponsesObject{ID: "r", Status: tc.status})
		if ev.Type != tc.want {
			t.Errorf("status %q -> event %q, want %q", tc.status, ev.Type, tc.want)
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"type":"`+tc.want+`"`) {
			t.Errorf("wire event missing type: %s", raw)
		}
	}
}

func TestResponsesStreamEventShapes(t *testing.T) {
	ids := ResponsesIDs{Response: "resp-7", CreatedAt: 1}
	created := ResponsesCreated(ids, "m")
	raw, _ := json.Marshal(created)
	if !strings.Contains(string(raw), `"status":"in_progress"`) {
		t.Errorf("created event = %s", raw)
	}
	added := ResponsesFunctionCallAdded(1, "fc-1", "get_weather")
	raw, _ = json.Marshal(added)
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if wire["type"] != "response.output_item.added" || wire["output_index"] != 1.0 {
		t.Errorf("added event = %v", wire)
	}
	item := wire["item"].(map[string]any)
	if item["call_id"] != "fc-1" || item["name"] != "get_weather" {
		t.Errorf("added item = %v", item)
	}
	delta := ResponsesTextDelta(0, "msg-1", "Hel")
	raw, _ = json.Marshal(delta)
	if !strings.Contains(string(raw), `"delta":"Hel"`) || !strings.Contains(string(raw), `"item_id":"msg-1"`) {
		t.Errorf("delta event = %s", raw)
	}
	done := ResponsesFunctionCallDone(1, "fc-1", `{"a":1}`)
	raw, _ = json.Marshal(done)
	if !strings.Contains(string(raw), `"arguments":"{\"a\":1}"`) {
		t.Errorf("done event = %s", raw)
	}
	// Done events must not carry a delta key, and deltas must not carry text/arguments.
	textDone := ResponsesTextDone(0, "msg-1", "hi")
	raw, _ = json.Marshal(textDone)
	if strings.Contains(string(raw), "delta") || !strings.Contains(string(raw), `"text":"hi"`) {
		t.Errorf("text done event = %s", raw)
	}
}
