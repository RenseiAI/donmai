package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
