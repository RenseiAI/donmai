package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/upstream"
)

func TestGateway_StatusReflectsRoutes(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	st := g.Status("/home/x/.donmai/gateway/cost-events.jsonl")
	if !st.Enabled {
		t.Error("status should be enabled after start")
	}
	if st.Routes != 0 {
		t.Errorf("routes = %d, want 0", st.Routes)
	}
	if len(st.Surfaces) != 1 || st.Surfaces[0] != string(agent.ProtoOpenAIChat) {
		t.Errorf("surfaces = %v, want [openai-chat]", st.Surfaces)
	}
	if st.LedgerPath == "" {
		t.Error("ledger path should be surfaced")
	}

	up := fakeUpstream(t, "k")
	b := bindSession(t, g, "s1", "opencode", "k", up.URL, "m")
	if g.Routes() != 1 {
		t.Fatalf("routes after bind = %d, want 1", g.Routes())
	}
	g.Unbind(token.Token(b.Env[gateway.TokenEnvVar]))
	if g.Routes() != 0 {
		t.Fatalf("routes after unbind = %d, want 0", g.Routes())
	}
}

func TestGateway_ModelsEndpoint(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	up := fakeUpstream(t, "k")
	b := bindSession(t, g, "s", "opencode", "k", up.URL, "gpt-4o")

	// With the session token: exactly the bound model.
	req, _ := http.NewRequest(http.MethodGet, b.BaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Data) != 1 || body.Data[0].ID != "gpt-4o" {
		t.Fatalf("models = %+v, want single gpt-4o", body.Data)
	}

	// Without a valid token: 401 (no catalog leak).
	req2, _ := http.NewRequest(http.MethodGet, b.BaseURL+"/models", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("unauth models request: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth /v1/models status = %d, want 401", resp2.StatusCode)
	}
}

func TestGateway_BindRequiresStarted(t *testing.T) {
	g := gateway.New(gateway.Options{})
	_, err := g.Bind(gateway.BindConfig{
		SessionID: "s", Company: agent.CompanyOpenAI, Surface: agent.ProtoOpenAIChat,
		Upstream: &upstream.OpenAICompat{}, Source: pool.SingleKey{Key: pool.Credential{Secret: "x"}},
	})
	if err == nil {
		t.Fatal("bind before start should error")
	}
}

func TestGateway_BindRequiresUpstreamAndSource(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	_, err := g.Bind(gateway.BindConfig{SessionID: "s", Company: agent.CompanyOpenAI, Surface: agent.ProtoOpenAIChat})
	if err == nil {
		t.Fatal("bind without upstream/source should error")
	}
}

func TestGateway_DoubleStartErrors(t *testing.T) {
	g := gateway.New(gateway.Options{})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
	if err := g.Start(context.Background()); err == nil {
		t.Fatal("second start should error")
	}
}
