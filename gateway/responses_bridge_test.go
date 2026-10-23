package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/gateway/costfeed"
)

// Documented OpenAI Chat Completions shapes, with the envelope metadata and
// empty fields real backends send on every reply.
const (
	realChatUsage = `"usage":{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`
	realChatEnv   = `"id":"chatcmpl-fixture","created":1741569952,"model":"fixture-model","service_tier":"default","system_fingerprint":"fp_fixture"`
)

func realChatReply(message, finish string) string {
	return `{` + realChatEnv + `,"object":"chat.completion","choices":[{"index":0,"message":` + message + `,"logprobs":null,"finish_reason":"` + finish + `"}],` + realChatUsage + `}`
}

func realChatChunk(delta, finish string) string {
	if finish != "null" {
		finish = `"` + finish + `"`
	}
	return `data: {` + realChatEnv + `,"object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `,"logprobs":null,"finish_reason":` + finish + `}],"usage":null,"obfuscation":"pad"}` + "\n\n"
}

// realChatStream ends with the include_usage chunk (empty choices) and [DONE].
func realChatStream(chunks ...string) string {
	return strings.Join(chunks, "") + `data: {` + realChatEnv + `,"object":"chat.completion.chunk","choices":[],` + realChatUsage + `}` + "\n\n" + "data: [DONE]\n\n"
}

// replyServer answers the n-th request with replies[n] (the last one repeats)
// and counts the dials.
func replyServer(t *testing.T, replies ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		body := replies[n]
		if strings.HasPrefix(body, "data: ") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, &calls
}

// terminalEvent returns the type and response status of the last SSE event.
func terminalEvent(t *testing.T, body string) (kind, status string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("event %s: %v", line, err)
		}
		kind, status = ev.Type, ev.Response.Status
	}
	return kind, status
}

const userInput = `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"store":false`

func TestGatewayResponsesRealOpenAIChatReplies(t *testing.T) {
	toolMessage := `{"role":"assistant","content":null,"refusal":null,"annotations":[],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]}`
	for _, tc := range []struct {
		name, reply, want string
		stream            bool
	}{
		{"text", realChatReply(`{"role":"assistant","content":"Hello there","refusal":null,"annotations":[]}`, "stop"), "Hello there", false},
		{"tool_call", realChatReply(toolMessage, "tool_calls"), `"call_id":"call_1"`, false},
		{"text_stream", realChatStream(
			realChatChunk(`{"role":"assistant","content":"","refusal":null}`, "null"),
			realChatChunk(`{"content":"Hello there"}`, "null"),
			realChatChunk(`{}`, "stop"),
		), `"text":"Hello there"`, true},
		{"tool_call_stream", realChatStream(
			realChatChunk(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}`, "null"),
			realChatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a\"}"}}]}`, "null"),
			realChatChunk(`{}`, "tool_calls"),
		), `"call_id":"call_1"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := replyServer(t, tc.reply)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "real-chat", "key", ts.URL, false)
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":`+boolStr(tc.stream)+`}`)
			if status != http.StatusOK || !strings.Contains(body, tc.want) {
				t.Fatalf("documented Chat reply status=%d body=%s", status, body)
			}
			if tc.stream {
				if kind, st := terminalEvent(t, body); kind != "response.completed" || st != "completed" {
					t.Fatalf("terminal=%s/%s body=%s", kind, st, body)
				}
			} else if !strings.Contains(body, `"status":"completed"`) {
				t.Fatalf("reply not completed: %s", body)
			}
			rows := waitEvents(t, sink, 1)
			if len(rows) != 1 || rows[0].TokensIn != 19 || rows[0].TokensOut != 10 {
				t.Fatalf("documented Chat reply meter=%+v", rows)
			}
		})
	}
}

// TestGatewayResponsesRefusedReplyKeepsCredentialHealthy refuses one healthy
// 2xx reply the codec cannot represent, then sends a valid request on the same
// session: the session's only credential must not have been cooled down.
func TestGatewayResponsesRefusedReplyKeepsCredentialHealthy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		native        bool
		refused, good string
	}{
		{"chat", false, realChatReply(`{"role":"assistant","content":null,"refusal":"I cannot help with that"}`, "stop"), realChatReply(`{"role":"assistant","content":"fine"}`, "stop")},
		{"native", true, `{"id":"r1","status":"completed","output":[{"type":"tool_search_call"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, `{"id":"r2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fine"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, calls := replyServer(t, tc.refused, tc.good)
			g := startGateway(t, &costfeed.MemorySink{})
			b := bindResponses(t, g, "healthy-key", "key", ts.URL, tc.native)
			if status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`}`); status != http.StatusBadGateway {
				t.Fatalf("refused reply status=%d body=%s", status, body)
			}
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`}`)
			if status != http.StatusOK || !strings.Contains(body, "fine") || calls.Load() != 2 {
				t.Fatalf("second request status=%d calls=%d body=%s", status, calls.Load(), body)
			}
		})
	}
}

func TestGateway_ResponsesIncompleteLength(t *testing.T) {
	for _, tc := range []struct{ finish, reason string }{
		{"length", "max_output_tokens"},
		{"content_filter", "content_filter"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.finish+"/stream="+boolStr(stream), func(t *testing.T) {
				reply := realChatReply(`{"role":"assistant","content":"partial","refusal":null}`, tc.finish)
				if stream {
					reply = realChatStream(realChatChunk(`{"role":"assistant","content":"partial"}`, "null"), realChatChunk(`{}`, tc.finish))
				}
				ts, _ := replyServer(t, reply)
				sink := &costfeed.MemorySink{}
				g := startGateway(t, sink)
				b := bindResponses(t, g, "incomplete", "key", ts.URL, false)
				status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":`+boolStr(stream)+`}`)
				if status != http.StatusOK || !strings.Contains(body, `"reason":"`+tc.reason+`"`) || strings.Contains(body, `"type":"response.completed"`) {
					t.Fatalf("%s finish status=%d body=%s", tc.finish, status, body)
				}
				if stream {
					if kind, st := terminalEvent(t, body); kind != "response.incomplete" || st != "incomplete" {
						t.Fatalf("terminal=%s/%s body=%s", kind, st, body)
					}
				} else {
					var reply struct {
						Status string `json:"status"`
					}
					if err := json.Unmarshal([]byte(body), &reply); err != nil || reply.Status != "incomplete" {
						t.Fatalf("reply status=%q err=%v body=%s", reply.Status, err, body)
					}
				}
				rows := waitEvents(t, sink, 1)
				if len(rows) != 1 || rows[0].TokensIn != 19 || rows[0].TokensOut != 10 {
					t.Fatalf("%s finish meter=%+v", tc.finish, rows)
				}
			})
		}
	}
}

// TestGateway_ResponsesTerminalMatchesStatus drives each Chat finish through
// the gateway and pins the terminal event to the snapshot status: a turn that
// ended is completed or incomplete and metered; a truncated stream fails and
// is never metered.
func TestGateway_ResponsesTerminalMatchesStatus(t *testing.T) {
	for _, tc := range []struct {
		name, wire, status string
		meter              int
	}{
		{"stop", realChatStream(realChatChunk(`{"role":"assistant","content":"done"}`, "null"), realChatChunk(`{}`, "stop")), "completed", 1},
		{"length", realChatStream(realChatChunk(`{"role":"assistant","content":"cut"}`, "null"), realChatChunk(`{}`, "length")), "incomplete", 1},
		{"content_filter", realChatStream(realChatChunk(`{"role":"assistant","content":"cut"}`, "null"), realChatChunk(`{}`, "content_filter")), "incomplete", 1},
		{"truncated", realChatChunk(`{"role":"assistant","content":"cut"}`, "null"), "failed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := replyServer(t, tc.wire)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "terminal", "key", ts.URL, false)
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":true}`)
			kind, st := terminalEvent(t, body)
			if status != http.StatusOK || st != tc.status || kind != "response."+tc.status {
				t.Fatalf("terminal=%s/%s want %s: %s", kind, st, tc.status, body)
			}
			if tc.meter == 0 {
				if rows := sink.Events(); len(rows) != 0 {
					t.Fatalf("truncated stream metered %+v", rows)
				}
				return
			}
			if rows := waitEvents(t, sink, tc.meter); len(rows) != tc.meter {
				t.Fatalf("meter=%+v", rows)
			}
		})
	}
}

// TestGatewayResponsesTruncatedChatStreamNeverCompletes reports usage on an
// intermediate chunk and then ends without a finish: the turn was cut off, so
// it is never served as completed and never metered.
func TestGatewayResponsesTruncatedChatStreamNeverCompletes(t *testing.T) {
	ts, _ := replyServer(t, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "truncated", "key", ts.URL, false)
	_, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":true}`)
	if kind, _ := terminalEvent(t, body); kind != "response.failed" || strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("truncated stream terminal=%s body=%s", kind, body)
	}
	if rows := sink.Events(); len(rows) != 0 {
		t.Fatalf("truncated stream metered %+v", rows)
	}
}

// TestGatewayResponsesDriftedToolIdentityNeverCompletes sends a second tool
// fragment that names a different call: it is never merged into the first
// call, the stream fails, and the finished upstream turn is still metered once.
func TestGatewayResponsesDriftedToolIdentityNeverCompletes(t *testing.T) {
	ts, _ := replyServer(t, realChatStream(
		realChatChunk(`{"role":"assistant","tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"{"}}]}`, "null"),
		realChatChunk(`{"tool_calls":[{"index":0,"id":"b","type":"function","function":{"name":"g","arguments":"}"}}]}`, "null"),
		realChatChunk(`{}`, "tool_calls"),
	))
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "drift", "key", ts.URL, false)
	_, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":true}`)
	if kind, _ := terminalEvent(t, body); kind != "response.failed" || strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, `"name":"g"`) {
		t.Fatalf("drifted tool identity terminal=%s body=%s", kind, body)
	}
	if rows := waitEvents(t, sink, 1); len(rows) != 1 {
		t.Fatalf("finished upstream turn meter=%+v", rows)
	}
}

// TestGatewayResponsesNativeDuplicateCreatedFails starts a native stream twice:
// the second creation is a lifecycle violation, never a completed response,
// and the turn the backend still finished and billed is metered once.
func TestGatewayResponsesNativeDuplicateCreatedFails(t *testing.T) {
	ts, _ := replyServer(t, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
		`data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
		`data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "duplicate", "key", ts.URL, true)
	_, body := responseRequest(t, b, `{"model":"fixture-model","input":[],"stream":true,"store":false}`)
	if kind, _ := terminalEvent(t, body); kind != "response.failed" || strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("duplicate creation terminal=%s body=%s", kind, body)
	}
	if rows := waitEvents(t, sink, 1); len(rows) != 1 || rows[0].TokensIn != 1 || rows[0].TokensOut != 1 {
		t.Fatalf("billed duplicate-creation turn meter=%+v", rows)
	}
}

// TestGatewayResponsesNativeStreamWrongContentType answers a stream request
// with a JSON body: the exchange is a 502, not a stream.
func TestGatewayResponsesNativeStreamWrongContentType(t *testing.T) {
	ts, _ := replyServer(t, `{"id":"r","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	g := startGateway(t, &costfeed.MemorySink{})
	b := bindResponses(t, g, "content-type", "key", ts.URL, true)
	if status, body := responseRequest(t, b, `{"model":"fixture-model","input":[],"stream":true,"store":false}`); status != http.StatusBadGateway {
		t.Fatalf("JSON for a stream status=%d body=%s", status, body)
	}
}

// Real-shape replies from OpenAI-compatible aggregators: each carries its own
// envelope and accounting extensions on top of the documented shape.
const (
	openRouterUsage = `"usage":{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29,"cost":0.000123,"is_byok":false,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.00005,"upstream_inference_completions_cost":0.00007},"completion_tokens_details":{"reasoning_tokens":0,"image_tokens":0}}`
	groqUsage       = `"usage":{"queue_time":0.037,"prompt_tokens":19,"prompt_time":0.0007,"completion_tokens":10,"completion_time":0.02,"total_tokens":29,"total_time":0.0207}`
	azureFilters    = `"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"protected_material_code":{"filtered":false,"detected":false},"protected_material_text":{"filtered":false,"detected":false},"self_harm":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"}}`
	azurePrompt     = `"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"jailbreak":{"filtered":false,"detected":false},"self_harm":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"}}}]`
)

func TestGatewayResponsesAggregatorChatReplies(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		stream     bool
	}{
		{"openrouter", `{"id":"gen-1","provider":"Fixture","model":"vendor/fixture-model","object":"chat.completion","created":1,"choices":[{"logprobs":null,"finish_reason":"stop","native_finish_reason":"stop","index":0,"message":{"role":"assistant","content":"Hello there","refusal":null,"reasoning":null}}],"system_fingerprint":"fp_1",` + openRouterUsage + `}`, false},
		{"openrouter_stream", ": OPENROUTER PROCESSING\n\n" +
			`data: {"id":"gen-1","provider":"Fixture","model":"vendor/fixture-model","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello there","reasoning":null},"finish_reason":null,"native_finish_reason":null,"logprobs":null}]}` + "\n\n" +
			`data: {"id":"gen-1","provider":"Fixture","model":"vendor/fixture-model","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop","native_finish_reason":"stop","logprobs":null}]}` + "\n\n" +
			`data: {"id":"gen-1","provider":"Fixture","model":"vendor/fixture-model","object":"chat.completion.chunk","created":1,"choices":[],` + openRouterUsage + `}` + "\n\n" +
			"data: [DONE]\n\n", true},
		{"groq", `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"logprobs":null,"finish_reason":"stop"}],` + groqUsage + `,"usage_breakdown":null,"system_fingerprint":"fp_1","x_groq":{"id":"req_1"},"service_tier":"on_demand"}`, false},
		{"groq_stream", `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"fixture-model","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}],"x_groq":{"id":"req_1"}}` + "\n\n" +
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"fixture-model","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"content":"Hello there"},"logprobs":null,"finish_reason":null}]}` + "\n\n" +
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"fixture-model","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}],"x_groq":{"id":"req_1","usage":{"queue_time":0.03,"prompt_tokens":19,"completion_tokens":10,"total_tokens":29}}}` + "\n\n" +
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"fixture-model","choices":[],` + groqUsage + `}` + "\n\n" +
			"data: [DONE]\n\n", true},
		{"azure", `{"choices":[{` + azureFilters + `,"finish_reason":"stop","index":0,"logprobs":null,"message":{"annotations":[],"content":"Hello there","refusal":null,"role":"assistant"}}],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion",` + azurePrompt + `,"system_fingerprint":"fp_1",` + realChatUsage + `}`, false},
		{"azure_stream", `data: {"choices":[],"created":0,"id":"","model":"","object":"",` + azurePrompt + `}` + "\n\n" +
			`data: {"choices":[{"content_filter_results":{},"delta":{"content":"","refusal":null,"role":"assistant"},"finish_reason":null,"index":0,"logprobs":null}],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion.chunk","system_fingerprint":"fp_1"}` + "\n\n" +
			`data: {"choices":[{` + azureFilters + `,"delta":{"content":"Hello there"},"finish_reason":null,"index":0,"logprobs":null}],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion.chunk","system_fingerprint":"fp_1"}` + "\n\n" +
			`data: {"choices":[{"content_filter_results":{},"delta":{},"finish_reason":"stop","index":0,"logprobs":null}],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion.chunk","system_fingerprint":"fp_1"}` + "\n\n" +
			`data: {"choices":[],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion.chunk","system_fingerprint":"fp_1",` + realChatUsage + `}` + "\n\n" +
			"data: [DONE]\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := replyServer(t, tc.wire)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "aggregator", "key", ts.URL, false)
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":`+boolStr(tc.stream)+`}`)
			if status != http.StatusOK || !strings.Contains(body, "Hello there") {
				t.Fatalf("%s reply status=%d body=%s", tc.name, status, body)
			}
			if tc.stream {
				if kind, st := terminalEvent(t, body); kind != "response.completed" || st != "completed" {
					t.Fatalf("terminal=%s/%s body=%s", kind, st, body)
				}
			} else if !strings.Contains(body, `"status":"completed"`) {
				t.Fatalf("reply not completed: %s", body)
			}
			rows := waitEvents(t, sink, 1)
			if len(rows) != 1 || rows[0].TokensIn != 19 || rows[0].TokensOut != 10 {
				t.Fatalf("%s meter=%+v", tc.name, rows)
			}
		})
	}
}

// TestGatewayResponsesAzureFilteredOutputIsIncomplete serves output an Azure
// content filter blocked as the content_filter incomplete it is, and meters it.
func TestGatewayResponsesAzureFilteredOutputIsIncomplete(t *testing.T) {
	ts, _ := replyServer(t, `{"choices":[{"content_filter_results":{"violence":{"filtered":true,"severity":"high"}},"finish_reason":"content_filter","index":0,"logprobs":null,"message":{"content":null,"refusal":null,"role":"assistant"}}],"created":1,"id":"chatcmpl-1","model":"fixture-model","object":"chat.completion",`+azurePrompt+`,`+realChatUsage+`}`)
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindResponses(t, g, "azure-filtered", "key", ts.URL, false)
	status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`}`)
	if status != http.StatusOK || !strings.Contains(body, `"status":"incomplete"`) || !strings.Contains(body, `"reason":"content_filter"`) {
		t.Fatalf("filtered output status=%d body=%s", status, body)
	}
	if rows := waitEvents(t, sink, 1); len(rows) != 1 {
		t.Fatalf("filtered output meter=%+v", rows)
	}
}

// TestGatewayResponsesRefusedBilledTurnIsMetered refuses replies below the
// surface that still finished a turn and reported usage: the client gets a
// failure, and the billed turn is recorded exactly once. A refusal with no
// trustworthy usage records nothing.
func TestGatewayResponsesRefusedBilledTurnIsMetered(t *testing.T) {
	nativeStream := func(events ...string) string {
		var b strings.Builder
		for _, ev := range events {
			b.WriteString("data: " + ev + "\n\n")
		}
		return b.String()
	}
	for _, tc := range []struct {
		name   string
		native bool
		stream bool
		wire   string
		rows   int
	}{
		{"chat_refusal_text", false, false, realChatReply(`{"role":"assistant","content":null,"refusal":"I cannot help with that"}`, "stop"), 1},
		{"chat_annotations", false, false, realChatReply(`{"role":"assistant","content":"fact","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.com","title":"Example","start_index":0,"end_index":4}}]}`, "stop"), 1},
		{"chat_stream_refusal_text", false, true, realChatStream(realChatChunk(`{"role":"assistant","content":null}`, "null"), realChatChunk(`{"refusal":"I cannot help"}`, "null"), realChatChunk(`{}`, "stop")), 1},
		{"chat_refused_without_usage", false, false, `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"stop"}]}`, 0},
		{"chat_stream_refused_unfinished", false, true, realChatChunk(`{"role":"assistant","refusal":"no"}`, "null") + `data: {"choices":[],` + realChatUsage + `}` + "\n\n", 0},
		{"native_stream_refused_event", true, true, nativeStream(
			`{"type":"response.created","response":{"id":"r"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"tool_search_call","id":"ts"}}`,
			`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":19,"output_tokens":10,"total_tokens":29}}}`), 1},
		{"native_stream_failed_with_usage", true, true, nativeStream(
			`{"type":"response.created","response":{"id":"r"}}`,
			`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"x"},"usage":{"input_tokens":19,"output_tokens":10,"total_tokens":29}}}`), 1},
		{"native_stream_other_response", true, true, nativeStream(
			`{"type":"response.created","response":{"id":"r"}}`,
			`{"type":"response.created","response":{"id":"other"}}`,
			`{"type":"response.completed","response":{"id":"other","usage":{"input_tokens":19,"output_tokens":10,"total_tokens":29}}}`), 0},
		{"native_failed_reply_with_usage", true, false, `{"id":"r","object":"response","status":"failed","error":{"code":"server_error","message":"x"},"output":[],"usage":{"input_tokens":19,"output_tokens":10,"total_tokens":29}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := replyServer(t, tc.wire)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "refused-billed", "key", ts.URL, tc.native)
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"stream":`+boolStr(tc.stream)+`}`)
			if strings.Contains(body, `"status":"completed"`) && !tc.stream || tc.stream && strings.Contains(body, `"type":"response.completed"`) {
				t.Fatalf("refused reply served as completed: %s", body)
			}
			if !tc.stream && status != http.StatusBadGateway {
				t.Fatalf("refused reply status=%d body=%s", status, body)
			}
			if tc.stream {
				if kind, _ := terminalEvent(t, body); kind != "response.failed" {
					t.Fatalf("refused stream terminal=%s body=%s", kind, body)
				}
			}
			if tc.rows == 0 {
				if rows := sink.Events(); len(rows) != 0 {
					t.Fatalf("unbilled refusal metered %+v", rows)
				}
				return
			}
			rows := waitEvents(t, sink, tc.rows)
			if len(rows) != tc.rows || rows[0].TokensIn != 19 || rows[0].TokensOut != 10 {
				t.Fatalf("billed refusal meter=%+v", rows)
			}
		})
	}
}

// TestGatewayResponsesNativeIncompleteIsServedAndMetered serves a native turn
// the backend cut short as incomplete, never completed or failed, and meters it.
func TestGatewayResponsesNativeIncompleteIsServedAndMetered(t *testing.T) {
	const snapshot = `{"id":"r","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","id":"m","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":19,"output_tokens":10,"total_tokens":29}}`
	for _, tc := range []struct {
		name, wire string
		stream     bool
	}{
		{"reply", snapshot, false},
		{"stream", `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"cut"}` + "\n\n" +
			`data: {"type":"response.incomplete","response":` + snapshot + `}` + "\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := replyServer(t, tc.wire)
			sink := &costfeed.MemorySink{}
			g := startGateway(t, sink)
			b := bindResponses(t, g, "native-incomplete", "key", ts.URL, true)
			status, body := responseRequest(t, b, `{"model":"fixture-model",`+userInput+`,"max_output_tokens":10,"stream":`+boolStr(tc.stream)+`}`)
			if status != http.StatusOK || !strings.Contains(body, `"reason":"max_output_tokens"`) || strings.Contains(body, `"type":"response.completed"`) {
				t.Fatalf("native incomplete status=%d body=%s", status, body)
			}
			if tc.stream {
				if kind, st := terminalEvent(t, body); kind != "response.incomplete" || st != "incomplete" {
					t.Fatalf("terminal=%s/%s body=%s", kind, st, body)
				}
			} else {
				var reply struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal([]byte(body), &reply); err != nil || reply.Status != "incomplete" {
					t.Fatalf("reply status=%q err=%v", reply.Status, err)
				}
			}
			rows := waitEvents(t, sink, 1)
			if len(rows) != 1 || rows[0].TokensIn != 19 || rows[0].TokensOut != 10 {
				t.Fatalf("native incomplete meter=%+v", rows)
			}
		})
	}
}
