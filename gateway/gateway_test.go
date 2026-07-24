package gateway_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/upstream"
)

// fakeUpstream is an httptest OpenAI-compatible /chat/completions endpoint. It
// asserts the Authorization bearer equals wantKey (proving the gateway dialed
// with THIS session's credential, not another's) and replies streaming or not
// per the request body.
func fakeUpstream(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantKey {
			t.Errorf("upstream got auth %q, want %q", got, "Bearer "+wantKey)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			chunks := []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			}
			for _, c := range chunks {
				_, _ = io.WriteString(w, "data: "+c+"\n\n")
				if fl != nil {
					fl.Flush()
				}
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"up-1","object":"chat.completion","model":"`+body.Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func startGateway(t *testing.T, sink costfeed.Sink) *gateway.Gateway {
	t.Helper()
	g := gateway.New(gateway.Options{Sink: sink})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	return g
}

func bindSession(t *testing.T, g *gateway.Gateway, sessionID, harness, upstreamKey, upstreamURL, model string) agent.EndpointBinding {
	t.Helper()
	b, err := g.Bind(gateway.BindConfig{
		SessionID: sessionID,
		Harness:   harness,
		Company:   agent.CompanyOpenAI,
		Model:     model,
		AuthMode:  agent.AuthMetered,
		Surface:   agent.ProtoOpenAIChat,
		Upstream:  &upstream.OpenAICompat{Company: "openai", BaseURL: upstreamURL},
		Source:    pool.SingleKey{Key: pool.Credential{ID: "k1", Secret: upstreamKey}},
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	return b
}

// chatRequest posts a chat-completions request to the gateway using the
// binding's baseURL and bearer token (the exact shape an OpenAI-compatible
// harness client emits).
func chatRequest(t *testing.T, b agent.EndpointBinding, stream bool) *http.Response {
	t.Helper()
	body := `{"model":"` + b.Model + `","messages":[{"role":"user","content":"hi"}],"stream":` + boolStr(stream) + `}`
	req, err := http.NewRequest(http.MethodPost, b.BaseURL+"/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestGateway_NonStreaming_RoundTripAndCostRow(t *testing.T) {
	up := fakeUpstream(t, "sk-upstream")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)

	b := bindSession(t, g, "sess-1", "opencode", "sk-upstream", up.URL, "gpt-4o")
	if !strings.HasPrefix(b.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("binding baseURL not loopback: %q", b.BaseURL)
	}
	if b.Env[gateway.TokenEnvVar] == "" {
		t.Fatal("binding carries no session token")
	}

	resp := chatRequest(t, b, false)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}

	// Smoke item 5: a cost ledger row per completion with the harness column.
	evs := waitEvents(t, sink, 1)
	ev := evs[0]
	if ev.Harness != "opencode" {
		t.Errorf("cost row harness = %q, want opencode", ev.Harness)
	}
	if ev.ProviderID != "openai" {
		t.Errorf("cost row providerId = %q, want openai", ev.ProviderID)
	}
	if ev.Host != costfeed.Host {
		t.Errorf("cost row host = %q, want gateway", ev.Host)
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("cost row sessionId = %q, want sess-1", ev.SessionID)
	}
	if ev.TokensIn != 7 || ev.TokensOut != 3 {
		t.Errorf("cost row tokens = %d/%d, want 7/3", ev.TokensIn, ev.TokensOut)
	}
	if ev.Model != "gpt-4o" {
		t.Errorf("cost row model = %q, want gpt-4o", ev.Model)
	}
}

func TestGateway_Streaming_RoundTripAndCostRow(t *testing.T) {
	up := fakeUpstream(t, "sk-upstream")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindSession(t, g, "sess-stream", "opencode", "sk-upstream", up.URL, "gpt-4o")

	resp := chatRequest(t, b, true)
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want SSE", ct)
	}

	var text strings.Builder
	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if !sawDone {
		t.Error("stream never sent [DONE]")
	}
	if text.String() != "Hello" {
		t.Errorf("assembled stream text = %q, want Hello", text.String())
	}

	evs := waitEvents(t, sink, 1)
	if evs[0].TokensIn != 7 || evs[0].TokensOut != 3 {
		t.Errorf("streamed cost row tokens = %d/%d, want 7/3", evs[0].TokensIn, evs[0].TokensOut)
	}
	if evs[0].Harness != "opencode" {
		t.Errorf("streamed cost row harness = %q, want opencode", evs[0].Harness)
	}
}

// TestGateway_SessionIsolation proves smoke item 4: a request carrying session
// A's token can only ever reach session A's credential scope, and an unknown
// token is refused — never allowed to borrow another session's upstream.
func TestGateway_SessionIsolation(t *testing.T) {
	upA := fakeUpstream(t, "sk-A")
	upB := fakeUpstream(t, "sk-B")
	g := startGateway(t, &costfeed.MemorySink{})

	bindA := bindSession(t, g, "A", "opencode", "sk-A", upA.URL, "gpt-4o")
	bindB := bindSession(t, g, "B", "opencode", "sk-B", upB.URL, "gpt-4o")

	// A's token → A's upstream (which asserts sk-A); B's token → B's upstream.
	for _, b := range []agent.EndpointBinding{bindA, bindB} {
		resp := chatRequest(t, b, false)
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("isolation round trip status %d: %s", resp.StatusCode, raw)
		}
		_ = resp.Body.Close()
	}

	// An unknown token cannot borrow any session's scope.
	rogue := bindA
	rogue.Env = map[string]string{gateway.TokenEnvVar: string(mustMint(t))}
	resp := chatRequest(t, rogue, false)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rogue token status = %d, want 401", resp.StatusCode)
	}
}

func TestGateway_NoCredential_ServiceUnavailable(t *testing.T) {
	up := fakeUpstream(t, "sk-x")
	g := startGateway(t, &costfeed.MemorySink{})
	// Bind with an empty single-key source: no credential is configured.
	b, err := g.Bind(gateway.BindConfig{
		SessionID: "empty",
		Harness:   "opencode",
		Company:   agent.CompanyOpenAI,
		Model:     "gpt-4o",
		AuthMode:  agent.AuthMetered,
		Surface:   agent.ProtoOpenAIChat,
		Upstream:  &upstream.OpenAICompat{Company: "openai", BaseURL: up.URL},
		Source:    pool.SingleKey{},
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	resp := chatRequest(t, b, false)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no-credential status = %d, want 503", resp.StatusCode)
	}
}

// TestGateway_Teardown_ReleasesListener proves smoke item 6.
func TestGateway_Teardown_ReleasesListener(t *testing.T) {
	g := gateway.New(gateway.Options{Sink: &costfeed.MemorySink{}})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	addr := g.Addr()
	if addr == "" {
		t.Fatal("no addr after start")
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if g.Started() {
		t.Error("gateway still Started() after Stop")
	}
	// The listener is released: a request to the old address fails to connect.
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get("http://" + addr + "/v1/models"); err == nil {
		t.Error("expected connection failure after teardown, got success")
	}
}

func TestGateway_BindRejectsUnsupportedSurface(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	_, err := g.Bind(gateway.BindConfig{
		SessionID: "s", Company: agent.CompanyAnthropic, Surface: agent.ProtoAnthropicMessages,
		Upstream: &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	})
	if err == nil {
		t.Fatal("expected error binding an M2 surface in M1")
	}
}

// waitEvents polls the sink until it has at least n events (the Meter call
// happens after the response is written; give it a brief, bounded window).
func waitEvents(t *testing.T, sink *costfeed.MemorySink, n int) []costfeed.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := sink.Events()
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("cost sink had %d events, want >= %d", len(evs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustMint(t *testing.T) token.Token {
	t.Helper()
	tok, err := token.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// assert the fake upstream body carried the reasoning_effort mapping when set,
// exercising the thinking normalization through the real request encoder.
func TestGateway_ThinkingEffortForwarded(t *testing.T) {
	got := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		got <- body.ReasoningEffort
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	g := startGateway(t, &costfeed.MemorySink{})
	b := bindSession(t, g, "s", "opencode", "k", ts.URL, "m")

	reqBody := bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`))
	req, _ := http.NewRequest(http.MethodPost, b.BaseURL+"/chat/completions", reqBody)
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case effort := <-got:
		if effort != "high" {
			t.Errorf("upstream reasoning_effort = %q, want high (normalized round trip)", effort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}
}
