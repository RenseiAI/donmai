package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/token"
)

// TestGateway_BearerExpiresShortTTL is the acceptance proof for the hostile-repo
// threat: a session bearer a repo hook, apiKeyHelper, or MCP server exfiltrates
// stops authorizing the route quickly. The gateway's bearer is short-TTL and
// loopback-only (bindWorkerGateway's loopback surface), so the stolen value is
// useless off-host and expires even on-host. RED proof: remove the expiry check
// in Dispatch and this test fails — the bearer works past its TTL.
func TestGateway_BearerExpiresShortTTL(t *testing.T) {
	now := time.Now()
	current := now
	g := gateway.New(gateway.Options{
		Sink: &costfeed.MemorySink{},
		Now:  func() time.Time { return current },
	})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() { _ = g.Stop(context.Background()) }()

	up := fakeUpstream(t, "sk-upstream")
	b := bindSession(t, g, "sess-expiry", "claude-code", "sk-upstream", up.URL, "gpt-4o")

	// Fresh bearer works.
	resp := chatRequest(t, b, false)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fresh bearer status = %d, want 200", resp.StatusCode)
	}

	// Past the short TTL the same bearer is refused — never borrowed, never
	// extended. A hostile repo holding this value gets 401, not a model call.
	current = now.Add(16 * time.Minute)
	resp2 := chatRequest(t, b, false)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != 401 {
		t.Fatalf("expired bearer status = %d, want 401: a stolen bearer must stop working", resp2.StatusCode)
	}
}

// TestGateway_ModelsSurfaceHonorsBearerExpiry pins the same expiry on the
// models surface: an expired bearer lists nothing.
func TestGateway_ModelsSurfaceHonorsBearerExpiry(t *testing.T) {
	now := time.Now()
	current := now
	g := gateway.New(gateway.Options{
		Sink: &costfeed.MemorySink{},
		Now:  func() time.Time { return current },
	})
	if err := g.Start(context.Background()); err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer func() { _ = g.Stop(context.Background()) }()

	up := fakeUpstream(t, "sk-upstream")
	b := bindSession(t, g, "sess-models-expiry", "claude-code", "sk-upstream", up.URL, "gpt-4o")

	current = now.Add(16 * time.Minute)
	req, err := http.NewRequest(http.MethodGet, b.BaseURL+"/models", nil)
	if err != nil {
		t.Fatalf("build models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.Env[gateway.TokenEnvVar])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 401 {
		t.Fatalf("expired bearer on /v1/models = %d, want 401", resp.StatusCode)
	}
}

// TestGateway_UnbindKillsBearerImmediately proves teardown (session end, the
// workerGateway.Close path both spawn lanes run) releases the route at once:
// the bearer does not linger until TTL.
func TestGateway_UnbindKillsBearerImmediately(t *testing.T) {
	g := startGateway(t, &costfeed.MemorySink{})
	up := fakeUpstream(t, "sk-upstream")
	b := bindSession(t, g, "sess-unbind", "claude-code", "sk-upstream", up.URL, "gpt-4o")

	resp := chatRequest(t, b, false)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("fresh bearer status = %d, want 200", resp.StatusCode)
	}

	g.Unbind(token.Token(b.Env[gateway.TokenEnvVar]))

	resp2 := chatRequest(t, b, false)
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != 401 {
		t.Fatalf("unbound bearer status = %d, want 401", resp2.StatusCode)
	}
}

var _ = agent.ProtoOpenAIChat
