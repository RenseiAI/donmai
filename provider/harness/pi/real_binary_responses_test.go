package pi

// real_binary_responses_test.go — real `pi` binary evidence for the injected
// provider's Responses lane: a binding on agent.ProtoOpenAIResponses makes pi
// send its session id as prompt_cache_key to a host that is not OpenAI's,
// and the cached-token count the endpoint reports reaches the event mapper as
// the cache-read bucket. Same CI scope as real_binary_test.go: it runs where
// `pi` and `node` are on PATH and skips cleanly elsewhere.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// The usage the Responses stub reports on every response: input_tokens
// includes the cached tokens (OpenAI accounting), which pi subtracts.
const (
	responsesStubInputTokens  = 5000
	responsesStubCachedTokens = 4096
	responsesStubOutputTokens = 7
)

// responsesStubRequest is one recorded /v1/responses request.
type responsesStubRequest struct {
	Model          string
	PromptCacheKey string
	Stream         bool
}

// responsesStub is a local Responses-API endpoint that streams a short text
// reply with fixed usage and records every request.
type responsesStub struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []responsesStubRequest
}

func newResponsesStub(t *testing.T) *responsesStub {
	t.Helper()
	s := &responsesStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", s.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("responses stub: unsupported route"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *responsesStub) baseURL() string { return s.srv.URL + "/v1" }

func (s *responsesStub) recorded() []responsesStubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]responsesStubRequest(nil), s.requests...)
}

func (s *responsesStub) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model          string `json:"model"`
		PromptCacheKey string `json:"prompt_cache_key"`
		Stream         bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, responsesStubRequest{Model: body.Model, PromptCacheKey: body.PromptCacheKey, Stream: body.Stream})
	n := len(s.requests)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(payload map[string]any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", payload["type"], data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	id := fmt.Sprintf("resp_stub_%d", n)
	text := fmt.Sprintf("stub-responses-reply-%d", n)
	message := map[string]any{
		"type": "message", "id": fmt.Sprintf("msg_stub_%d", n), "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	send(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
	send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
		"type": "message", "id": message["id"], "role": "assistant", "status": "in_progress", "content": []any{},
	}})
	send(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": message["id"], "delta": text})
	send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message})
	send(map[string]any{"type": "response.completed", "response": map[string]any{
		"id": id, "status": "completed", "output": []any{message},
		"usage": map[string]any{
			"input_tokens":          responsesStubInputTokens,
			"output_tokens":         responsesStubOutputTokens,
			"total_tokens":          responsesStubInputTokens + responsesStubOutputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": responsesStubCachedTokens},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}})
}

// TestRealBinary_ResponsesLane_SendsSessionCacheKey drives two turns of a
// real pi session whose binding names the Responses protocol against a local
// endpoint (not an OpenAI host). Every request must carry prompt_cache_key,
// the same value on both turns, equal to pi's own session id — the key an
// aggregator such as the Vercel AI Gateway caches a session's prefix under.
// The endpoint's cached-token count must come back through the event mapper
// as the cache-read bucket, with input excluding it.
func TestRealBinary_ResponsesLane_SendsSessionCacheKey(t *testing.T) {
	realBinaryAvailable(t)

	stub := newResponsesStub(t)
	spec := realBinarySpec(t.TempDir(), "reply with a short greeting and nothing else", stub.baseURL())
	spec.Endpoint.Protocol = agent.ProtoOpenAIResponses

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	turns := [][]agent.Event{drainToResult(t, h, 30*time.Second)}
	if err := h.Inject(ctx, "reply once more"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	turns = append(turns, drainToResult(t, h, 30*time.Second))

	var calls []agent.LlmCallEvent
	var last *agent.ResultEvent
	for i, evs := range turns {
		var result *agent.ResultEvent
		for _, ev := range evs {
			switch e := ev.(type) {
			case agent.LlmCallEvent:
				calls = append(calls, e)
			case agent.ResultEvent:
				r := e
				result = &r
			case agent.ErrorEvent:
				t.Fatalf("turn %d ended with an ErrorEvent: %+v", i+1, e)
			}
		}
		if result == nil || !result.Success {
			t.Fatalf("turn %d: no successful ResultEvent (got %+v)", i+1, result)
		}
		last = result
	}

	requests := stub.recorded()
	if len(requests) != 2 {
		t.Fatalf("the Responses endpoint saw %d requests; want 2 (one per turn): %+v", len(requests), requests)
	}
	sessionID := h.SessionID()
	if sessionID == "" {
		t.Fatal("the handle resolved no pi session id")
	}
	for i, req := range requests {
		if req.Model != realBinaryModel || !req.Stream {
			t.Errorf("request %d: model=%q stream=%v; want the pinned model, streamed", i+1, req.Model, req.Stream)
		}
		if req.PromptCacheKey != sessionID {
			t.Errorf("request %d: prompt_cache_key = %q; want pi's session id %q on every request", i+1, req.PromptCacheKey, sessionID)
		}
	}

	if len(calls) != 2 {
		t.Fatalf("got %d LlmCallEvents; want 2: %+v", len(calls), calls)
	}
	for i, call := range calls {
		if call.InputTokens != responsesStubInputTokens-responsesStubCachedTokens ||
			call.CachedInputTokens != responsesStubCachedTokens || call.OutputTokens != responsesStubOutputTokens {
			t.Errorf("call %d: in=%d cached=%d out=%d; want in=%d cached=%d out=%d", i+1,
				call.InputTokens, call.CachedInputTokens, call.OutputTokens,
				responsesStubInputTokens-responsesStubCachedTokens, responsesStubCachedTokens, responsesStubOutputTokens)
		}
	}
	if last.Cost == nil || last.Cost.CachedInputTokens != 2*responsesStubCachedTokens ||
		last.Cost.InputTokens != 2*(responsesStubInputTokens-responsesStubCachedTokens) {
		t.Errorf("session Cost = %+v; want input %d and cache reads %d over both turns", last.Cost,
			2*(responsesStubInputTokens-responsesStubCachedTokens), 2*responsesStubCachedTokens)
	}
}
