package pi

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// aggregatorGatewayBaseURL and aggregatorGatewayModel are the exact binding
// a control plane sends for a pi session on a Vercel AI Gateway model
// profile: the aggregator's OpenAI-compatible base URL and the aggregator's
// own "<author>/<model>" slug, verbatim.
const (
	aggregatorGatewayBaseURL = "https://ai-gateway.vercel.sh/v1"
	aggregatorGatewayModel   = "anthropic/claude-sonnet-4.6"
)

// realVercelGatewayCatalogListing is real `pi --list-models
// vercel-ai-gateway/anthropic/claude-sonnet-4.6 --offline` output captured
// against @earendil-works/pi-coding-agent 0.80.10 with AI_GATEWAY_API_KEY
// set and an isolated PI_CODING_AGENT_DIR. The same query against pi's
// direct provider (`--list-models anthropic/claude-sonnet-4.6`) prints
// `No models matching "anthropic/claude-sonnet-4.6"`: pi's anthropic
// catalog spells the model claude-sonnet-4-6.
const realVercelGatewayCatalogListing = `provider           model                        context  max-out  thinking  images
vercel-ai-gateway  anthropic/claude-sonnet-4.6  1M       128K     yes       yes   
`

func aggregatorGatewayBinding() *agent.EndpointBinding {
	return &agent.EndpointBinding{
		Company:     agent.CompanyOpenAI,
		Model:       aggregatorGatewayModel,
		ModelAuthor: "anthropic",
		BaseURL:     aggregatorGatewayBaseURL,
		Protocol:    agent.ProtoOpenAIChat,
		Host:        agent.HostDirect,
		Auth:        agent.AuthBYOK,
	}
}

func TestBuiltinAggregatorForBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		baseURL string
		want    string
		wantOK  bool
	}{
		{name: "vercel gateway openai-compat base", baseURL: "https://ai-gateway.vercel.sh/v1", want: "vercel-ai-gateway", wantOK: true},
		{name: "vercel gateway root", baseURL: "https://ai-gateway.vercel.sh", want: "vercel-ai-gateway", wantOK: true},
		{name: "host match is case-insensitive", baseURL: "https://AI-Gateway.Vercel.sh/v1", want: "vercel-ai-gateway", wantOK: true},
		{name: "openrouter", baseURL: "https://openrouter.ai/api/v1", want: "openrouter", wantOK: true},
		{name: "plain http is not the provider's endpoint", baseURL: "http://ai-gateway.vercel.sh/v1", wantOK: false},
		{name: "look-alike subdomain", baseURL: "https://ai-gateway.vercel.sh.example.invalid/v1", wantOK: false},
		{name: "direct vendor endpoint", baseURL: "https://api.z.ai/api/coding/paas/v4", wantOK: false},
		{name: "loopback gateway", baseURL: "http://127.0.0.1:7734/v1", wantOK: false},
		{name: "empty", baseURL: "", wantOK: false},
		{name: "unparseable", baseURL: "://nope", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := builtinAggregatorForBaseURL(tc.baseURL)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("builtinAggregatorForBaseURL(%q) = (%q, %v), want (%q, %v)", tc.baseURL, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestNativeProviderPin_AggregatorEndpoint proves the serving host, not the
// model slug's author segment, selects the built-in provider: the gateway
// slug "anthropic/claude-sonnet-4.6" on the gateway's base URL routes to
// pi's vercel-ai-gateway provider with the slug kept whole, never to pi's
// direct anthropic provider with "claude-sonnet-4.6".
func TestNativeProviderPin_AggregatorEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		model        string
		ep           *agent.EndpointBinding
		wantProvider string
		wantModel    string
		wantNative   bool
	}{
		{
			name: "vercel gateway anthropic slug", model: aggregatorGatewayModel, ep: aggregatorGatewayBinding(),
			wantProvider: "vercel-ai-gateway", wantModel: aggregatorGatewayModel, wantNative: true,
		},
		{
			name: "vercel gateway openai slug", model: "openai/gpt-5.4", ep: aggregatorGatewayBinding(),
			wantProvider: "vercel-ai-gateway", wantModel: "openai/gpt-5.4", wantNative: true,
		},
		{
			name: "pin already carrying the aggregator's own prefix", model: "vercel-ai-gateway/" + aggregatorGatewayModel, ep: aggregatorGatewayBinding(),
			wantProvider: "vercel-ai-gateway", wantModel: aggregatorGatewayModel, wantNative: true,
		},
		{
			name: "openrouter slug", model: "anthropic/claude-sonnet-4.6",
			ep:           &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://openrouter.ai/api/v1"},
			wantProvider: "openrouter", wantModel: "anthropic/claude-sonnet-4.6", wantNative: true,
		},
		{
			name: "same slug with no endpoint keeps the prefix-split behavior", model: aggregatorGatewayModel, ep: nil,
			wantProvider: "anthropic", wantModel: "claude-sonnet-4.6", wantNative: true,
		},
		{
			name: "loopback gateway host is never treated as an aggregator", model: aggregatorGatewayModel,
			ep:           &agent.EndpointBinding{Host: agent.HostGateway, BaseURL: "http://127.0.0.1:7734/v1"},
			wantProvider: "anthropic", wantModel: "claude-sonnet-4.6", wantNative: false,
		},
		{
			name: "direct vendor endpoint keeps the prefix-split behavior", model: "zai/glm-5.3",
			ep:           &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://api.z.ai/api/coding/paas/v4"},
			wantProvider: "zai", wantModel: "glm-5.3", wantNative: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, model, native := nativeProviderPin(tc.model, tc.ep)
			if provider != tc.wantProvider || model != tc.wantModel || native != tc.wantNative {
				t.Errorf("nativeProviderPin(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.model, provider, model, native, tc.wantProvider, tc.wantModel, tc.wantNative)
			}
		})
	}
}

// TestModelPinArgs_AggregatorEndpoint pins the argv both spawn modes share
// (rpcArgs and interactiveArgs call modelPinArgs).
func TestModelPinArgs_AggregatorEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		effort agent.EffortLevel
		want   []string
	}{
		{name: "no effort", want: []string{"--provider", "vercel-ai-gateway", "--model", aggregatorGatewayModel}},
		{name: "effort suffix applies to the whole slug", effort: agent.EffortHigh, want: []string{"--provider", "vercel-ai-gateway", "--model", aggregatorGatewayModel + ":high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := modelPinArgs(agent.Spec{Model: aggregatorGatewayModel, Effort: tc.effort, Endpoint: aggregatorGatewayBinding()})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("modelPinArgs = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyEndpoint_AggregatorCredentialRouting covers both key-delivery
// shapes. Over the dispatch wire the binding's Env is empty (json:"-") and
// the resolved key arrives on Spec.Env, as DONMAI_PI_KEY alone or alongside
// AI_GATEWAY_API_KEY; either way the aggregator's own env var must carry it
// and the direct vendor's env var must not.
func TestApplyEndpoint_AggregatorCredentialRouting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		specEnv map[string]string
		epEnv   map[string]string
	}{
		{name: "wire shape, pi key only", specEnv: map[string]string{PiKeyEnvVar: "gw-key"}},
		{name: "wire shape, both vars", specEnv: map[string]string{PiKeyEnvVar: "gw-key", "AI_GATEWAY_API_KEY": "gw-key"}},
		{name: "in-process binding env", epEnv: map[string]string{"OPENAI_API_KEY": "gw-key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := aggregatorGatewayBinding()
			ep.Env = tc.epEnv
			spec, err := applyEndpoint(agent.Spec{Model: aggregatorGatewayModel, Env: tc.specEnv, Endpoint: ep})
			if err != nil {
				t.Fatalf("applyEndpoint: %v", err)
			}
			if got := spec.Env["AI_GATEWAY_API_KEY"]; got != "gw-key" {
				t.Errorf("AI_GATEWAY_API_KEY = %q, want the resolved gateway key", got)
			}
			if _, leaked := spec.Env["ANTHROPIC_API_KEY"]; leaked {
				t.Errorf("gateway key mirrored onto the direct vendor's ANTHROPIC_API_KEY: %v", spec.Env)
			}
			if spec.Model != aggregatorGatewayModel {
				t.Errorf("spec.Model = %q, want %q", spec.Model, aggregatorGatewayModel)
			}
		})
	}
}

// TestProviderPinEnv_AggregatorKeepsWholeSlug proves the injected provider's
// registered model id is the aggregator's wire model code, unstripped.
func TestProviderPinEnv_AggregatorKeepsWholeSlug(t *testing.T) {
	t.Parallel()
	env := providerPinEnv(agent.Spec{Model: aggregatorGatewayModel, Endpoint: aggregatorGatewayBinding()})
	want := piModelEnvVar + "=" + aggregatorGatewayModel
	if !slices.Contains(env, want) {
		t.Errorf("providerPinEnv = %v, want it to contain %q", env, want)
	}
}

// TestSpawn_AggregatorEndpoint_CatalogPreflight drives Spawn end to end with
// the gateway binding: the preflight must query pi's vercel-ai-gateway
// catalog for the whole slug with the gateway credential, pass on the real
// 0.80.10 listing, and deny when the aggregator catalog lacks the model.
// Before the fix the probe was asked for anthropic/claude-sonnet-4.6 under
// the direct anthropic provider and spawn failed with "pi has no built-in
// model".
func TestSpawn_AggregatorEndpoint_CatalogPreflight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		listing   string
		wantAllow bool
	}{
		{name: "model present in the aggregator catalog", listing: realVercelGatewayCatalogListing, wantAllow: true},
		{name: "model absent from the aggregator catalog", listing: `No models matching "vercel-ai-gateway/anthropic/claude-sonnet-4.6"` + "\n", wantAllow: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pr, pw := io.Pipe()
			t.Cleanup(func() { _ = pw.Close() })
			type probeCall struct{ provider, model, credVar, credValue string }
			calls := make(chan probeCall, 1)
			p, err := New(Options{
				skipProcess:      true,
				stdinOverride:    &syncBuffer{},
				stdoutOverride:   pr,
				handshakeToken:   testHandshakeToken,
				HandshakeTimeout: 2 * time.Second,
				CatalogProbe: func(_ context.Context, _, provider, model, credVar, credValue string) (string, error) {
					calls <- probeCall{provider, model, credVar, credValue}
					return tc.listing, nil
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if tc.wantAllow {
				body := handshakeEvent("h1") + getStateResponse("ses_gateway") +
					event(map[string]any{"type": "agent_start"}) +
					event(map[string]any{"type": "agent_settled"})
				go func() { _, _ = io.WriteString(pw, body) }()
			}
			h, err := p.Spawn(context.Background(), agent.Spec{
				Prompt:   "hi",
				Cwd:      t.TempDir(),
				Model:    aggregatorGatewayModel,
				Env:      map[string]string{PiKeyEnvVar: "gw-key", "AI_GATEWAY_API_KEY": "gw-key"},
				Endpoint: aggregatorGatewayBinding(),
			})
			got := <-calls
			want := probeCall{"vercel-ai-gateway", aggregatorGatewayModel, "AI_GATEWAY_API_KEY", "gw-key"}
			if got != want {
				t.Errorf("catalog probe called with %+v, want %+v", got, want)
			}
			if !tc.wantAllow {
				if err == nil {
					_ = h.Stop(context.Background())
					t.Fatal("Spawn succeeded for a model the aggregator catalog lacks")
				}
				if !errors.Is(err, agent.ErrSpawnFailed) {
					t.Errorf("Spawn error does not wrap agent.ErrSpawnFailed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
			drain(t, h)
		})
	}
}
