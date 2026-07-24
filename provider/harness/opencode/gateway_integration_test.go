package opencode

import (
	"context"
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
	"github.com/RenseiAI/donmai/gateway/upstream"
)

// TestGatewayConsumer_ConfigInjectionAndRoundTrip is the M1 opencode-as-first-
// consumer wiring proof (08 §9). It stands up the translating gateway with an
// httptest OpenAI-compatible upstream, binds an opencode session (the resolver's
// job in production), renders the session opencode.json via the SAME buildConfig
// path Lane B uses, and confirms two things:
//
//  1. the injected provider baseURL points at the gateway's loopback surface
//     (config.go consumes Endpoint.BaseURL — the anticipated 08 hook), while the
//     key stays the {env:...} indirection (never inlined);
//  2. an OpenAI-compatible client driving that exact baseURL + the session
//     bearer completes a chat turn through the gateway to the upstream — the
//     wire path an opencode serve child takes.
func TestGatewayConsumer_ConfigInjectionAndRoundTrip(t *testing.T) {
	const upstreamKey = "sk-upstream"

	// httptest upstream: assert the gateway dialed with the session credential,
	// then reply with a canned completion.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamKey {
			t.Errorf("upstream auth = %q, want Bearer %s", got, upstreamKey)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"u1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer up.Close()

	sink := &costfeed.MemorySink{}
	g := gateway.New(gateway.Options{Sink: sink})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() { _ = g.Stop(context.Background()) }()

	binding, err := g.Bind(gateway.BindConfig{
		SessionID: "oc-sess",
		Harness:   string(agent.HarnessOpenCode),
		Company:   agent.CompanyOpenAI,
		Model:     "gpt-4o",
		AuthMode:  agent.AuthMetered,
		Surface:   agent.ProtoOpenAIChat,
		Upstream:  &upstream.OpenAICompat{Company: "openai", BaseURL: up.URL},
		Source:    pool.SingleKey{Key: pool.Credential{ID: "k", Secret: upstreamKey}},
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// (1) config.go renders the gateway binding into the opencode provider.
	spec := agent.Spec{Endpoint: &binding}
	cfg := buildConfig(spec)
	prov, ok := cfg.Provider[OCProviderID]
	if !ok {
		t.Fatalf("no %q provider in rendered config", OCProviderID)
	}
	if prov.Options.BaseURL != binding.BaseURL {
		t.Errorf("injected baseURL = %q, want gateway surface %q", prov.Options.BaseURL, binding.BaseURL)
	}
	if !strings.HasPrefix(prov.Options.BaseURL, "http://127.0.0.1:") {
		t.Errorf("gateway baseURL not loopback: %q", prov.Options.BaseURL)
	}
	if prov.Options.APIKey != "{env:"+OCKeyEnvVar+"}" {
		t.Errorf("apiKey = %q, want {env:%s} indirection", prov.Options.APIKey, OCKeyEnvVar)
	}
	if cfg.Model != OCProviderID+"/gpt-4o" {
		t.Errorf("cfg.Model = %q", cfg.Model)
	}

	// (2) an opencode-shaped client drives the rendered baseURL + the session
	// bearer (which the runner sources from binding.Env into DONMAI_OC_KEY).
	sessionToken := binding.Env[gateway.TokenEnvVar]
	if sessionToken == "" {
		t.Fatal("binding carried no session token")
	}
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, prov.Options.BaseURL+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat request: %v", err)
	}
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
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello" {
		t.Fatalf("unexpected completion: %+v", out)
	}

	// The exchange metered a cost row stamped with the opencode harness.
	evs := sink.Events()
	if len(evs) != 1 {
		t.Fatalf("cost rows = %d, want 1", len(evs))
	}
	if evs[0].Harness != string(agent.HarnessOpenCode) || evs[0].ProviderID != "openai" {
		t.Errorf("cost row = %+v, want opencode/openai", evs[0])
	}
}
