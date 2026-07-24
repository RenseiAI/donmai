package surface

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/token"
)

// fakeDispatcher lets the surface be exercised without a live gateway.
type fakeDispatcher struct {
	outcome Outcome
	err     error
	gotTok  token.Token
	gotReq  ir.Request
}

func (f *fakeDispatcher) Dispatch(_ context.Context, tok token.Token, req ir.Request) (Outcome, error) {
	f.gotTok = tok
	f.gotReq = req
	return f.outcome, f.err
}

func post(t *testing.T, h http.Handler, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	h := NewOpenAIChat(&fakeDispatcher{})
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestServeHTTP_MissingToken(t *testing.T) {
	h := NewOpenAIChat(&fakeDispatcher{})
	rec := post(t, h, "", `{"model":"m","messages":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}
}

func TestServeHTTP_MalformedBody(t *testing.T) {
	h := NewOpenAIChat(&fakeDispatcher{})
	rec := post(t, h, "Bearer t", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", rec.Code)
	}
}

func TestServeHTTP_DispatchHTTPError(t *testing.T) {
	fd := &fakeDispatcher{err: &HTTPError{Status: http.StatusServiceUnavailable, Code: "no_credential", Message: "none"}}
	h := NewOpenAIChat(fd)
	rec := post(t, h, "Bearer t", `{"model":"m","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if env.Error.Code != "no_credential" {
		t.Errorf("error code = %q, want no_credential", env.Error.Code)
	}
}

func TestServeHTTP_NonStreamingMeters(t *testing.T) {
	var metered bool
	fd := &fakeDispatcher{outcome: Outcome{
		Response: &ir.Response{Model: "m", Content: []ir.Part{{Kind: ir.PartText, Text: "hi"}}, FinishReason: ir.FinishStop, Usage: ir.Usage{TokensIn: 2, TokensOut: 1}},
		Model:    "m",
		Meter:    func(_ ir.Usage, _ ir.FinishReason) { metered = true },
	}}
	h := NewOpenAIChat(fd)
	rec := post(t, h, "Bearer tok", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
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
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Choices[0].Message.Content != "hi" {
		t.Errorf("content = %q, want hi", resp.Choices[0].Message.Content)
	}
}

func TestServeHTTP_StreamingMetersOnce(t *testing.T) {
	deltas := make(chan ir.StreamDelta, 3)
	deltas <- ir.StreamDelta{TextDelta: "a"}
	deltas <- ir.StreamDelta{TextDelta: "b", Finish: ir.FinishStop, Usage: &ir.Usage{TokensIn: 4, TokensOut: 2}}
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
	h := NewOpenAIChat(fd)
	rec := post(t, h, "Bearer tok", `{"model":"m","messages":[],"stream":true}`)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want SSE", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Error("stream missing [DONE]")
	}
	if meterCalls != 1 {
		t.Fatalf("meter called %d times, want exactly 1", meterCalls)
	}
	if meteredUsage.TokensIn != 4 || meteredUsage.TokensOut != 2 {
		t.Errorf("metered usage = %+v, want 4/2 from terminal delta", meteredUsage)
	}
}
