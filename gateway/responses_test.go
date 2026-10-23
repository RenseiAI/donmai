package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/translate"
	"github.com/RenseiAI/donmai/gateway/upstream"
)

func bindResponses(t *testing.T, g *gateway.Gateway, session, key, url string, native bool) agent.EndpointBinding {
	t.Helper()
	compat := upstream.OpenAICompat{Company: "openai", BaseURL: url}
	var backend upstream.Upstream = &compat
	if native {
		backend = &upstream.OpenAIResponses{OpenAICompat: compat}
	}
	b, err := g.BindProtocol(gateway.BindConfig{SessionID: session, DispatchID: session + "-dispatch", Harness: "codex", Company: agent.CompanyOpenAI, Model: "fixture-model", AuthMode: agent.AuthMetered, Upstream: backend, Source: pool.SingleKey{Key: pool.Credential{ID: session, Secret: key}}}, agent.ProtoOpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	if b.Protocol != agent.ProtoOpenAIResponses || b.Host != agent.HostGateway {
		t.Fatalf("wrong binding %+v", b)
	}
	return b
}

func responseRequest(t *testing.T, b agent.EndpointBinding, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func TestGatewayResponsesNativeToolFollowupAndCost(t *testing.T) {
	observed := make(chan ir.Request, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer native-key" {
			t.Errorf("wrong native route/auth: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		req, err := translate.DecodeResponsesRequest(raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		observed <- req
		w.Header().Set("Content-Type", "application/json")
		if len(req.Items) > 0 && req.Items[len(req.Items)-1].Kind == "function_call_output" {
			_, _ = io.WriteString(w, `{"id":"r2","status":"completed","model":"fixture-model","output":[{"type":"message","id":"msg2","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"read succeeded"}]}],"usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"r1","status":"completed","model":"fixture-model","output":[{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"opaque=="},{"type":"function_call","id":"fc1","call_id":"call1","name":"read","arguments":"{\"path\":\"a\"}"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`)
		}
	}))
	t.Cleanup(ts.Close)
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "responses-native", "native-key", ts.URL, true)
	status, body := responseRequest(t, b, `{"model":"fixture-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read"}]}],"store":false,"include":["reasoning.encrypted_content"]}`)
	if status != 200 {
		t.Fatalf("native status=%d body=%s", status, body)
	}
	first, err := translate.DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "r1" || first.Items[0].EncryptedContent != "opaque==" || first.Items[1].ID != "fc1" || first.Items[1].CallID != "call1" {
		t.Fatalf("native output lost semantics %+v", first)
	}
	status, body = responseRequest(t, b, `{"model":"fixture-model","input":[{"type":"reasoning","id":"rs1","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"opaque=="},{"type":"function_call","id":"fc1","call_id":"call1","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"function_call_output","call_id":"call1","output":"file contents"}],"store":false,"include":["reasoning.encrypted_content"]}`)
	if status != 200 || !strings.Contains(body, "read succeeded") {
		t.Fatalf("followup status=%d body=%s", status, body)
	}
	<-observed
	followup := <-observed
	if followup.Items[2].CallID != "call1" || followup.Items[2].Output != "file contents" || followup.Items[0].EncryptedContent != "opaque==" {
		t.Fatalf("upstream lost followup %+v", followup)
	}
	rows := waitEvents(t, sink, 2)
	if len(rows) != 2 || rows[0].SessionID != "responses-native" || rows[0].DispatchID != "responses-native-dispatch" || rows[0].Harness != "codex" || rows[0].TokensIn != 7 || rows[1].TokensOut != 2 {
		t.Fatalf("cost rows=%+v", rows)
	}
}

func TestGatewayResponsesNativeSSEOrderCustomReasoning(t *testing.T) {
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"r"}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"item_id":"msg","delta":"hello"}`,
		`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"type":"message","id":"msg","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"hello"}]}}`,
		`{"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"opaque"}}`,
		`{"type":"response.output_item.added","sequence_number":5,"output_index":2,"item":{"type":"custom_tool_call","id":"ctc","status":"in_progress","call_id":"custom1","name":"shell","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","sequence_number":6,"output_index":2,"item_id":"ctc","delta":"echo ok"}`,
		`{"type":"response.custom_tool_call_input.done","sequence_number":7,"output_index":2,"item_id":"ctc","input":"echo ok"}`,
		`{"type":"response.output_item.done","sequence_number":8,"output_index":2,"item":{"type":"custom_tool_call","id":"ctc","status":"completed","call_id":"custom1","name":"shell","input":"echo ok"}}`,
		`{"type":"response.completed","sequence_number":9,"response":{"id":"r","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"encrypted_content":"opaque"`) || !strings.Contains(string(raw), `"type":"custom_tool_call_output"`) || !strings.Contains(string(raw), `"call_id":"custom1"`) {
			t.Errorf("request lost custom/reasoning %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(ts.Close)
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "sse", "key", ts.URL, true)
	status, body := responseRequest(t, b, `{"model":"fixture-model","store":false,"stream":true,"input":[{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"opaque"},{"type":"custom_tool_call","id":"ctc","call_id":"custom1","name":"shell","input":"echo ok"},{"type":"custom_tool_call_output","call_id":"custom1","output":"ok"}]}`)
	if status != 200 {
		t.Fatalf("SSE status=%d body=%s", status, body)
	}
	var got []ir.ResponsesStreamEvent
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			ev, err := translate.DecodeResponsesStreamEvent([]byte(strings.TrimPrefix(line, "data: ")))
			if err != nil {
				t.Fatalf("SSE event %s: %v", line, err)
			}
			got = append(got, ev)
		}
	}
	if len(got) != len(events) || got[2].Delta != "hello" || got[4].Item.EncryptedContent != "opaque" || got[8].Item.CallID != "custom1" || got[8].Item.Input != "echo ok" || got[9].Kind != "response.completed" {
		t.Fatalf("SSE semantics=%+v body=%s", got, body)
	}
	for i, ev := range got {
		if ev.SequenceNumber == nil || *ev.SequenceNumber != i {
			t.Fatalf("event %d sequence=%v", i, ev.SequenceNumber)
		}
	}
	rows := waitEvents(t, sink, 1)
	if len(rows) != 1 || rows[0].TokensIn != 7 || rows[0].TokensOut != 3 {
		t.Fatalf("SSE cost %+v", rows)
	}
}

func TestGatewayResponsesProtocolAndTokenIsolation(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		key := r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "r", "status": "completed",
			"output": []map[string]any{{
				"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "output_text", "text": key}},
			}},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(ts.Close)
	g := startGateway(t, &costfeed.MemorySink{})
	a := bindResponses(t, g, "a", "key-a", ts.URL, true)
	b := bindResponses(t, g, "b", "key-b", ts.URL, true)
	for _, binding := range []agent.EndpointBinding{a, b} {
		status, body := responseRequest(t, binding, `{"model":"fixture-model","input":[],"store":false}`)
		if status != 200 || !strings.Contains(body, "key-"+map[string]string{a.Env[gateway.TokenEnvVar]: "a", b.Env[gateway.TokenEnvVar]: "b"}[binding.Env[gateway.TokenEnvVar]]) {
			t.Fatalf("token route status=%d body=%s", status, body)
		}
	}
	chat := bindSession(t, g, "chat", "opencode", "chat-key", ts.URL, "fixture-model")
	before := calls.Load()
	status, body := responseRequest(t, chat, `{"model":"fixture-model","input":[]}`)
	if status != 403 {
		t.Fatalf("Chat token reached Responses: %d %s", status, body)
	}
	resp := chatRequest(t, a, false)
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("Responses token reached Chat: %d", resp.StatusCode)
	}
	if calls.Load() != before {
		t.Fatal("cross-surface refusal reached upstream")
	}
	invalid := a
	invalid.Env = map[string]string{gateway.TokenEnvVar: a.Env[gateway.TokenEnvVar] + "suffix"}
	status, _ = responseRequest(t, invalid, `{"model":"fixture-model","input":[]}`)
	if status != 401 {
		t.Fatalf("partial token accepted: %d", status)
	}
	for _, protocol := range []agent.WireProtocol{"", "unknown", agent.ProtoAnthropicMessages} {
		if _, err := g.BindProtocol(gateway.BindConfig{}, protocol); err == nil {
			t.Fatalf("accepted protocol %q", protocol)
		}
	}
	if _, err := g.BindProtocol(gateway.BindConfig{Surface: agent.ProtoOpenAIChat}, agent.ProtoOpenAIResponses); err == nil {
		t.Fatal("conflicting surface accepted")
	}
	if _, err := g.Bind(gateway.BindConfig{Surface: agent.ProtoOpenAIResponses}); err == nil {
		t.Fatal("legacy Bind accepted Responses")
	}
}

func TestGatewayResponsesChatSubsetHTTPAndSSE(t *testing.T) {
	up := fakeUpstream(t, "chat-key")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "chat-subset", "chat-key", up.URL, false)
	for _, stream := range []bool{false, true} {
		status, body := responseRequest(t, b, `{"model":"fixture-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":`+boolStr(stream)+`,"store":false}`)
		if status != 200 || !strings.Contains(body, func() string {
			if stream {
				return "response.completed"
			}
			return "Hello"
		}()) {
			t.Fatalf("Chat subset stream=%v status=%d body=%s", stream, status, body)
		}
	}
	rows := waitEvents(t, sink, 2)
	if len(rows) != 2 {
		t.Fatalf("Chat rows=%+v", rows)
	}
	status, _ := responseRequest(t, b, `{"model":"fixture-model","input":[],"store":false,"include":["reasoning.encrypted_content"]}`)
	if status != 400 {
		t.Fatalf("unrepresentable Chat request status=%d", status)
	}
}

func TestGatewayResponsesFailuresNeverMeterSuccess(t *testing.T) {
	for _, terminal := range []string{"response.failed", "response.incomplete", "EOF", "malformed", "wrong-id"} {
		t.Run(terminal, func(t *testing.T) {
			handlerDone := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer close(handlerDone)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n")
				switch terminal {
				case "EOF":
				case "malformed":
					_, _ = io.WriteString(w, "data: {bad}\n\n")
				case "wrong-id":
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"other","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
				default:
					_, _ = io.WriteString(w, `data: {"type":"`+terminal+`","response":{"id":"r"}}`+"\n\n")
				}
			}))
			t.Cleanup(ts.Close)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "failed", "key", ts.URL, true)
			_, body := responseRequest(t, b, `{"model":"fixture-model","input":[],"stream":true,"store":false}`)
			<-handlerDone
			if strings.Contains(body, `"type":"response.completed"`) || !strings.Contains(body, `"type":"response.failed"`) {
				t.Fatalf("false terminal %s", body)
			}
			if rows := sink.Events(); len(rows) != 0 {
				t.Fatalf("failed stream metered %+v", rows)
			}
		})
	}
}

func TestGatewayResponsesUnknownRequestDoesNotDial(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(ts.Close)
	g := startGateway(t, &costfeed.MemorySink{})
	b := bindResponses(t, g, "bad", "key", ts.URL, true)
	for _, body := range []string{`{bad}`, `{"model":"fixture-model","input":[],"unknown":true}`, `{"model":"fixture-model","input":[],"store":true}`, `{"model":"fixture-model","input":[],"previous_response_id":"state"}`} {
		status, _ := responseRequest(t, b, body)
		if status != 400 {
			t.Fatalf("refusal status=%d", status)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("malformed/stateful request dialed upstream")
	}
}

func TestGatewayResponsesChatToolFollowup(t *testing.T) {
	observed := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		observed <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"tool result accepted"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(ts.Close)
	g := startGateway(t, &costfeed.MemorySink{})
	b := bindResponses(t, g, "tool-chat", "key", ts.URL, false)
	status, body := responseRequest(t, b, `{"model":"fixture-model","input":[{"type":"function_call","call_id":"call1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call1","output":"result"}],"store":false}`)
	if status != 200 || !strings.Contains(body, "tool result accepted") {
		t.Fatalf("tool result status=%d body=%s", status, body)
	}
	var sent struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		}
	}
	if err := json.Unmarshal([]byte(<-observed), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 2 || sent.Messages[1].Role != "tool" || sent.Messages[1].ToolCallID != "call1" || sent.Messages[1].Content != "result" {
		t.Fatalf("tool output mapping %+v", sent)
	}
}

func TestGatewayResponsesChatUsagePresence(t *testing.T) {
	cases := []struct {
		name, usage string
		valid       bool
	}{
		{"missing", "", false},
		{"null", "null", false},
		{"empty", "{}", false},
		{"missing_input", `{"completion_tokens":0,"total_tokens":0}`, false},
		{"missing_output", `{"prompt_tokens":0,"total_tokens":0}`, false},
		{"missing_total", `{"prompt_tokens":0,"completion_tokens":0}`, false},
		{"null_input", `{"prompt_tokens":null,"completion_tokens":0,"total_tokens":0}`, false},
		{"null_output", `{"prompt_tokens":0,"completion_tokens":null,"total_tokens":0}`, false},
		{"null_total", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":null}`, false},
		{"reported_zero", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+"/stream="+boolStr(stream), func(t *testing.T) {
				var calls atomic.Int32
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/chat/completions" {
						t.Errorf("wrong Chat backend path %s", r.URL.Path)
					}
					request, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(request), `"role":"user"`) || !strings.Contains(string(request), `"content":"check"`) {
						t.Errorf("lost inbound request: %s", request)
					}
					usageField := ""
					if tc.usage != "" {
						usageField = `,"usage":` + tc.usage
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						// Intermediate chunks may omit usage; final success still requires it.
						_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
						_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`+usageField+"}\n\n")
						_, _ = io.WriteString(w, "data: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]`+usageField+`}`)
					}
				}))
				t.Cleanup(ts.Close)
				sink := &costfeed.MemorySink{}
				g := startGateway(t, sink)
				b := bindResponses(t, g, "usage-chat", "key", ts.URL, false)
				status, body := responseRequest(t, b, `{"model":"fixture-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"check"}]}],"stream":`+boolStr(stream)+`,"store":false}`)
				if calls.Load() != 1 {
					t.Fatalf("backend calls=%d", calls.Load())
				}
				if tc.valid {
					if status != 200 || !strings.Contains(body, `"total_tokens":0`) || stream && !strings.Contains(body, `"type":"response.completed"`) || !stream && !strings.Contains(body, `"status":"completed"`) {
						t.Fatalf("reported zero refused or omitted: status=%d body=%s", status, body)
					}
					rows := waitEvents(t, sink, 1)
					if len(rows) != 1 || rows[0].TokensIn != 0 || rows[0].TokensOut != 0 {
						t.Fatalf("reported zero meter=%+v", rows)
					}
				} else {
					if !stream && status != 502 || strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, `"status":"completed"`) {
						t.Fatalf("missing usage became success: status=%d body=%s", status, body)
					}
					if stream && !strings.Contains(body, `"type":"response.failed"`) {
						t.Fatalf("missing usage did not fail stream: %s", body)
					}
					if rows := sink.Events(); len(rows) != 0 {
						t.Fatalf("missing usage metered as zero: %+v", rows)
					}
				}
			})
		}
	}
}

func TestGatewayResponsesNativeNamespaceAndWebSearchExchange(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer search-key" {
			t.Errorf("native route/auth changed: %s", r.URL.Path)
		}
		req, err := translate.DecodeResponsesRequest(raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.Tools) != 2 || req.Tools[0].Kind != "namespace" || req.Tools[0].Name != "functions" || len(req.Tools[0].Tools) != 2 || req.Tools[0].Tools[0].Name != "read" || req.Tools[0].Tools[1].Name != "patch" || req.Tools[1].Kind != "web_search" || req.Tools[1].WebSearch == nil || req.Tools[1].WebSearch.ExternalWebAccess == nil || !*req.Tools[1].WebSearch.ExternalWebAccess {
			t.Errorf("native tool identity/order/config lost: %+v", req.Tools)
		}
		count := calls.Add(1)
		if count == 2 && (len(req.Items) != 2 || req.Items[0].WebSearchAction == nil || req.Items[0].WebSearchAction.Query == nil || *req.Items[0].WebSearchAction.Query != "fixture query" || len(req.Items[1].Parts[0].Annotations) != 1 || req.Items[1].Parts[0].Annotations[0].URL != "https://example.com") {
			t.Errorf("search history/action/citation lost: %+v", req.Items)
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"search-r1","status":"completed","output":[{"type":"web_search_call","id":"ws1","status":"completed","action":{"type":"search","query":"fixture query"}},{"type":"message","id":"msg1","role":"assistant","content":[{"type":"output_text","text":"fact","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"search-r2"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws2","status":"in_progress"}}`,
			`{"type":"response.web_search_call.in_progress","item_id":"ws2","output_index":0}`,
			`{"type":"response.web_search_call.searching","item_id":"ws2","output_index":0}`,
			`{"type":"response.web_search_call.completed","item_id":"ws2","output_index":0}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws2","status":"completed","action":{"type":"open_page","url":"https://example.com"}}}`,
			`{"type":"response.output_text.annotation.added","item_id":"msg2","output_index":1,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg2","role":"assistant","content":[{"type":"output_text","text":"fact","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}]}}`,
			`{"type":"response.completed","response":{"id":"search-r2","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
		} {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	t.Cleanup(ts.Close)
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "search-session", "search-key", ts.URL, true)
	tools := `[{"type":"namespace","name":"functions","description":"ordered","tools":[{"type":"function","name":"read","strict":false,"parameters":{"type":"object"}},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: WORD"}}]},{"type":"web_search","external_web_access":true}]`
	status, body := responseRequest(t, b, `{"model":"fixture-model","input":[],"tools":`+tools+`,"store":false}`)
	if status != 200 {
		t.Fatalf("native search request status=%d body=%s", status, body)
	}
	response, err := translate.DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 || response.Items[0].Kind != "web_search_call" || response.Items[0].ID != "ws1" || response.Items[0].WebSearchAction == nil || len(response.Items[1].Parts[0].Annotations) != 1 {
		t.Fatalf("native search reply lost: %+v", response)
	}
	followup, err := translate.EncodeResponsesRequest(ir.Request{Model: "fixture-model", Items: response.Items, Tools: func() []ir.ToolDef {
		req, decodeErr := translate.DecodeResponsesRequest([]byte(`{"model":"fixture-model","input":[],"tools":` + tools + `,"store":false}`))
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return req.Tools
	}(), Responses: &ir.ResponsesControls{}, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	status, body = responseRequest(t, b, string(followup))
	if status != 200 {
		t.Fatalf("search followup status=%d body=%s", status, body)
	}
	var events []ir.ResponsesStreamEvent
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			event, err := translate.DecodeResponsesStreamEvent([]byte(strings.TrimPrefix(line, "data: ")))
			if err != nil {
				t.Fatalf("native search SSE=%s error=%v", line, err)
			}
			events = append(events, event)
		}
	}
	if len(events) != 9 || events[2].Kind != "response.web_search_call.in_progress" || events[3].Kind != "response.web_search_call.searching" || events[4].Kind != "response.web_search_call.completed" || events[5].Item.WebSearchAction.Kind != "open_page" || events[6].Annotation == nil || events[6].Annotation.URL != "https://example.com" || events[7].Item.Parts[0].Annotations[0].Title != "Example" || events[8].Kind != "response.completed" {
		t.Fatalf("native search SSE semantics=%+v", events)
	}
	rows := waitEvents(t, sink, 2)
	if len(rows) != 2 || rows[0].TokensIn != 1 || rows[1].TokensIn != 2 || calls.Load() != 2 {
		t.Fatalf("native exchanges/cost calls=%d rows=%+v", calls.Load(), rows)
	}
}

func TestGatewayResponsesChatRefusesNativeToolFamilies(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(ts.Close)
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "native-chat-refusal", "key", ts.URL, false)
	for _, body := range []string{
		`{"model":"fixture-model","input":[],"tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f","parameters":{}}]}],"store":false}`,
		`{"model":"fixture-model","input":[],"tools":[{"type":"web_search","external_web_access":true}],"store":false}`,
		`{"model":"fixture-model","input":[],"tools":[{"type":"function","name":"f","parameters":{},"defer_loading":false}],"store":false}`,
		`{"model":"fixture-model","input":[{"type":"web_search_call","id":"ws","status":"completed","action":{"type":"search","query":"q"}}],"store":false}`,
		`{"model":"fixture-model","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fact","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}]}],"store":false}`,
	} {
		status, _ := responseRequest(t, b, body)
		if status != 400 {
			t.Fatalf("Chat bridge native semantics refusal status=%d", status)
		}
	}
	if calls.Load() != 0 || len(sink.Events()) != 0 {
		t.Fatalf("native tool semantics dialed/metered Chat: calls=%d rows=%v", calls.Load(), sink.Events())
	}
}
