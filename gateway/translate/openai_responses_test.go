package translate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

func TestResponsesRequestPinnedItemsRoundTrip(t *testing.T) {
	body := `{"model":"fixture-model","instructions":"work carefully","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},{"type":"message","id":"msg1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"working"}]},{"type":"function_call","id":"fc1","call_id":"call1","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"function_call_output","call_id":"call1","output":"result"},{"type":"custom_tool_call","id":"ctc1","status":"completed","call_id":"custom1","name":"shell","input":"echo ok"},{"type":"custom_tool_call_output","call_id":"custom1","output":"ok"},{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"summary"}],"content":[{"type":"reasoning_text","text":"thinking"}],"encrypted_content":"opaque=="}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"},"strict":true},{"type":"custom","name":"shell","format":{"type":"text"}}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"medium","summary":"auto"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"service_tier":"default","prompt_cache_key":"cache","client_metadata":{"key":"value"}}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Items) != 7 || req.Items[2].ID != "fc1" || req.Items[2].CallID != "call1" || req.Items[3].Output != "result" || req.Items[6].EncryptedContent != "opaque==" || req.Items[6].Parts[0].Kind != ir.PartThinking {
		t.Fatalf("item semantics lost: %+v", req.Items)
	}
	encoded, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	round, err := DecodeResponsesRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req, round) {
		t.Fatalf("roundtrip differs:\n%+v\n%+v", req, round)
	}
}

func TestResponsesRequestRefusesUnsupportedSemantics(t *testing.T) {
	cases := []string{
		`{"model":"m","input":[],"store":true}`,
		`{"model":"m","input":[],"previous_response_id":"resp_old"}`,
		`{"model":"m","input":[],"previous_response_id":null}`,
		`{"model":"m","input":[],"unknown":true}`,
		`{"model":"m","model":"other","input":[]}`,
		`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`,
		`{"model":"m","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}","input":"discarded"}]}`,
		`{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"structured"}]}]}`,
		`{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":null}]}`,
		`{"model":"m","input":[{"type":"item_reference","id":"state"}]}`,
		`{"model":"m","input":[],"tools":[{"type":"tool_search"}]}`,
		`{"model":"m","input":[],"tools":[{"type":"function","name":"f","format":{"type":"text"}}]}`,
		`{"model":"m","input":[],"include":["unknown"]}`,
		`{"model":"m","input":[],"reasoning":{"unknown":"value"}}`,
		`{"model":"m","input":[],"text":{"unknown":true}}`,
		`{"model":"m","input":[]} {}`,
		`{"model":"m","input":[],"unknown":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}`,
	}
	for i, body := range cases {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			if _, err := DecodeResponsesRequest([]byte(body)); err == nil {
				t.Fatalf("accepted unsupported request: %s", body)
			}
		})
	}
}

func TestResponsesChatProjectionIsLosslessOrRefused(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","instructions":"system","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"result"}],"store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeRequest(projected)
	if err != nil {
		t.Fatal(err)
	}
	var wire ChatRequest
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Messages) != 4 || wire.Messages[2].ToolCalls[0].ID != "call_1" || wire.Messages[3].ToolCallID != "call_1" || wire.Messages[3].Content != "result" {
		t.Fatalf("lost linkage: %s", encoded)
	}
	for _, mutate := range []func(*ir.Request){
		func(r *ir.Request) { r.Responses.Include = []string{"reasoning.encrypted_content"} },
		func(r *ir.Request) { r.Items[0].Phase = "commentary" },
		func(r *ir.Request) { r.Items[1].ID = "item_1" },
		func(r *ir.Request) { r.Items[1].Namespace = "namespace" },
		func(r *ir.Request) { r.Items[0].Kind = "reasoning" },
	} {
		copyReq := req
		copyReq.Items = append([]ir.Item{}, req.Items...)
		controls := *req.Responses
		copyReq.Responses = &controls
		mutate(&copyReq)
		if _, err := ProjectResponsesToChat(copyReq); err == nil {
			t.Fatal("accepted unrepresentable projection")
		}
	}
}

// TestResponsesChatProjectionParallelAndEffort pins the two controls the Chat
// codec carries: the parallel-tool default (absent or true) and a reasoning
// effort on the canonical ladder. Serial tool use and a requested reasoning
// summary have no Chat representation and are refused.
func TestResponsesChatProjectionParallelAndEffort(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"x"}`,
		`{"model":"m","input":"x","parallel_tool_calls":true}`,
	} {
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ProjectResponsesToChat(req); err != nil {
			t.Fatalf("Chat refused the parallel default %s: %v", body, err)
		}
	}
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"x","reasoning":{"effort":"xhigh"}}`))
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectResponsesToChat(req)
	if err != nil {
		t.Fatalf("Chat refused a reasoning effort: %v", err)
	}
	encoded, err := EncodeRequest(projected)
	if err != nil {
		t.Fatal(err)
	}
	var wire ChatRequest
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ReasoningEffort != "high" {
		t.Fatalf("xhigh effort reached Chat as %q, want the top OpenAI level", wire.ReasoningEffort)
	}
	// The effort is carried from the Responses control itself, never dropped
	// when a caller built the request without a canonical thinking spec.
	built := ir.Request{Model: "m", Items: req.Items, Responses: &ir.ResponsesControls{Reasoning: map[string]string{"effort": "low"}}}
	if projected, err := ProjectResponsesToChat(built); err != nil || projected.Thinking.Level != ir.ThinkingLow {
		t.Fatalf("effort projection = %+v, %v", projected.Thinking, err)
	}
	for _, body := range []string{
		`{"model":"m","input":"x","parallel_tool_calls":false}`,
		`{"model":"m","input":"x","reasoning":{"effort":"low","summary":"auto"}}`,
	} {
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ProjectResponsesToChat(req); err == nil {
			t.Fatalf("Chat silently represented %s", body)
		}
	}
}

func TestResponsesStreamEventWireShapes(t *testing.T) {
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"r","object":"response","status":"in_progress"}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"f","arguments":"","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"item_id":"fc","delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","sequence_number":3,"output_index":0,"item_id":"fc","arguments":"{}"}`,
		`{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc","call_id":"call","name":"f","arguments":"{}","status":"completed"}}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"ctc","delta":"echo ok"}`,
		`{"type":"response.custom_tool_call_input.done","item_id":"ctc","input":"echo ok"}`,
		`{"type":"response.reasoning_summary_part.added","summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","summary_index":0,"delta":"summary"}`,
		`{"type":"response.reasoning_text.delta","content_index":0,"delta":"thinking"}`,
		`{"type":"response.completed","sequence_number":5,"response":{"id":"r","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`,
	}
	for _, body := range events {
		ev, err := DecodeResponsesStreamEvent([]byte(body))
		if err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		wire, err := EncodeResponsesStreamEvent(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", body, err)
		}
		round, err := DecodeResponsesStreamEvent(wire)
		if err != nil {
			t.Fatalf("round %s: %v", wire, err)
		}
		if ev.Kind != round.Kind || ev.Delta != round.Delta || ev.ItemID != round.ItemID || !reflect.DeepEqual(ev.Item, round.Item) {
			t.Fatalf("event semantics changed: %+v -> %+v", ev, round)
		}
	}
}

func TestResponsesResponseEnvelopeAndRefusals(t *testing.T) {
	body := `{"id":"r","object":"response","created_at":10,"status":"completed","model":"m","background":false,"store":false,"error":null,"incomplete_details":null,"previous_response_id":null,"user":null,"instructions":"hello","parallel_tool_calls":true,"tool_choice":"auto","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":true}],"reasoning":{"effort":"medium"},"text":{"format":{"type":"text"}},"metadata":{"key":"value"},"service_tier":"default","temperature":1,"top_p":1,"max_output_tokens":100,"truncation":"disabled","output":[{"type":"message","id":"msg","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeResponsesResponse(resp, "different")
	if err != nil {
		t.Fatal(err)
	}
	round, err := DecodeResponsesResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp, round) {
		t.Fatalf("response semantics changed: %+v -> %+v", resp, round)
	}
	for _, bad := range []string{
		`{"id":"r","status":"failed","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"r","status":"completed"}`,
		`{"id":"r","status":"completed","output":[{"type":"tool_search_call"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"r","status":"completed","usage":{"input_tokens":-1,"output_tokens":1}}`,
		`{"id":"r","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":2}}}`,
	} {
		if _, err := DecodeResponsesResponse([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestResponsesReasoningDeltaKeepsZeroContentIndex(t *testing.T) {
	body, err := EncodeResponsesStreamEvent(ir.ResponsesStreamEvent{Kind: "response.reasoning_text.delta", ItemID: "rs", Delta: "thinking", ContentIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["content_index"]) != "0" {
		t.Fatalf("native required zero content_index absent: %s", body)
	}
}

func TestResponsesZeroUsageIncludesRequiredTotal(t *testing.T) {
	body, err := EncodeResponsesStreamEvent(ir.ResponsesStreamEvent{Kind: "response.completed", ResponseID: "r", Usage: &ir.Usage{}})
	if err != nil {
		t.Fatal(err)
	}
	var outer struct {
		Response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		t.Fatal(err)
	}
	if string(outer.Response.Usage["total_tokens"]) != "0" {
		t.Fatalf("native required reported zero total_tokens absent: %s", body)
	}
}

func TestResponsesMissingUsageFieldsRefuse(t *testing.T) {
	for _, usage := range []string{`{}`, `{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":0,"total_tokens":0}`, `{"output_tokens":0,"total_tokens":0}`} {
		if _, err := DecodeResponsesResponse([]byte(`{"id":"r","object":"response","status":"completed","output":[],"usage":` + usage + `}`)); err == nil {
			t.Fatalf("missing native usage became measured zero: %s", usage)
		}
	}
	if _, err := DecodeResponsesResponse([]byte(`{"id":"r","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)); err != nil {
		t.Fatalf("explicit reported zero refused: %v", err)
	}
}

func TestResponsesNullOrPartialUsageRefusesResponseAndTerminal(t *testing.T) {
	for _, usage := range []string{
		`{}`,
		`{"input_tokens":null,"output_tokens":0,"total_tokens":0}`,
		`{"input_tokens":0,"output_tokens":null,"total_tokens":0}`,
		`{"input_tokens":0,"output_tokens":0,"total_tokens":null}`,
		`{"input_tokens":0,"total_tokens":0}`,
		`{"input_tokens":0,"output_tokens":0}`,
	} {
		response := `{"id":"r","status":"completed","output":[],"usage":` + usage + `}`
		if _, err := DecodeResponsesResponse([]byte(response)); err == nil {
			t.Fatalf("missing/null response usage became reported zero: %s", usage)
		}
		if _, err := DecodeResponsesStreamEvent([]byte(`{"type":"response.completed","response":` + response + `}`)); err == nil {
			t.Fatalf("missing/null terminal usage became reported zero: %s", usage)
		}
	}
}

func TestResponsesPinnedNativeToolFamiliesRoundTrip(t *testing.T) {
	for _, tool := range []string{
		`{"type":"namespace","name":"functions","description":"fixture tools","tools":[{"type":"function","name":"read","description":"fixture","parameters":{"type":"object","properties":{"path":{"type":"string"}}},"strict":false}]}`,
		`{"type":"web_search","external_web_access":true}`,
	} {
		body := []byte(`{"model":"fixture-model","store":false,"stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture"}]}],"tools":[` + tool + `],"tool_choice":"auto"}`)
		req, err := DecodeResponsesRequest(body)
		if err != nil {
			t.Fatalf("actual pinned native tool shape refused: %v", err)
		}
		encoded, err := EncodeResponsesRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]json.RawMessage
		if err := json.Unmarshal(body, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &after); err != nil {
			t.Fatal(err)
		}
		var want, got any
		if err := json.Unmarshal(before["tools"], &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(after["tools"], &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatal("pinned native tool definitions lost through IR")
		}
		if _, err := ProjectResponsesToChat(req); err == nil {
			t.Fatal("native-only tool family silently represented as Chat")
		}
	}
}

func TestResponsesNativeToolConfigAndActionsRoundTrip(t *testing.T) {
	body := `{"model":"m","input":[{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"q","queries":["q","q2"]}},{"type":"web_search_call","id":"ws2","status":"open","action":{"type":"open_page","url":"https://example.com"}},{"type":"web_search_call","id":"ws3","status":"in_progress","action":{"type":"find_in_page","url":"https://example.com","pattern":"p"}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"citation","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":8}]}]}],"tools":[{"type":"namespace","name":"functions","description":"ordered","tools":[{"type":"function","name":"read","strict":false,"defer_loading":false,"parameters":{"type":"object"}},{"type":"custom","name":"patch","defer_loading":true,"format":{"type":"grammar","syntax":"lark","definition":"start: WORD"}}]},{"type":"web_search","external_web_access":false,"indexed_web_access":true,"filters":{"allowed_domains":["example.com"]},"user_location":{"type":"approximate","country":"US","region":"California","city":"Oakland","timezone":"America/Los_Angeles"},"search_context_size":"high","search_content_types":["text","image"]}],"tool_choice":"auto","store":false}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	round, err := DecodeResponsesRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req, round) {
		t.Fatalf("native tools/actions lost: %+v -> %+v", req, round)
	}
	if len(round.Tools[0].Tools) != 2 || round.Tools[0].Tools[0].Name != "read" || round.Tools[0].Tools[1].Name != "patch" || round.Tools[0].Tools[0].DeferLoading == nil || *round.Tools[0].Tools[0].DeferLoading || round.Tools[1].WebSearch.ExternalWebAccess == nil || *round.Tools[1].WebSearch.ExternalWebAccess {
		t.Fatalf("namespace identity/order or explicit false lost: %s", wire)
	}
	if _, err := ProjectResponsesToChat(req); err == nil {
		t.Fatal("Chat accepted native tool/action/citation semantics")
	}
	response := ir.Response{ID: "r", Model: "m", Items: req.Items, UsageReported: true, FinishReason: ir.FinishStop}
	encoded, err := EncodeResponsesResponse(response, "r")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeResponsesResponse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Items, decoded.Items) {
		t.Fatalf("native output/history changed: %s", encoded)
	}
}

func TestResponsesNativeSearchLifecycleAndCitation(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws","status":"in_progress"}}`,
		`{"type":"response.web_search_call.in_progress","item_id":"ws","output_index":0,"sequence_number":1}`,
		`{"type":"response.web_search_call.searching","item_id":"ws","output_index":0,"sequence_number":2}`,
		`{"type":"response.web_search_call.completed","item_id":"ws","output_index":0,"sequence_number":3}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws","status":"completed","action":{"type":"search","query":"q"}}}`,
		`{"type":"response.output_text.annotation.added","item_id":"msg","output_index":1,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}}`,
		`{"type":"response.content_part.done","item_id":"msg","output_index":1,"content_index":0,"part":{"type":"output_text","text":"text","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}}`,
	}
	for _, body := range events {
		ev, err := DecodeResponsesStreamEvent([]byte(body))
		if err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		wire, err := EncodeResponsesStreamEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		round, err := DecodeResponsesStreamEvent(wire)
		if err != nil {
			t.Fatalf("decode emitted %s: %v", wire, err)
		}
		if !reflect.DeepEqual(ev, round) {
			t.Fatalf("native lifecycle/citation lost: %+v -> %+v", ev, round)
		}
		if ev.Annotation != nil && !strings.Contains(string(wire), `"annotation_index":0`) {
			t.Fatalf("required zero annotation index missing: %s", wire)
		}
	}
}

func TestResponsesNativeToolUnknownFieldsRefuse(t *testing.T) {
	for _, tool := range []string{
		`{"type":"namespace","name":"n","tools":[{"type":"namespace","name":"child","tools":[]}]}`,
		`{"type":"namespace","name":"n","tools":[{"type":"web_search"}]}`,
		`{"type":"namespace","name":"n","tools":[] ,"unknown":true}`,
		`{"type":"web_search","filters":{"unknown":true}}`,
		`{"type":"web_search","search_context_size":"unbounded"}`,
		`{"type":"web_search","user_location":{"type":"precise"}}`,
		`{"type":"web_search","external_web_access":"true"}`,
		`{"type":"function","name":"f","parameters":{},"output_schema":{}}`,
		`{"type":"function","name":"f","tools":[]}`,
	} {
		if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[],"tools":[` + tool + `],"store":false}`)); err == nil {
			t.Fatalf("accepted unknown/cross-kind tool: %s", tool)
		}
	}
	for _, item := range []string{
		`{"type":"web_search_call","action":{"type":"unknown"}}`,
		`{"type":"web_search_call","action":{"type":"search","url":"https://example.com"}}`,
		`{"type":"web_search_call","action":{"type":"search","sources":[]}}`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"t","annotations":[{"type":"file_citation","url":"x","start_index":0,"end_index":1}]}]}`,
	} {
		if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[` + item + `],"store":false}`)); err == nil {
			t.Fatalf("accepted unknown/cross-kind item: %s", item)
		}
	}
}

func TestDecodeResponsesRequest_StringInput(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("model = %q, want m", req.Model)
	}
	if len(req.Items) != 1 || req.Items[0].Kind != "message" || req.Items[0].Role != ir.RoleUser {
		t.Fatalf("items = %+v, want one user message", req.Items)
	}
	if len(req.Items[0].Parts) != 1 || req.Items[0].Parts[0].Text != "hello" {
		t.Errorf("parts = %+v, want the input text", req.Items[0].Parts)
	}
}

// TestDecodeResponsesRequest_FullShape decodes every supported item family
// into canonical items with their identity kept: instructions stay separate,
// a developer message (in the string-content shorthand) keeps its role, a
// replayed call keeps its call_id, the tool output stays joined to it, the
// reasoning summary stays a reasoning item, and the sampling controls reach
// the canonical sampling spec. Only a structured multi-part tool output is
// refused, rather than being flattened into text.
func TestDecodeResponsesRequest_FullShape(t *testing.T) {
	body := `{
		"model": "m",
		"instructions": "be brief",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"developer","content":"sys"},
			{"type":"function_call","call_id":"call-1","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"sunny"},
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
	if req.Instructions != "be brief" {
		t.Errorf("instructions = %q", req.Instructions)
	}
	if len(req.Items) != 5 {
		t.Fatalf("items = %+v, want user + developer + replayed call + tool result + reasoning", req.Items)
	}
	if req.Items[0].Role != ir.RoleUser {
		t.Errorf("first item role = %q, want user", req.Items[0].Role)
	}
	if dev := req.Items[1]; dev.Role != ir.RoleDeveloper || len(dev.Parts) != 1 || dev.Parts[0].Kind != ir.PartText || dev.Parts[0].Text != "sys" {
		t.Errorf("developer item = %+v, want the developer role and its string content kept", dev)
	}
	if replayed := req.Items[2]; replayed.Kind != "function_call" || replayed.Name != "get_weather" || replayed.CallID != "call-1" {
		t.Errorf("replayed call = %+v, want the function_call item", replayed)
	}
	if result := req.Items[3]; result.Kind != "function_call_output" || result.Output != "sunny" || result.CallID != "call-1" {
		t.Errorf("tool result = %+v, want linked result", result)
	}
	if thinking := req.Items[4]; thinking.Kind != "reasoning" || len(thinking.Summary) != 1 || thinking.Summary[0] != "thought" {
		t.Errorf("reasoning = %+v, want the summary item", thinking)
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
	structured := `{"model":"m","input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"sunny"}]}]}`
	if _, err := DecodeResponsesRequest([]byte(structured)); err == nil {
		t.Error("structured tool output accepted; want a refusal instead of flattened text")
	}
}

// TestResponsesSamplingReachesBothCodecs carries temperature, top_p and
// max_output_tokens onto the Chat wire and the native Responses wire.
func TestResponsesSamplingReachesBothCodecs(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[{"type":"message","role":"assistant","content":"earlier"},{"type":"message","role":"user","content":"next"}],"temperature":0.25,"top_p":0.5,"max_output_tokens":64}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Items) != 2 || req.Items[0].Parts[0].Text != "earlier" || req.Items[1].Parts[0].Text != "next" {
		t.Fatalf("string content lost: %+v", req.Items)
	}
	projected, err := ProjectResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	chatBody, err := EncodeRequest(projected)
	if err != nil {
		t.Fatal(err)
	}
	var chat ChatRequest
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Temperature == nil || *chat.Temperature != 0.25 || chat.TopP == nil || *chat.TopP != 0.5 || chat.MaxTokens == nil || *chat.MaxTokens != 64 {
		t.Fatalf("Chat wire lost sampling: %s", chatBody)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "assistant" || chat.Messages[0].Content != "earlier" || chat.Messages[1].Content != "next" {
		t.Fatalf("Chat wire lost string content: %s", chatBody)
	}
	nativeBody, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"temperature":0.25`, `"top_p":0.5`, `"max_output_tokens":64`, `"type":"output_text","text":"earlier"`} {
		if !strings.Contains(string(nativeBody), want) {
			t.Fatalf("native wire missing %s: %s", want, nativeBody)
		}
	}
	if _, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"x","max_output_tokens":0}`)); err == nil {
		t.Fatal("a zero output budget was accepted")
	}
}

// TestDecodeResponsesRequest_Refusals pins what a Chat-served exchange
// refuses: either the decoder refuses the request outright, or the Chat
// projection refuses a control only a native Responses upstream can honor.
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
			req, err := DecodeResponsesRequest([]byte(body))
			if err == nil {
				_, err = ProjectResponsesToChat(req)
			}
			if err == nil {
				t.Fatalf("Chat-served %s = nil error, want a loud refusal", name)
			}
		})
	}
}

func TestDecodeResponsesRequest_ParallelToolCallsTrueAllowed(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"x","parallel_tool_calls":true}`))
	if err != nil {
		t.Fatalf("explicit parallel_tool_calls=true should be accepted: %v", err)
	}
	if len(req.Items) != 1 {
		t.Errorf("items = %+v", req.Items)
	}
}

// TestEncodeResponsesResponse_TextToolUsage encodes a text + tool-call reply
// as a message then a function_call with complete usage accounting. A Chat
// thinking part has no lossless Responses item, so it is refused instead of
// being relabeled as a reasoning summary.
func TestEncodeResponsesResponse_TextToolUsage(t *testing.T) {
	resp := ir.Response{
		Model:        "m",
		FinishReason: ir.FinishToolCalls,
		Usage:        ir.Usage{TokensIn: 10, TokensOut: 5, ReasoningTokens: 2, CacheReadTokens: 3},
		Content: []ir.Part{
			{Kind: ir.PartText, Text: "hi"},
			{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "call-9", Name: "f", Arguments: `{"a":1}`}},
		},
	}
	raw, err := EncodeResponsesResponse(resp, "resp-1")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if wire["id"] != "resp-1" || wire["object"] != "response" || wire["status"] != "completed" {
		t.Errorf("envelope = %v", wire)
	}
	output, _ := wire["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output has %d items, want 2 (message, function_call): %v", len(output), wire)
	}
	msg := output[0].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("first item = %v, want the assistant message", msg)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call-9" || call["name"] != "f" {
		t.Errorf("second item = %v, want the function call", call)
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
	resp.Content = append(resp.Content, ir.Part{Kind: ir.PartThinking, Text: "hmm"})
	if _, err := EncodeResponsesResponse(resp, "resp-1"); err == nil {
		t.Error("a Chat thinking part encoded; want a refusal, not a fabricated reasoning summary")
	}
}

type encodedStatus struct {
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []json.RawMessage `json:"output"`
}

func encodeStatus(t *testing.T, resp ir.Response) encodedStatus {
	t.Helper()
	raw, err := EncodeResponsesResponse(resp, "r")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var obj encodedStatus
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return obj
}

func TestEncodeResponsesResponse_IncompleteLength(t *testing.T) {
	// The production path: a documented Chat reply that stopped on length
	// passes the bridge validator, decodes, and encodes as incomplete.
	reply := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"annotations":[]},"logprobs":null,"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14},"system_fingerprint":"fp"}`)
	if err := ValidateChatResponsesSubset(reply, false); err != nil {
		t.Fatalf("length reply refused by the bridge: %v", err)
	}
	resp, err := DecodeResponse(reply)
	if err != nil {
		t.Fatal(err)
	}
	obj := encodeStatus(t, resp)
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

// TestEncodeResponsesResponse_ErrorIsFailed pins the encoder alone: neither
// upstream path produces an error finish, so production never reaches it.
func TestEncodeResponsesResponse_ErrorIsFailed(t *testing.T) {
	obj := encodeStatus(t, ir.Response{Model: "m", FinishReason: ir.FinishError})
	if obj.Status != "failed" {
		t.Errorf("status = %q, want failed", obj.Status)
	}
}

// TestResponsesTerminal_MatchesStatus pins that the terminal event type always
// matches the snapshot status the surface emits: a stop finish completes, a
// length or content-filter finish is incomplete, and a failed stream is
// failed. A finish cannot be relabeled onto a different terminal.
func TestResponsesTerminal_MatchesStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
		finish ir.FinishReason
	}{
		{"completed", "response.completed", ir.FinishStop},
		{"incomplete", "response.incomplete", ir.FinishLength},
		{"incomplete", "response.incomplete", ir.FinishContentFilter},
		{"failed", "response.failed", ""},
	} {
		raw, err := EncodeResponsesStreamEvent(ir.ResponsesStreamEvent{Kind: tc.want, ResponseID: "r", Usage: &ir.Usage{}, Response: &ir.Response{FinishReason: tc.finish}})
		if err != nil {
			t.Fatalf("status %q: %v", tc.status, err)
		}
		if !strings.Contains(string(raw), `"type":"`+tc.want+`"`) || !strings.Contains(string(raw), `"status":"`+tc.status+`"`) {
			t.Errorf("status %q -> event %s, want %q", tc.status, raw, tc.want)
		}
	}
	for _, mismatch := range []ir.ResponsesStreamEvent{
		{Kind: "response.completed", ResponseID: "r", Usage: &ir.Usage{}, Response: &ir.Response{FinishReason: ir.FinishLength}},
		{Kind: "response.incomplete", ResponseID: "r", Usage: &ir.Usage{}, Response: &ir.Response{FinishReason: ir.FinishStop}},
	} {
		if raw, err := EncodeResponsesStreamEvent(mismatch); err == nil {
			t.Errorf("%s with finish %q encoded as %s; want a refusal", mismatch.Kind, mismatch.Response.FinishReason, raw)
		}
	}
}

func TestResponsesStreamEventShapes(t *testing.T) {
	encode := func(ev ir.ResponsesStreamEvent) string {
		t.Helper()
		raw, err := EncodeResponsesStreamEvent(ev)
		if err != nil {
			t.Fatalf("encode %s: %v", ev.Kind, err)
		}
		return string(raw)
	}
	created := encode(ir.ResponsesStreamEvent{Kind: "response.created", ResponseID: "resp-7", Response: &ir.Response{Model: "m", Details: &ir.ResponseDetails{Object: "response", Status: "in_progress"}}})
	if !strings.Contains(created, `"status":"in_progress"`) {
		t.Errorf("created event = %s", created)
	}
	added := encode(ir.ResponsesStreamEvent{Kind: "response.output_item.added", OutputIndex: 1, Item: &ir.Item{Kind: "function_call", ID: "fc-1", CallID: "fc-1", Name: "get_weather", Status: "in_progress"}})
	var wire map[string]any
	if err := json.Unmarshal([]byte(added), &wire); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if wire["type"] != "response.output_item.added" || wire["output_index"] != 1.0 {
		t.Errorf("added event = %v", wire)
	}
	item := wire["item"].(map[string]any)
	if item["call_id"] != "fc-1" || item["name"] != "get_weather" {
		t.Errorf("added item = %v", item)
	}
	delta := encode(ir.ResponsesStreamEvent{Kind: "response.output_text.delta", ItemID: "msg-1", Delta: "Hel"})
	if !strings.Contains(delta, `"delta":"Hel"`) || !strings.Contains(delta, `"item_id":"msg-1"`) {
		t.Errorf("delta event = %s", delta)
	}
	done := encode(ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.done", OutputIndex: 1, ItemID: "fc-1", Delta: `{"a":1}`})
	if !strings.Contains(done, `"arguments":"{\"a\":1}"`) {
		t.Errorf("done event = %s", done)
	}
	// Done events must not carry a delta key, and deltas must not carry text/arguments.
	textDone := encode(ir.ResponsesStreamEvent{Kind: "response.output_text.done", ItemID: "msg-1", Delta: "hi"})
	if strings.Contains(textDone, "delta") || !strings.Contains(textDone, `"text":"hi"`) {
		t.Errorf("text done event = %s", textDone)
	}
}

// TestValidateChatResponsesSubsetClassifiesFields drives documented OpenAI
// Chat Completions replies and stream chunks through the bridge: envelope
// metadata, usage counters and empty fields pass; anything that would be
// dropped with content in it is refused.
func TestValidateChatResponsesSubsetClassifiesFields(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`
	reply := func(message, extra string) string {
		return `{"id":"chatcmpl-1","object":"chat.completion","created":1741569952,"model":"m","choices":[{"index":0,"message":` + message + `,"logprobs":null,"finish_reason":"stop"}],` + usage + `,"service_tier":"default","system_fingerprint":"fp_1"` + extra + `}`
	}
	chunk := func(choices, extra string) string {
		return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","service_tier":"default","system_fingerprint":"fp_1","choices":` + choices + `,"usage":null,"obfuscation":"pad"` + extra + `}`
	}
	accepted := []struct {
		name   string
		body   string
		stream bool
	}{
		{"reply", reply(`{"role":"assistant","content":"Hello","refusal":null,"annotations":[]}`, ""), false},
		{"reply_tool_call", reply(`{"role":"assistant","content":null,"refusal":null,"annotations":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]}`, ""), false},
		{"reply_empty_logprobs_object", strings.Replace(reply(`{"role":"assistant","content":"Hi"}`, ""), `"logprobs":null`, `"logprobs":{"content":null,"refusal":null}`, 1), false},
		{"chunk_role", chunk(`[{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":null}]`, ""), true},
		{"chunk_tool_call", chunk(`[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]},"logprobs":null,"finish_reason":null}]`, ""), true},
		{"chunk_finish_length", chunk(`[{"index":0,"delta":{},"logprobs":null,"finish_reason":"length"}]`, ""), true},
		{"chunk_include_usage", strings.Replace(chunk(`[]`, ""), `"usage":null`, usage, 1), true},
		// Aggregator metadata and accounting extensions.
		{"openrouter_reply", strings.Replace(strings.Replace(reply(`{"role":"assistant","content":"Hi","refusal":null,"reasoning":null}`, `,"provider":"Fixture"`), `"finish_reason":"stop"`, `"finish_reason":"stop","native_finish_reason":"end_turn"`, 1), `"total_tokens":29,`, `"total_tokens":29,"cost":0.00012,"is_byok":false,"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.00005},`, 1), false},
		{"groq_reply", strings.Replace(reply(`{"role":"assistant","content":"Hi"}`, `,"x_groq":{"id":"req_1"},"usage_breakdown":null`), `"total_tokens":29,`, `"total_tokens":29,"queue_time":0.03,"prompt_time":0.001,"completion_time":0.02,"total_time":0.021,`, 1), false},
		{"azure_reply_unfiltered", strings.Replace(reply(`{"role":"assistant","content":"Hi","refusal":null,"annotations":[]}`, `,"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"jailbreak":{"filtered":false,"detected":false}}}]`), `"logprobs":null`, `"logprobs":null,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"protected_material_text":{"filtered":false,"detected":false}}`, 1), false},
		{"azure_reply_filtered_output", strings.Replace(strings.Replace(reply(`{"role":"assistant","content":null}`, ""), `"logprobs":null`, `"logprobs":null,"content_filter_results":{"violence":{"filtered":true,"severity":"high"}}`, 1), `"finish_reason":"stop"`, `"finish_reason":"content_filter"`, 1), false},
		{"azure_first_chunk", `{"choices":[],"created":0,"id":"","model":"","object":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}]}`, true},
	}
	for _, tc := range accepted {
		if err := ValidateChatResponsesSubset([]byte(tc.body), tc.stream); err != nil {
			t.Errorf("documented %s refused: %v", tc.name, err)
		}
	}
	refused := []struct {
		name   string
		body   string
		stream bool
	}{
		{"refusal_text", reply(`{"role":"assistant","content":null,"refusal":"I cannot help"}`, ""), false},
		{"annotations", reply(`{"role":"assistant","content":"x","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.com"}}]}`, ""), false},
		{"audio", reply(`{"role":"assistant","content":null,"audio":{"id":"a","data":"AAAA"}}`, ""), false},
		{"reasoning_content", reply(`{"role":"assistant","content":"x","reasoning_content":"hidden chain"}`, ""), false},
		{"legacy_function_call", reply(`{"role":"assistant","content":null,"function_call":{"name":"f","arguments":"{}"}}`, ""), false},
		{"logprobs_content", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"logprobs":null`, `"logprobs":{"content":[{"token":"x","logprob":-0.1}]}`, 1), false},
		{"top_level_error", reply(`{"role":"assistant","content":"x"}`, `,"error":{"message":"overloaded"}`), false},
		{"role_user", reply(`{"role":"user","content":"x"}`, ""), false},
		{"content_parts", reply(`{"role":"assistant","content":[{"type":"text","text":"x"}]}`, ""), false},
		{"finish_function_call", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"finish_reason":"stop"`, `"finish_reason":"function_call"`, 1), false},
		{"zero_choices", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"logprobs":null,"finish_reason":"stop"}]`, `"choices":[]`, 1), false},
		{"two_choices", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"finish_reason":"stop"}]`, `"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"y"},"finish_reason":"stop"}]`, 1), false},
		{"usage_counter_text", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"audio_tokens":0}`, `"audio_tokens":"0"}`, 1), false},
		{"chunk_two_tool_calls", chunk(`[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":""}},{"index":1,"id":"b","type":"function","function":{"name":"g","arguments":""}}]},"finish_reason":null}]`, ""), true},
		{"chunk_refusal_text", chunk(`[{"index":0,"delta":{"refusal":"no"},"finish_reason":null}]`, ""), true},
		{"chunk_unknown_choice_content", chunk(`[{"index":0,"delta":{"content":"x"},"finish_reason":null,"content_blocks":[{"text":"y"}]}]`, ""), true},
		// Guards that keep a malformed reply from being served or metered.
		{"chunk_second_choice_index", chunk(`[{"index":1,"delta":{"content":"other"},"finish_reason":null}]`, ""), true},
		{"duplicate_message_key", strings.Replace(reply(`{"role":"assistant","content":null,"refusal":"I refuse"}`, ""), `"logprobs":null`, `"message":{"role":"assistant","content":"ok"},"logprobs":null`, 1), false},
		{"choice_without_message", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"message":{"role":"assistant","content":"x"},`, ``, 1), false},
		{"null_message", reply(`null`, ""), false},
		{"negative_reasoning_tokens", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"reasoning_tokens":0`, `"reasoning_tokens":-7`, 1), false},
		// Content filters that blocked something are never served as a success.
		{"azure_filtered_output_with_stop", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"logprobs":null`, `"logprobs":null,"content_filter_results":{"violence":{"filtered":true,"severity":"high"}}`, 1), false},
		{"azure_filtered_prompt", reply(`{"role":"assistant","content":"x"}`, `,"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"jailbreak":{"filtered":true,"detected":true}}}]`), false},
		{"usage_cost_text", strings.Replace(reply(`{"role":"assistant","content":"x"}`, ""), `"total_tokens":29,`, `"total_tokens":29,"cost":"0.1",`, 1), false},
	}
	for _, tc := range refused {
		if err := ValidateChatResponsesSubset([]byte(tc.body), tc.stream); err == nil {
			t.Errorf("%s accepted; it would be dropped by the Chat decoder", tc.name)
		}
	}
}

// TestResponsesNativeGuardsPinned pins the codec guards that stop a native
// exchange from fabricating success or silently dropping a Responses control.
func TestResponsesNativeGuardsPinned(t *testing.T) {
	// A turn the backend did not end is never decoded; an incomplete turn
	// needs a reason the IR carries; neither is ever labeled completed.
	for _, refused := range []string{
		`"status":"failed"`,
		`"status":"in_progress"`,
		`"status":"incomplete"`,
		`"status":"incomplete","incomplete_details":{"reason":"interrupted"}`,
		`"status":"completed","incomplete_details":{"reason":"max_output_tokens"}`,
	} {
		body := `{"id":"r","object":"response",` + refused + `,"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		if _, err := DecodeResponsesResponse([]byte(body)); err == nil {
			t.Errorf("native reply %s with complete usage decoded", refused)
		}
	}
	for reason, finish := range map[string]ir.FinishReason{"max_output_tokens": ir.FinishLength, "content_filter": ir.FinishContentFilter} {
		body := `{"id":"r","object":"response","status":"incomplete","incomplete_details":{"reason":"` + reason + `"},"output":[{"type":"message","id":"m","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil || resp.FinishReason != finish {
			t.Fatalf("incomplete %s decoded as %q, %v", reason, resp.FinishReason, err)
		}
		encoded, err := EncodeResponsesResponse(resp, "r")
		if err != nil || !strings.Contains(string(encoded), `"status":"incomplete"`) || !strings.Contains(string(encoded), `"reason":"`+reason+`"`) || strings.Contains(string(encoded), `"status":"completed"`) {
			t.Fatalf("incomplete %s re-encoded as %s, %v", reason, encoded, err)
		}
		event, err := DecodeResponsesStreamEvent([]byte(`{"type":"response.incomplete","response":` + body + `}`))
		if err != nil || event.Response.FinishReason != finish || event.Usage == nil {
			t.Fatalf("incomplete %s terminal decoded as %+v, %v", reason, event, err)
		}
		if _, err := DecodeResponsesStreamEvent([]byte(`{"type":"response.completed","response":` + body + `}`)); err == nil {
			t.Fatalf("an incomplete snapshot decoded under a completed terminal")
		}
	}
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"x","parallel_tool_calls":false,"store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"parallel_tool_calls":false`) {
		t.Fatalf("explicit serial tool use did not reach the native wire: %s", wire)
	}
	for name, chatShaped := range map[string]ir.Request{
		"messages": {Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: "x"}}}}},
		"system":   {Model: "m", System: []ir.Part{{Kind: ir.PartText, Text: "x"}}},
		"stop":     {Model: "m", Sampling: ir.Sampling{Stop: []string{"END"}}},
	} {
		if body, err := EncodeResponsesRequest(chatShaped); err == nil {
			t.Errorf("Chat-shaped %s encoded onto the native wire: %s", name, body)
		}
	}
}

// TestResponsesChatProjectionRefusesNativeOnlyControls pins every control the
// Chat projection refuses rather than dropping.
func TestResponsesChatProjectionRefusesNativeOnlyControls(t *testing.T) {
	for name, body := range map[string]string{
		"developer_item":   `{"model":"m","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]}]}`,
		"text_format":      `{"model":"m","input":"x","text":{"format":{"type":"json_schema","name":"s","schema":{"type":"object"}}}}`,
		"message_id":       `{"model":"m","input":[{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"x"}]}]}`,
		"prompt_cache_key": `{"model":"m","input":"x","prompt_cache_key":"k"}`,
		"service_tier":     `{"model":"m","input":"x","service_tier":"flex"}`,
		"client_metadata":  `{"model":"m","input":"x","client_metadata":{"k":"v"}}`,
	} {
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if _, err := ProjectResponsesToChat(req); err == nil {
			t.Errorf("Chat projection dropped %s instead of refusing it", name)
		}
	}
}

// TestResponsesRequiredZeroIndexes pins the zero output_index and
// summary_index the protocol requires on the first item.
func TestResponsesRequiredZeroIndexes(t *testing.T) {
	for _, tc := range []struct {
		ev    ir.ResponsesStreamEvent
		field string
	}{
		{ir.ResponsesStreamEvent{Kind: "response.output_item.added", OutputIndex: 0, Item: &ir.Item{Kind: "message", ID: "m", Role: ir.RoleAssistant, Status: "in_progress"}}, "output_index"},
		{ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.delta", ItemID: "fc", Delta: "{}"}, "output_index"},
		{ir.ResponsesStreamEvent{Kind: "response.reasoning_summary_text.delta", ItemID: "rs", Delta: "plan"}, "summary_index"},
	} {
		body, err := EncodeResponsesStreamEvent(tc.ev)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields[tc.field]) != "0" {
			t.Errorf("%s: required zero %s absent: %s", tc.ev.Kind, tc.field, body)
		}
	}
}

// TestResponsesNestingDepthBounded nests a schema past the bound under a field
// the codec admits, so only the depth limit can refuse it.
func TestResponsesNestingDepthBounded(t *testing.T) {
	nest := func(depth int) string {
		return strings.Repeat(`{"a":`, depth) + `{}` + strings.Repeat(`}`, depth)
	}
	tool := func(depth int) string {
		return `{"model":"m","input":[],"tools":[{"type":"function","name":"f","parameters":` + nest(depth) + `}]}`
	}
	if _, err := DecodeResponsesRequest([]byte(tool(40))); err != nil {
		t.Fatalf("a schema within the bound was refused: %v", err)
	}
	if _, err := DecodeResponsesRequest([]byte(tool(70))); err == nil {
		t.Fatal("a schema past the nesting bound was accepted")
	}
}
