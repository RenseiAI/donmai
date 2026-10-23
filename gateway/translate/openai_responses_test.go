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
// a developer message keeps its role, a replayed call keeps its call_id, the
// tool output stays joined to it, and the reasoning summary stays a reasoning
// item. Shapes outside the stateless codec (string content, structured tool
// output, sampling controls) are refused rather than flattened.
func TestDecodeResponsesRequest_FullShape(t *testing.T) {
	body := `{
		"model": "m",
		"instructions": "be brief",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]},
			{"type":"function_call","call_id":"call-1","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"sunny"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}
		],
		"tools": [{"type":"function","name":"get_weather","description":"d","parameters":{"type":"object"}}],
		"tool_choice": "auto",
		"reasoning": {"effort":"high"}
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
	if dev := req.Items[1]; dev.Role != ir.RoleDeveloper || len(dev.Parts) != 1 || dev.Parts[0].Text != "sys" {
		t.Errorf("developer item = %+v, want the developer role kept", dev)
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
	for name, refused := range map[string]string{
		"string_content":    `{"model":"m","input":[{"type":"message","role":"developer","content":"sys"}]}`,
		"structured_output": `{"model":"m","input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"sunny"}]}]}`,
		"temperature":       `{"model":"m","input":"x","temperature":0.5}`,
		"max_output_tokens": `{"model":"m","input":"x","max_output_tokens":100}`,
	} {
		if _, err := DecodeResponsesRequest([]byte(refused)); err == nil {
			t.Errorf("%s accepted; want a refusal instead of a flattened or dropped field", name)
		}
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
	obj := encodeStatus(t, ir.Response{Model: "m", FinishReason: ir.FinishLength})
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
	obj := encodeStatus(t, ir.Response{Model: "m", FinishReason: ir.FinishError})
	if obj.Status != "failed" {
		t.Errorf("status = %q, want failed", obj.Status)
	}
}

// TestResponsesTerminal_MatchesStatus pins the only terminal the codec emits:
// response.completed, carrying a completed snapshot. An incomplete or failed
// stream is never encoded as a codec event; the surface closes it with its
// own response.failed frame and never meters it.
func TestResponsesTerminal_MatchesStatus(t *testing.T) {
	raw, err := EncodeResponsesStreamEvent(ir.ResponsesStreamEvent{Kind: "response.completed", ResponseID: "r", Usage: &ir.Usage{}})
	if err != nil {
		t.Fatalf("encode completed: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"response.completed"`) || !strings.Contains(string(raw), `"status":"completed"`) {
		t.Errorf("wire event missing type or status: %s", raw)
	}
	for _, kind := range []string{"response.incomplete", "response.failed"} {
		if raw, err := EncodeResponsesStreamEvent(ir.ResponsesStreamEvent{Kind: kind, ResponseID: "r", Usage: &ir.Usage{}}); err == nil {
			t.Errorf("%s encoded as %s; want a refusal", kind, raw)
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
