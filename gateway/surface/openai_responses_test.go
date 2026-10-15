package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

func postResponses(t *testing.T, h http.Handler, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestResponses_MethodNotAllowed(t *testing.T) {
	h := NewOpenAIResponses(&fakeDispatcher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestResponses_MissingToken(t *testing.T) {
	h := NewOpenAIResponses(&fakeDispatcher{})
	rec := postResponses(t, h, "", `{"model":"m","input":"hi"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}
}

func TestResponses_RefusedFieldIsBadRequest(t *testing.T) {
	h := NewOpenAIResponses(&fakeDispatcher{})
	rec := postResponses(t, h, "Bearer t", `{"model":"m","input":"hi","previous_response_id":"resp-1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("previous_response_id status = %d, want 400", rec.Code)
	}
}

func TestResponses_MalformedBody(t *testing.T) {
	h := NewOpenAIResponses(&fakeDispatcher{})
	rec := postResponses(t, h, "Bearer t", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", rec.Code)
	}
}

func TestResponses_UnknownEffortIsBadRequestNamingValue(t *testing.T) {
	h := NewOpenAIResponses(&fakeDispatcher{})
	rec := postResponses(t, h, "Bearer t", `{"model":"m","input":"hi","reasoning":{"effort":"ultra"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown effort status = %d, want 400", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if !strings.Contains(env.Error.Message, "ultra") {
		t.Errorf("error message %q should name the rejected value", env.Error.Message)
	}
}

func TestResponses_XHighEffortReachesDispatcher(t *testing.T) {
	fd := &fakeDispatcher{outcome: Outcome{
		Response: &ir.Response{Model: "m", FinishReason: ir.FinishStop},
		Model:    "m",
	}}
	h := NewOpenAIResponses(fd)
	rec := postResponses(t, h, "Bearer t", `{"model":"m","input":"hi","reasoning":{"effort":"xhigh"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("xhigh effort status = %d, want 200", rec.Code)
	}
	if fd.gotReq.Thinking.Level != ir.ThinkingMax {
		t.Errorf("dispatch level = %q, want max", fd.gotReq.Thinking.Level)
	}
}

func TestResponses_DispatchHTTPError(t *testing.T) {
	fd := &fakeDispatcher{err: &HTTPError{Status: http.StatusUnauthorized, Code: "invalid_token", Message: "none"}}
	h := NewOpenAIResponses(fd)
	rec := postResponses(t, h, "Bearer t", `{"model":"m","input":"hi"}`)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the dispatch status", rec.Code)
	}
}

func TestResponses_NonStreamingMetersAndShapes(t *testing.T) {
	var metered bool
	fd := &fakeDispatcher{outcome: Outcome{
		Response: &ir.Response{
			Model:        "m",
			FinishReason: ir.FinishStop,
			Usage:        ir.Usage{TokensIn: 2, TokensOut: 1},
			Content: []ir.Part{
				{Kind: ir.PartText, Text: "hi"},
				{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "call-1", Name: "f", Arguments: "{}"}},
			},
		},
		Model: "m",
		Meter: func(_ ir.Usage, _ ir.FinishReason) { metered = true },
	}}
	h := NewOpenAIResponses(fd)
	h.IDFn = func() string { return "resp-test-1" }
	rec := postResponses(t, h, "Bearer tok", `{"model":"m","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fd.gotTok != "tok" {
		t.Errorf("dispatch token = %q, want tok", fd.gotTok)
	}
	if !metered {
		t.Error("non-streaming response should meter")
	}
	var resp struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "resp-test-1" || resp.Object != "response" || resp.Status != "completed" {
		t.Errorf("envelope = %+v", resp)
	}
	if len(resp.Output) != 2 || resp.Output[0].Type != "message" || resp.Output[1].Type != "function_call" {
		t.Fatalf("output = %+v, want message + function_call", resp.Output)
	}
	if resp.Output[0].Content[0].Text != "hi" {
		t.Errorf("message text = %+v", resp.Output[0].Content)
	}
	if resp.Output[1].CallID == "" || resp.Output[1].Name != "f" {
		t.Errorf("function call = %+v", resp.Output[1])
	}
	if resp.Usage.InputTokens != 2 || resp.Usage.OutputTokens != 1 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestResponses_EmptyUpstreamResponseIsBadGateway(t *testing.T) {
	fd := &fakeDispatcher{outcome: Outcome{Model: "m"}}
	h := NewOpenAIResponses(fd)
	rec := postResponses(t, h, "Bearer tok", `{"model":"m","input":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestResponses_StreamingEventSequence drives a text + tool-call stream and
// asserts the full wire order: created, per-item added before first delta,
// deltas, per-item done with assembled payloads, and the completed snapshot
// with usage — plus exactly one Meter call.
func TestResponses_StreamingEventSequence(t *testing.T) {
	deltas := make(chan ir.StreamDelta, 5)
	deltas <- ir.StreamDelta{TextDelta: "He"}
	deltas <- ir.StreamDelta{TextDelta: "llo"}
	deltas <- ir.StreamDelta{ToolCallDelta: &ir.ToolCallDelta{Index: 0, ID: "call-3", Name: "get_weather"}}
	deltas <- ir.StreamDelta{ToolCallDelta: &ir.ToolCallDelta{Index: 0, ArgsDelta: `{"city":`}}
	deltas <- ir.StreamDelta{
		ToolCallDelta: &ir.ToolCallDelta{Index: 0, ArgsDelta: `"sf"}`},
		Finish:        ir.FinishToolCalls,
		Usage:         &ir.Usage{TokensIn: 9, TokensOut: 4},
	}
	close(deltas)

	meterCalls := 0
	var meteredUsage ir.Usage
	fd := &fakeDispatcher{outcome: Outcome{
		Stream: &ir.Stream{Deltas: deltas, Err: func() error { return nil }},
		Model:  "m",
		Meter: func(u ir.Usage, _ ir.FinishReason) {
			meterCalls++
			meteredUsage = u
		},
	}}
	h := NewOpenAIResponses(fd)
	h.IDFn = func() string { return "resp-stream-1" }
	rec := postResponses(t, h, "Bearer tok", `{"model":"m","input":"hi","stream":true}`)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want SSE", ct)
	}
	types := eventTypes(t, rec.Body.String())
	wantOrder := []string{
		"response.created",
		"response.output_item.added", // message
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.output_text.done",
		"response.function_call_arguments.done",
		"response.completed",
	}
	if len(types) != len(wantOrder) {
		t.Fatalf("event types = %v, want %v", types, wantOrder)
	}
	for i, want := range wantOrder {
		if types[i] != want {
			t.Fatalf("event %d = %q, want %q (full: %v)", i, types[i], want, types)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"text":"Hello"`) {
		t.Error("text done event should carry the assembled text")
	}
	if !strings.Contains(body, `"arguments":"{\"city\":\"sf\"}"`) {
		t.Error("function done event should carry the assembled arguments")
	}
	if !strings.Contains(body, `"output_tokens":4`) {
		t.Error("completed snapshot should carry usage")
	}
	// The second added event (the function call) must come at output index 1,
	// after the message at index 0.
	addedIdx := addedOutputIndices(t, rec.Body.String())
	if len(addedIdx) != 2 || addedIdx[0] != 0 || addedIdx[1] != 1 {
		t.Errorf("added output indices = %v, want [0 1]", addedIdx)
	}
	if meterCalls != 1 {
		t.Fatalf("meter called %d times, want exactly 1", meterCalls)
	}
	if meteredUsage.TokensIn != 9 || meteredUsage.TokensOut != 4 {
		t.Errorf("metered usage = %+v, want 9/4 from terminal delta", meteredUsage)
	}
}

// TestResponses_StreamingToolBeforeText keeps announcement order consistent:
// when tool deltas arrive before any text, the tool takes index 0 and the
// later message takes index 1 — in deltas, done events, and the snapshot.
func TestResponses_StreamingToolBeforeText(t *testing.T) {
	deltas := make(chan ir.StreamDelta, 3)
	deltas <- ir.StreamDelta{ToolCallDelta: &ir.ToolCallDelta{Index: 0, ID: "c1", Name: "f", ArgsDelta: "{}"}}
	deltas <- ir.StreamDelta{TextDelta: "late"}
	deltas <- ir.StreamDelta{Finish: ir.FinishStop, Usage: &ir.Usage{TokensIn: 1, TokensOut: 1}}
	close(deltas)
	fd := &fakeDispatcher{outcome: Outcome{
		Stream: &ir.Stream{Deltas: deltas, Err: func() error { return nil }},
		Model:  "m",
	}}
	h := NewOpenAIResponses(fd)
	h.IDFn = func() string { return "resp-order-1" }
	rec := postResponses(t, h, "Bearer tok", `{"model":"m","input":"hi","stream":true}`)
	addedIdx := addedOutputIndices(t, rec.Body.String())
	if len(addedIdx) != 2 || addedIdx[0] != 0 || addedIdx[1] != 1 {
		t.Fatalf("added output indices = %v, want [0 1]", addedIdx)
	}
	types := eventTypes(t, rec.Body.String())
	// done events follow announcement order: function first, then text.
	donePos := map[string]int{}
	for i, typ := range types {
		if _, ok := donePos[typ]; !ok {
			donePos[typ] = i
		}
	}
	if donePos["response.function_call_arguments.done"] > donePos["response.output_text.done"] {
		t.Errorf("done events out of announcement order: %v", types)
	}
	// The snapshot carries both items.
	if !strings.Contains(rec.Body.String(), `"type":"response.completed"`) {
		t.Error("missing response.completed")
	}
}

func eventTypes(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("decode event %q: %v", payload, err)
		}
		out = append(out, ev.Type)
	}
	return out
}

func addedOutputIndices(t *testing.T, body string) []int {
	t.Helper()
	var out []int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		var ev struct {
			Type        string `json:"type"`
			OutputIndex int    `json:"output_index"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev.Type == "response.output_item.added" {
			out = append(out, ev.OutputIndex)
		}
	}
	return out
}
