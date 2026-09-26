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
		{name: "look-alike suffix host", baseURL: "https://ai-gateway.vercel.sh.evil.example/v1", wantOK: false},
		{name: "look-alike subdomain", baseURL: "https://x.ai-gateway.vercel.sh/v1", wantOK: false},
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

// TestModelRouting_BoundEndpoint pins the whole routing rule for a pin on a
// bound (non-loopback) endpoint, through all three consumers: the argv both
// spawn modes share (modelPinArgs), the injected provider's registered model
// id (providerPinEnv), and the credential mirror (applyEndpoint). The key
// arrives in the dispatch-wire shape: DONMAI_PI_KEY on Spec.Env, no binding
// Env. Native routing, and a vendor env var carrying the key, happen only on
// the provider's own https host; every other BaseURL stays on the injected
// provider and no vendor env var is set.
func TestModelRouting_BoundEndpoint(t *testing.T) {
	t.Parallel()
	vendorVars := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "AI_GATEWAY_API_KEY", "OPENROUTER_API_KEY", "ZAI_API_KEY"}
	cases := []struct {
		name          string
		model         string
		baseURL       string
		wantArgs      []string
		wantPinModel  string
		wantVendorVar string // "" = no vendor var may be set
	}{
		{
			name:  "gateway slug before catalog promotion stays on the injected provider, whole",
			model: aggregatorGatewayModel, baseURL: aggregatorGatewayBaseURL,
			wantArgs: []string{"--provider", pinnedProviderName, "--model", aggregatorGatewayModel}, wantPinModel: aggregatorGatewayModel,
		},
		{
			name:  "promoted gateway pin routes natively with the gateway key",
			model: "vercel-ai-gateway/" + aggregatorGatewayModel, baseURL: aggregatorGatewayBaseURL,
			wantArgs: []string{"--provider", "vercel-ai-gateway", "--model", aggregatorGatewayModel}, wantPinModel: aggregatorGatewayModel,
			wantVendorVar: "AI_GATEWAY_API_KEY",
		},
		{
			name:  "gateway model newer than pi's catalog (non-builtin author) stays injected, whole",
			model: "meta/muse-spark-1.3-contributor", baseURL: aggregatorGatewayBaseURL,
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "meta/muse-spark-1.3-contributor"}, wantPinModel: "meta/muse-spark-1.3-contributor",
		},
		{
			name:  "unprefixed model on the gateway stays injected",
			model: "gpt-5.4", baseURL: aggregatorGatewayBaseURL,
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "gpt-5.4"}, wantPinModel: "gpt-5.4",
		},
		{
			name:  "promoted openrouter pin routes natively with the openrouter key",
			model: "openrouter/anthropic/claude-sonnet-4.6", baseURL: "https://openrouter.ai/api/v1",
			wantArgs: []string{"--provider", "openrouter", "--model", "anthropic/claude-sonnet-4.6"}, wantPinModel: "anthropic/claude-sonnet-4.6",
			wantVendorVar: "OPENROUTER_API_KEY",
		},
		{
			name:  "direct vendor on its own host routes natively (unchanged)",
			model: "zai/glm-5.3", baseURL: "https://api.z.ai/api/coding/paas/v4",
			wantArgs: []string{"--provider", "zai", "--model", "glm-5.3"}, wantPinModel: "glm-5.3",
			wantVendorVar: "ZAI_API_KEY",
		},
		{
			name:  "vendor-prefixed model on an unknown https proxy stays injected",
			model: "anthropic/claude-sonnet-4-6", baseURL: "https://my-proxy.example.com/v1",
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "claude-sonnet-4-6"}, wantPinModel: "claude-sonnet-4-6",
		},
		{
			name:  "vendor-prefixed model on a look-alike gateway host stays injected",
			model: "openai/gpt-5.4", baseURL: "https://ai-gateway.vercel.sh.evil.example/v1",
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "gpt-5.4"}, wantPinModel: "gpt-5.4",
		},
		{
			name:  "vendor-prefixed model on plain http gateway host stays injected",
			model: "anthropic/claude-sonnet-4-6", baseURL: "http://ai-gateway.vercel.sh/v1",
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "claude-sonnet-4-6"}, wantPinModel: "claude-sonnet-4-6",
		},
		{
			name:  "vendor-prefixed model on plain http vendor host stays injected",
			model: "anthropic/claude-sonnet-4-6", baseURL: "http://api.anthropic.com",
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "claude-sonnet-4-6"}, wantPinModel: "claude-sonnet-4-6",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ep := &agent.EndpointBinding{
				Company: agent.CompanyOpenAI, BaseURL: tc.baseURL, Protocol: agent.ProtoOpenAIChat,
				Host: agent.HostDirect, Auth: agent.AuthBYOK,
			}
			spec, err := applyEndpoint(agent.Spec{Model: tc.model, Env: map[string]string{PiKeyEnvVar: "wire-key"}, Endpoint: ep})
			if err != nil {
				t.Fatalf("applyEndpoint: %v", err)
			}
			if got := modelPinArgs(spec); !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("modelPinArgs = %q, want %q", got, tc.wantArgs)
			}
			if env, want := providerPinEnv(spec), piModelEnvVar+"="+tc.wantPinModel; !slices.Contains(env, want) {
				t.Errorf("providerPinEnv = %v, want it to contain %q", env, want)
			}
			for _, v := range vendorVars {
				got, set := spec.Env[v]
				switch {
				case v == tc.wantVendorVar && got != "wire-key":
					t.Errorf("%s = %q, want the wire key", v, got)
				case v != tc.wantVendorVar && set:
					t.Errorf("%s must not be set on this route, got %q", v, got)
				}
			}
		})
	}
}

// TestSpawn_AggregatorEndpoint_CatalogPromotion drives Spawn end to end with
// the gateway binding. The catalog query is made for the whole slug under
// the aggregator with the resolved key; a confirmed slug routes natively
// through pi's aggregator provider, and a slug pi's catalog lacks (or a
// failed probe) falls back to the injected provider instead of denying spawn.
func TestSpawn_AggregatorEndpoint_CatalogPromotion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		model    string
		baseURL  string // "" = the Vercel gateway binding
		listing  string
		probeErr error
		wantArgs []string
		wantEnv  map[string]string
	}{
		{
			name: "openrouter slug present in pi's catalog routes natively with the openrouter key", model: "anthropic/claude-sonnet-4.6",
			baseURL:  "https://openrouter.ai/api/v1",
			listing:  "provider    model                        context\nopenrouter  anthropic/claude-sonnet-4.6  1M\n",
			wantArgs: []string{"--provider", "openrouter", "--model", "anthropic/claude-sonnet-4.6"},
			wantEnv:  map[string]string{"OPENROUTER_API_KEY": "gw-key", PiKeyEnvVar: "gw-key"},
		},
		{
			name: "slug present in pi's catalog routes natively", model: aggregatorGatewayModel, listing: realVercelGatewayCatalogListing,
			wantArgs: []string{"--provider", "vercel-ai-gateway", "--model", aggregatorGatewayModel},
			wantEnv:  map[string]string{"AI_GATEWAY_API_KEY": "gw-key", PiKeyEnvVar: "gw-key"},
		},
		{
			name: "slug newer than pi's catalog uses the injected provider", model: "meta/muse-spark-1.3-contributor",
			listing:  `No models matching "vercel-ai-gateway/meta/muse-spark-1.3-contributor"` + "\n",
			wantArgs: []string{"--provider", pinnedProviderName, "--model", "meta/muse-spark-1.3-contributor"},
			wantEnv:  map[string]string{PiKeyEnvVar: "gw-key"},
		},
		{
			name: "probe error uses the injected provider", model: aggregatorGatewayModel, probeErr: errors.New("probe unavailable"),
			wantArgs: []string{"--provider", pinnedProviderName, "--model", aggregatorGatewayModel},
			wantEnv:  map[string]string{PiKeyEnvVar: "gw-key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			type probeCall struct{ provider, model, credVar, credValue string }
			calls := make(chan probeCall, 4)
			p := &Provider{binary: "pi", opts: Options{
				CatalogProbe: func(_ context.Context, _, provider, model, credVar, credValue string) (string, error) {
					calls <- probeCall{provider, model, credVar, credValue}
					return tc.listing, tc.probeErr
				},
			}}
			ep := aggregatorGatewayBinding()
			ep.Model = tc.model
			wantProvider, wantVar := "vercel-ai-gateway", "AI_GATEWAY_API_KEY"
			if tc.baseURL != "" {
				ep.BaseURL = tc.baseURL
				wantProvider, wantVar = "openrouter", "OPENROUTER_API_KEY"
			}
			spec, err := p.prepare(context.Background(), agent.Spec{
				Prompt:   "hi",
				Cwd:      t.TempDir(),
				Model:    tc.model,
				Env:      map[string]string{PiKeyEnvVar: "gw-key"},
				Endpoint: ep,
			})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			close(calls)
			var got []probeCall
			for c := range calls {
				got = append(got, c)
			}
			want := []probeCall{{wantProvider, tc.model, wantVar, "gw-key"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("catalog probe calls = %+v, want exactly %+v", got, want)
			}
			if args := modelPinArgs(spec); !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("modelPinArgs = %q, want %q", args, tc.wantArgs)
			}
			for _, v := range []string{"AI_GATEWAY_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", PiKeyEnvVar} {
				if spec.Env[v] != tc.wantEnv[v] {
					t.Errorf("env %s = %q, want %q", v, spec.Env[v], tc.wantEnv[v])
				}
			}
		})
	}
}

// TestSpawn_AggregatorEndpoint_Native completes a full Spawn on the promoted
// native route, proving prepare's promotion composes with the launch path.
func TestSpawn_AggregatorEndpoint_Native(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    &syncBuffer{},
		stdoutOverride:   pr,
		handshakeToken:   testHandshakeToken,
		HandshakeTimeout: 2 * time.Second,
		CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return realVercelGatewayCatalogListing, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := handshakeEvent("h1") + getStateResponse("ses_gateway") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	go func() { _, _ = io.WriteString(pw, body) }()
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:   "hi",
		Cwd:      t.TempDir(),
		Model:    aggregatorGatewayModel,
		Env:      map[string]string{PiKeyEnvVar: "gw-key", "AI_GATEWAY_API_KEY": "gw-key"},
		Endpoint: aggregatorGatewayBinding(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	drain(t, h)
}
