package surface

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/token"
)

type responsesDispatcher struct {
	out         Outcome
	err         error
	gotProtocol agent.WireProtocol
	gotRequest  ir.Request
	gotToken    token.Token
	called      bool
}

func (d *responsesDispatcher) DispatchProtocol(_ context.Context, tok token.Token, p agent.WireProtocol, r ir.Request) (Outcome, error) {
	d.called = true
	d.gotProtocol = p
	d.gotRequest = r
	d.gotToken = tok
	return d.out, d.err
}

func TestResponsesSurfaceStrictRequestsAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, auth, body string
		status           int
	}{
		{"missing_token", "", `{"model":"m","input":[]}`, 401},
		{"malformed", "Bearer t", "{bad}", 400},
		{"stateful", "Bearer t", `{"model":"m","input":[],"previous_response_id":"r"}`, 400},
		{"unknown", "Bearer t", `{"model":"m","input":[],"new_field":true}`, 400},
		{"oversized", "Bearer t", strings.Repeat(" ", (32<<20)+1), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &responsesDispatcher{}
			rec := post(t, NewOpenAIResponses(d), tc.auth, tc.body)
			if rec.Code != tc.status || d.called {
				t.Fatalf("code=%d called=%v", rec.Code, d.called)
			}
		})
	}
	d := &responsesDispatcher{out: Outcome{Response: &ir.Response{Model: "m", Content: []ir.Part{{Kind: ir.PartText, Text: "hello"}}, FinishReason: ir.FinishStop, UsageReported: true}}}
	h := NewOpenAIResponses(d)
	h.IDFn = func() string { return "response-test" }
	rec := post(t, h, "Bearer exact-token", `{"model":"m","input":[],"store":false}`)
	if rec.Code != 200 || d.gotProtocol != agent.ProtoOpenAIResponses || d.gotToken != "exact-token" || !strings.Contains(rec.Body.String(), `"id":"response-test"`) {
		t.Fatalf("protocol dispatch code=%d token=%s protocol=%s body=%s", rec.Code, d.gotToken, d.gotProtocol, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("method refusal=%d", rec.Code)
	}
}

func TestResponsesSurfaceStreamErrorAndCancelNeverMeter(t *testing.T) {
	usage := &ir.Usage{TokensIn: 1, TokensOut: 1}
	for _, mode := range []string{"terminal_error", "missing_terminal", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ch := make(chan ir.StreamDelta, 2)
			ch <- ir.StreamDelta{Responses: &ir.ResponsesStreamEvent{Kind: "response.created", ResponseID: "r"}}
			if mode == "terminal_error" {
				ch <- ir.StreamDelta{Responses: &ir.ResponsesStreamEvent{Kind: "response.completed", ResponseID: "r", Usage: usage}}
			}
			if mode != "cancel" {
				close(ch)
			}
			metered := 0
			out := Outcome{Stream: &ir.Stream{Deltas: ch, Err: func() error {
				if mode == "terminal_error" {
					return errors.New("private upstream error")
				}
				return nil
			}}, Meter: func(ir.Usage, ir.FinishReason) { metered++ }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":[],"stream":true}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			NewOpenAIResponses(&responsesDispatcher{out: out}).ServeHTTP(rec, req)
			if metered != 0 || strings.Contains(rec.Body.String(), `"type":"response.completed"`) || !strings.Contains(rec.Body.String(), `"type":"response.failed"`) {
				t.Fatalf("mode=%s meter=%d body=%s", mode, metered, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "private upstream") {
				t.Fatal("stream leaked upstream error")
			}
		})
	}
}

func TestResponsesSurfaceCanceledBlockedStreamReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan struct{})
	returned := make(chan struct{})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":[],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer t")
	ch := make(chan ir.StreamDelta)
	go func() {
		close(started)
		NewOpenAIResponses(&responsesDispatcher{out: Outcome{Stream: &ir.Stream{Deltas: ch}}}).ServeHTTP(httptest.NewRecorder(), req)
		close(returned)
	}()
	<-started
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("blocked stream did not honor cancellation")
	}
}

func TestResponsesSurfaceMissingUsageOrUnrepresentableOutput(t *testing.T) {
	for _, resp := range []ir.Response{{Content: []ir.Part{{Kind: ir.PartText, Text: "hello"}}}, {UsageReported: true, Content: []ir.Part{{Kind: ir.PartImage, ImageURL: "x"}}}, {UsageReported: true, FinishReason: ir.FinishLength}} {
		metered := 0
		d := &responsesDispatcher{out: Outcome{Response: &resp, Meter: func(ir.Usage, ir.FinishReason) { metered++ }}}
		rec := post(t, NewOpenAIResponses(d), "Bearer t", `{"model":"m","input":[]}`)
		if rec.Code != 502 || metered != 0 {
			t.Fatalf("unsupported output code=%d meter=%d", rec.Code, metered)
		}
	}
}
