package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
)

func TestOpenAIResponsesNativeRequestAndResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-key" || !strings.Contains(string(body), `"call_id":"call1"`) || !strings.Contains(string(body), `"output":"result"`) || !strings.Contains(string(body), `"store":false`) {
			t.Errorf("native request path=%s auth=%s body=%s", r.URL.Path, r.Header.Get("Authorization"), body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"response1","status":"completed","model":"m","output":[{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"opaque"},{"type":"custom_tool_call","id":"ctc","call_id":"custom1","name":"shell","input":"echo ok"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	}))
	t.Cleanup(ts.Close)
	u := &OpenAIResponses{OpenAICompat: OpenAICompat{Company: "openai", BaseURL: ts.URL + "/v1"}}
	out, err := u.Invoke(context.Background(), ir.Request{Model: "m", Responses: &ir.ResponsesControls{}, Items: []ir.Item{{Kind: "function_call_output", CallID: "call1", Output: "result"}}}, pool.Credential{Secret: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Response == nil || out.Response.ID != "response1" || out.Response.Items[0].EncryptedContent != "opaque" || out.Response.Items[1].Input != "echo ok" || out.Response.FinishReason != ir.FinishToolCalls {
		t.Fatalf("native outcome=%+v", out.Response)
	}
}

func TestOpenAIResponsesStreamTerminals(t *testing.T) {
	created := `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n"
	complete := `data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
	cases := []struct {
		name, wire string
		wantError  bool
	}{
		{"completed", created + `event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" + complete, false},
		{"failed", created + `data: {"type":"response.failed","response":{"id":"r","error":{"message":"private upstream"}}}` + "\n\n", true},
		{"incomplete", created + `data: {"type":"response.incomplete","response":{"id":"r"}}` + "\n\n", true},
		{"EOF", created, true},
		{"done_without_completion", created + "data: [DONE]\n\n", true},
		{"missing_created", complete, true},
		{"wrong_response_id", created + strings.ReplaceAll(complete, `"id":"r"`, `"id":"other"`), true},
		{"event_name_mismatch", created + "event: response.output_item.done\n" + complete, true},
		{"oversized_event", created + "data: " + strings.Repeat("x", (1<<20)+1) + "\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			}))
			t.Cleanup(ts.Close)
			u := &OpenAIResponses{OpenAICompat: OpenAICompat{BaseURL: ts.URL}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			t.Cleanup(cancel)
			out, err := u.Invoke(ctx, ir.Request{Model: "m", Responses: &ir.ResponsesControls{}, Stream: true}, pool.Credential{Secret: "key"})
			if err != nil {
				t.Fatal(err)
			}
			var terminal bool
			for d := range out.Stream.Deltas {
				if d.Responses.Kind == "response.completed" {
					terminal = true
				}
			}
			if got := out.Stream.Err(); (got != nil) != tc.wantError || terminal == tc.wantError {
				t.Fatalf("terminal=%v err=%v wantError=%v", terminal, got, tc.wantError)
			}
		})
	}
}

func TestOpenAIResponsesStreamCancellation(t *testing.T) {
	observed := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(observed)
	}))
	t.Cleanup(ts.Close)
	u := &OpenAIResponses{OpenAICompat: OpenAICompat{BaseURL: ts.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	out, err := u.Invoke(ctx, ir.Request{Model: "m", Responses: &ir.ResponsesControls{}, Stream: true}, pool.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	<-out.Stream.Deltas
	cancel()
	for d := range out.Stream.Deltas {
		if d.Responses.Kind == "response.completed" {
			t.Fatal("cancel fabricated completion")
		}
	}
	if out.Stream.Err() == nil {
		t.Fatal("cancel returned success")
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("upstream context not canceled")
	}
}

func TestOpenAICompatResponsesRefusesLoss(t *testing.T) {
	var invoked bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		invoked = true
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hello","refusal":"unsupported"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(ts.Close)
	u := &OpenAICompat{BaseURL: ts.URL}
	_, err := u.Invoke(context.Background(), ir.Request{Model: "m", Responses: &ir.ResponsesControls{Include: []string{"reasoning.encrypted_content"}}}, pool.Credential{})
	if err == nil || invoked {
		t.Fatal("unrepresentable request dialed Chat")
	}
	_, err = u.Invoke(context.Background(), ir.Request{Model: "m", Responses: &ir.ResponsesControls{}}, pool.Credential{})
	if err == nil {
		t.Fatal("Chat response refusal silently dropped")
	}
}

// TestStreamSendHonorsCancellation stops reading after the first delta and
// cancels: both stream readers must abandon a blocked send and report the
// cancellation, never park their goroutine on a consumer that has gone away.
func TestStreamSendHonorsCancellation(t *testing.T) {
	chat := strings.Repeat(`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`+"\n\n", 4)
	native := `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n" +
		strings.Repeat(`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"m","delta":"x"}`+"\n\n", 4)
	for _, tc := range []struct {
		name string
		wire string
		up   func(string) Upstream
		req  ir.Request
	}{
		{"chat", chat, func(url string) Upstream { return &OpenAICompat{BaseURL: url} }, ir.Request{Model: "m", Stream: true}},
		{"responses_bridge", chat, func(url string) Upstream { return &OpenAICompat{BaseURL: url} }, ir.Request{Model: "m", Stream: true, Responses: &ir.ResponsesControls{}}},
		{"native", native, func(url string) Upstream { return &OpenAIResponses{OpenAICompat: OpenAICompat{BaseURL: url}} }, ir.Request{Model: "m", Stream: true, Responses: &ir.ResponsesControls{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			out, err := tc.up(ts.URL).Invoke(ctx, tc.req, pool.Credential{})
			if err != nil {
				t.Fatal(err)
			}
			<-out.Stream.Deltas
			cancel()
			errc := make(chan error, 1)
			go func() { errc <- out.Stream.Err() }()
			select {
			case err := <-errc:
				if err == nil {
					t.Fatal("cancelled stream reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("stream reader ignored cancellation while blocked on send")
			}
		})
	}
}
