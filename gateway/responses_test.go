package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/upstream"
)

func bindProtocolSession(t *testing.T, g *gateway.Gateway, proto agent.WireProtocol, surf agent.WireProtocol, upstreamURL string) agent.EndpointBinding {
	t.Helper()
	b, err := g.BindProtocol(gateway.BindConfig{
		SessionID: "s-resp",
		Harness:   "codex",
		Company:   agent.CompanyOpenAI,
		Model:     "gpt-test",
		AuthMode:  agent.AuthMetered,
		Surface:   surf,
		Upstream:  &upstream.OpenAICompat{Company: "openai", BaseURL: upstreamURL},
		Source:    pool.SingleKey{Key: pool.Credential{ID: "k1", Secret: "up-key"}},
	}, proto)
	if err != nil {
		t.Fatalf("BindProtocol(%q): %v", proto, err)
	}
	return b
}

func TestGateway_BindProtocolChatMatchesBind(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	up := fakeUpstream(t, "up-key")
	viaBind := bindSession(t, g, "s-bind", "pi", "up-key", up.URL, "m")
	viaProto := bindProtocolSession(t, g, agent.ProtoOpenAIChat, "", up.URL)
	if viaProto.Protocol != agent.ProtoOpenAIChat {
		t.Errorf("protocol = %q, want openai-chat", viaProto.Protocol)
	}
	if viaProto.Host != viaBind.Host || viaProto.Company != viaBind.Company || viaProto.Mechanism != viaBind.Mechanism {
		t.Errorf("BindProtocol(chat) binding = %+v, want the Bind shape %+v", viaProto, viaBind)
	}
	if viaProto.Env[gateway.TokenEnvVar] == "" {
		t.Error("binding carries no bearer token")
	}
}

func TestGateway_BindProtocolResponsesCarriesExactProtocol(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	up := fakeUpstream(t, "up-key")
	b := bindProtocolSession(t, g, agent.ProtoOpenAIResponses, "", up.URL)
	if b.Protocol != agent.ProtoOpenAIResponses {
		t.Fatalf("protocol = %q, want openai-responses (no relabeling)", b.Protocol)
	}
	if b.Host != agent.HostGateway || b.Company != agent.CompanyOpenAI {
		t.Errorf("binding = %+v", b)
	}
	if !strings.HasSuffix(b.BaseURL, "/v1") {
		t.Errorf("baseURL = %q, want the /v1 root so the client appends /responses", b.BaseURL)
	}
}

func TestGateway_BindProtocolConflictingSurfaceRefuses(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	if _, err := g.BindProtocol(gateway.BindConfig{
		SessionID: "s", Surface: agent.ProtoOpenAIChat,
		Upstream: &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	}, agent.ProtoOpenAIResponses); err == nil {
		t.Fatal("BindProtocol with a conflicting Surface = nil error, want a loud refusal")
	}
}

func TestGateway_BindProtocolUnknownProtocolRefuses(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	if _, err := g.BindProtocol(gateway.BindConfig{
		SessionID: "s",
		Upstream:  &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	}, agent.ProtoAnthropicMessages); err == nil {
		t.Fatal("BindProtocol(anthropic-messages) = nil error, want a refusal (not an implicit chat bind)")
	}
	if _, err := g.BindProtocol(gateway.BindConfig{
		SessionID: "s",
		Upstream:  &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	}, ""); err == nil {
		t.Fatal("BindProtocol(\"\") = nil error, want a refusal (protocol is required)")
	}
}

func TestGateway_BindLegacyStillChatOnly(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	// Empty surface keeps refusing on the legacy path (explicit selection only).
	if _, err := g.Bind(gateway.BindConfig{
		SessionID: "s",
		Upstream:  &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	}); err == nil {
		t.Fatal("Bind(\"\") = nil error, want the legacy refusal")
	}
	// A responses Surface on the legacy path refuses rather than relabeling.
	if _, err := g.Bind(gateway.BindConfig{
		SessionID: "s", Surface: agent.ProtoOpenAIResponses,
		Upstream: &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	}); err == nil {
		t.Fatal("Bind(responses) = nil error, want a refusal (Bind serves chat only)")
	}
}

// TestGateway_ResponsesEndToEnd posts a Responses request with a tool
// declaration through the loopback gateway to the fake chat upstream and
// asserts the full trip: the upstream saw a chat-completions body carrying
// the tool, the client got a Responses object with the tool call and usage,
// and the cost ledger metered the exchange.
func TestGateway_ResponsesEndToEnd(t *testing.T) {
	var upstreamBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer up-key" {
			t.Errorf("upstream got auth %q, want this session's credential", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"up-1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"checking","tool_calls":[{"id":"call-42","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindProtocolSession(t, g, agent.ProtoOpenAIResponses, "", ts.URL)

	reqBody := `{"model":"gpt-test","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	req, _ := http.NewRequest(http.MethodPost, b.BaseURL+"/responses", bytes.NewReader([]byte(reqBody)))
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
	}

	// The chat upstream received the translated request with the tool.
	var upReq struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(upstreamBody, &upReq); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if len(upReq.Tools) != 1 || upReq.Tools[0].Function.Name != "get_weather" {
		t.Errorf("upstream tools = %+v, want the translated function", upReq.Tools)
	}
	if len(upReq.Messages) == 0 || upReq.Messages[len(upReq.Messages)-1].Content != "weather?" {
		t.Errorf("upstream messages = %+v", upReq.Messages)
	}

	// The client received a Responses object with text, the tool call, usage.
	var got struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode responses body: %v", err)
	}
	if got.Object != "response" || got.Status != "completed" {
		t.Errorf("envelope = %s/%s", got.Object, got.Status)
	}
	if len(got.Output) != 2 || got.Output[0].Type != "message" || got.Output[1].Type != "function_call" {
		t.Fatalf("output has %d items, want message + function_call: %s", len(got.Output), raw)
	}
	if got.Output[0].Content[0].Text != "checking" {
		t.Errorf("message text = %+v", got.Output[0].Content)
	}
	if got.Output[1].CallID != "call-42" || got.Output[1].Name != "get_weather" {
		t.Errorf("function call = %+v", got.Output[1])
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 6 {
		t.Errorf("usage = %+v", got.Usage)
	}

	evs := waitEvents(t, sink, 1)
	if evs[0].TokensIn != 12 || evs[0].TokensOut != 6 || evs[0].Harness != "codex" {
		t.Errorf("cost row = %+v, want 12/6 metered to the codex harness", evs[0])
	}
}

// TestGateway_ResponsesStreamingEndToEnd streams a Responses request through
// the loopback gateway and asserts SSE framing plus the completed snapshot.
func TestGateway_ResponsesStreamingEndToEnd(t *testing.T) {
	up := fakeUpstream(t, "up-key")
	sink := &costfeed.MemorySink{}
	g := startGateway(t, sink)
	b := bindProtocolSession(t, g, agent.ProtoOpenAIResponses, "", up.URL)

	req, _ := http.NewRequest(http.MethodPost, b.BaseURL+"/responses",
		bytes.NewReader([]byte(`{"model":"gpt-test","input":"hi","stream":true}`)))
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want SSE", ct)
	}
	body := string(raw)
	for _, want := range []string{
		`"type":"response.created"`,
		`"type":"response.output_text.delta"`,
		`"type":"response.output_text.done"`,
		`"type":"response.completed"`,
		`"text":"Hello"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %s:\n%s", want, body)
		}
	}
	evs := waitEvents(t, sink, 1)
	if evs[0].TokensIn != 7 || evs[0].TokensOut != 3 {
		t.Errorf("cost row = %+v, want the terminal usage 7/3", evs[0])
	}
}

// TestGateway_ResponsesUnknownTokenIs401 pins per-session isolation on the new
// surface: a bearer minted for another session (or none) cannot reach a route.
func TestGateway_ResponsesUnknownTokenIs401(t *testing.T) {
	up := fakeUpstream(t, "up-key")
	g := startGateway(t, &costfeed.MemorySink{})
	bindProtocolSession(t, g, agent.ProtoOpenAIResponses, "", up.URL)
	req, _ := http.NewRequest(http.MethodPost, mustBaseURL(t, g)+"/responses",
		bytes.NewReader([]byte(`{"model":"m","input":"hi"}`)))
	req.Header.Set("Authorization", "Bearer "+string(mustMintToken(t)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func mustBaseURL(t *testing.T, g *gateway.Gateway) string {
	t.Helper()
	return "http://" + g.Addr() + "/v1"
}

func mustMintToken(t *testing.T) token.Token {
	t.Helper()
	tok, err := token.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}
