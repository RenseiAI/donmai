package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
)

func TestOpenAICompat_Name(t *testing.T) {
	u := &OpenAICompat{Company: "openai"}
	if u.Name() != "openai" {
		t.Errorf("Name = %q, want openai", u.Name())
	}
}

func TestOpenAICompat_NonStreamingSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer ts.Close()

	u := &OpenAICompat{Company: "openai", BaseURL: ts.URL}
	out, err := u.Invoke(context.Background(), ir.Request{Model: "m"}, pool.Credential{Secret: "k"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.Status != 200 || out.Response == nil {
		t.Fatalf("outcome = %+v", out)
	}
	if len(out.Response.Content) != 1 || out.Response.Content[0].Text != "ok" {
		t.Errorf("response content = %+v", out.Response.Content)
	}
}

func TestOpenAICompat_ErrorStatusSurfaced(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer ts.Close()

	u := &OpenAICompat{Company: "openai", BaseURL: ts.URL}
	out, err := u.Invoke(context.Background(), ir.Request{Model: "m"}, pool.Credential{Secret: "k"})
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if out.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", out.Status)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *upstream.Error", err)
	}
	if ue.Status != http.StatusTooManyRequests {
		t.Errorf("upstream.Error.Status = %d, want 429", ue.Status)
	}
}

func TestOpenAICompat_CustomAuthHeader(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	u := &OpenAICompat{Company: "c", BaseURL: ts.URL, AuthHeader: "X-Api-Key"}
	if _, err := u.Invoke(context.Background(), ir.Request{Model: "m"}, pool.Credential{Secret: "secret-key"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if gotHeader != "secret-key" {
		t.Errorf("custom auth header = %q, want secret-key (no Bearer prefix)", gotHeader)
	}
}

func TestOpenAICompat_StreamingParsesSSE(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	u := &OpenAICompat{Company: "openai", BaseURL: ts.URL}
	out, err := u.Invoke(context.Background(), ir.Request{Model: "m", Stream: true}, pool.Credential{Secret: "k"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.Stream == nil {
		t.Fatal("expected a stream")
	}
	var text string
	var finish ir.FinishReason
	for d := range out.Stream.Deltas {
		text += d.TextDelta
		if d.Finish != "" {
			finish = d.Finish
		}
	}
	if err := out.Stream.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if text != "A" || finish != ir.FinishStop {
		t.Errorf("stream text=%q finish=%q, want A/stop", text, finish)
	}
}

// The failing shape: the replayed request carried an empty-thinking assistant
// turn (no text, no tool call) plus a ~46 KiB error tool result, and the
// upstream rejected it with a 400. Invoke must retry that 400 once with the
// offending parts sanitized — empty assistant dropped, tool result cut to the
// cap with the marker — and the turn continues instead of failing the session.
func TestOpenAICompat_Retry400WithSanitizedReplay(t *testing.T) {
	var bodies []string
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"The request contains invalid parameters","type":"AI_APICallError"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer ts.Close()

	big := strings.Repeat("x", (32<<10)+100)
	req := ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Parts: nil},
		{Role: ir.RoleAssistant, Parts: []ir.Part{
			{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "c1", Name: "bash", Arguments: "{}"}},
		}},
		{Role: ir.RoleTool, Parts: []ir.Part{
			{Kind: ir.PartToolResult, Text: big, ToolCallID: "c1"},
		}},
	}}
	u := &OpenAICompat{Company: "openai", BaseURL: ts.URL}
	out, err := u.Invoke(context.Background(), req, pool.Credential{Secret: "k"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.Status != 200 || out.Response == nil {
		t.Fatalf("outcome = %+v, want retried 200 with a response", out)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one 400, one sanitized retry)", calls)
	}
	var second struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("decode retried body: %v", err)
	}
	if len(second.Messages) != 2 {
		t.Fatalf("retried messages = %d, want 2 (empty assistant dropped, tool-call assistant + tool kept)", len(second.Messages))
	}
	for _, m := range second.Messages {
		if m.Role == "assistant" {
			// An assistant with neither content nor tool calls is the
			// replayed empty-thinking turn several upstreams reject with
			// a 400; a tool-call assistant (no content, has tool_calls)
			// is the pending call the tool result answers and must stay.
			content, _ := m.Content.(string)
			if content == "" && len(m.ToolCalls) == 0 {
				t.Fatalf("retried request still carries an empty assistant message: %+v", second.Messages)
			}
		}
		if m.Role == "tool" {
			s, _ := m.Content.(string)
			if len(s) > (32<<10)+256 || !strings.Contains(s, "truncated by gateway retry") {
				t.Fatalf("retried tool result not capped with marker (len=%d)", len(s))
			}
		}
	}
}

// A non-400 failure (here 429) must NOT retry: only a 400 carries the
// "invalid parameters on a replay" shape this path sanitizes.
func TestOpenAICompat_NoRetryOnNon400(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer ts.Close()

	req := ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Parts: nil},
	}}
	u := &OpenAICompat{Company: "openai", BaseURL: ts.URL}
	if _, err := u.Invoke(context.Background(), req, pool.Credential{Secret: "k"}); err == nil {
		t.Fatal("expected error on 429")
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry on non-400)", calls)
	}
}
