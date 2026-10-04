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

// directGoogleEndpoint is the control-plane binding for a session pinned to
// a provider-native model id against that provider's own API host: the
// bound base URL names the provider's own serving host, so nativeProviderPin
// routes it natively — until the catalog preflight confirms the id is newer
// than the bundled catalog and prepare() falls back to the injected
// provider.
func directGoogleEndpoint() *agent.EndpointBinding {
	return &agent.EndpointBinding{
		Company:  agent.CompanyGoogle,
		Model:    "google/gemini-4-pro",
		BaseURL:  "https://generativelanguage.googleapis.com/v1beta",
		Host:     agent.HostDirect,
		Protocol: agent.ProtoGeminiGenerate,
		Env:      map[string]string{"GEMINI_API_KEY": "cell-key"},
	}
}

// TestPrepare_CatalogMiss_FallsBackToInjectedProvider is the table-driven
// proof of the launch-day fallback: a native built-in-provider pin whose
// (provider, model) pair the scripted catalog reports as ABSENT reroutes
// onto the injected provider — the bound base URL, the provider-native wire
// API, the bare model id, the mirrored credential, and the resolved limits
// all survive — instead of denying spawn. A confirmed HIT still routes
// natively, and a probe ERROR keeps today's unverified behaviour (native,
// unblocked). RED proof: return the miss error from prepare() without
// translating it (drop the fallbackToInjectedProvider branch in pi.go) and
// every "miss" row below fails with a spawn denial instead of the injected
// route.
func TestPrepare_CatalogMiss_FallsBackToInjectedProvider(t *testing.T) {
	t.Parallel()
	missListing := "provider  model    context  max-out  thinking  images\ngoogle    gemini-2.5-pro  1M  65K  yes  no\n"
	hitListing := "provider  model    context  max-out  thinking  images\ngoogle    gemini-4-pro  1M  65K  yes  no\n"
	cases := []struct {
		name       string
		listing    string
		probeErr   error
		wantArgs   []string
		wantPinID  string
		wantAPI    string
		wantNative bool
	}{
		{
			name:      "miss on a direct provider endpoint falls back to the injected provider with the native wire API and bare model id",
			listing:   missListing,
			wantArgs:  []string{"--provider", pinnedProviderName, "--model", "gemini-4-pro"},
			wantPinID: "gemini-4-pro",
			wantAPI:   "google-generative-ai",
		},
		{
			name:       "hit on a direct provider endpoint still routes natively",
			listing:    hitListing,
			wantArgs:   []string{"--provider", "google", "--model", "gemini-4-pro"},
			wantPinID:  "gemini-4-pro",
			wantAPI:    "google-generative-ai",
			wantNative: true,
		},
		{
			name:       "probe error keeps today's unverified native route",
			listing:    "",
			probeErr:   errors.New("probe unavailable"),
			wantArgs:   []string{"--provider", "google", "--model", "gemini-4-pro"},
			wantPinID:  "gemini-4-pro",
			wantAPI:    "google-generative-ai",
			wantNative: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Provider{binary: "pi", opts: Options{
				CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
					return tc.listing, tc.probeErr
				},
			}}
			spec, err := p.prepare(context.Background(), agent.Spec{
				Prompt:   "hi",
				Cwd:      t.TempDir(),
				Model:    "google/gemini-4-pro",
				Env:      map[string]string{PiKeyEnvVar: "cell-key"},
				Endpoint: directGoogleEndpoint(),
				ProviderConfig: map[string]any{
					"contextWindow":   1_000_000,
					"maxOutputTokens": 64000,
				},
			})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if args := modelPinArgs(spec); !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("modelPinArgs = %q, want %q", args, tc.wantArgs)
			}
			if _, _, useNative := nativeProviderPin(spec.Model, spec.Endpoint); useNative != tc.wantNative {
				t.Errorf("fallback spec routes useNative=%v, want %v (spec.Model=%q)", useNative, tc.wantNative, spec.Model)
			}
			if env := providerPinEnv(spec); !slices.Contains(env, piModelEnvVar+"="+tc.wantPinID) {
				t.Errorf("providerPinEnv = %v, want it to contain %q", env, piModelEnvVar+"="+tc.wantPinID)
			} else if !slices.Contains(env, piAPIEnvVar+"="+tc.wantAPI) {
				t.Errorf("providerPinEnv = %v, want it to contain %q (the provider-native wire API)", env, piAPIEnvVar+"="+tc.wantAPI)
			}
			if !slices.Contains(providerPinEnv(spec), piBaseURLEnvVar+"=https://generativelanguage.googleapis.com/v1beta") {
				t.Errorf("providerPinEnv lost the bound base URL: %v", providerPinEnv(spec))
			}
			if !slices.Contains(providerPinEnv(spec), piContextWindowEnvVar+"=1000000") {
				t.Errorf("providerPinEnv lost the resolved context window: %v", providerPinEnv(spec))
			}
			if !slices.Contains(providerPinEnv(spec), piOutputLimitEnvVar+"=64000") {
				t.Errorf("providerPinEnv lost the resolved output limit: %v", providerPinEnv(spec))
			}
			if spec.Env["GEMINI_API_KEY"] != "cell-key" {
				t.Errorf("provider-native credential mirror lost: %v", spec.Env)
			}
			if spec.Env[PiKeyEnvVar] != "cell-key" {
				t.Errorf("injected-provider key mirror lost: %v", spec.Env)
			}
		})
	}
}

// TestPrepare_CatalogMiss_UnknownFutureModelPassesThrough pins that an
// unknown future model id is never allowlist-rejected by the fallback: the
// bare id rides the injected provider verbatim, whatever it spells.
func TestPrepare_CatalogMiss_UnknownFutureModelPassesThrough(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi", opts: Options{
		CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return "provider  model\n", nil
		},
	}}
	ep := directGoogleEndpoint()
	ep.Model = "google/gemini-4-pro-preview"
	spec, err := p.prepare(context.Background(), agent.Spec{
		Prompt:         "hi",
		Cwd:            t.TempDir(),
		Model:          "google/gemini-4-pro-preview",
		Env:            map[string]string{PiKeyEnvVar: "cell-key"},
		Endpoint:       ep,
		ProviderConfig: map[string]any{"maxOutputTokens": 64000},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if spec.Model != "gemini-4-pro-preview" {
		t.Errorf("spec.Model = %q, want the bare provider-native id", spec.Model)
	}
	if args := modelPinArgs(spec); !reflect.DeepEqual(args, []string{"--provider", pinnedProviderName, "--model", "gemini-4-pro-preview"}) {
		t.Errorf("modelPinArgs = %q, want the injected route with the bare id", args)
	}
}

// TestPrepare_CatalogMiss_KeepsRefusingUnknownProvider pins the labelled
// negative: a miss for a provider the harness does not know keeps denying
// spawn with a spawn failure — there is no wire API or credential mapping
// to select, so there is nothing to fall back onto. RED proof: route the
// unknown provider through the fallback anyway and this test fails because
// prepare stops denying.
func TestPrepare_CatalogMiss_KeepsRefusingUnknownProvider(t *testing.T) {
	t.Parallel()
	if _, miss := fallbackToInjectedProvider(agent.Spec{
		Model:    "mystery/m-1",
		Endpoint: &agent.EndpointBinding{BaseURL: "https://models.example.com/v1"},
	}, &catalogMissError{provider: "mystery", model: "m-1"}); miss == nil {
		t.Fatal("fallbackToInjectedProvider(unknown provider) = nil error, want the miss back (still a denial)")
	}
}

// TestPrepare_CatalogMiss_KeepsRefusingUnbound pins the second labelled
// negative: a miss with no bound endpoint keeps denying spawn — the injected
// provider registers only under a bound base URL, so there is no endpoint
// to route the fallback through.
func TestPrepare_CatalogMiss_KeepsRefusingUnbound(t *testing.T) {
	t.Parallel()
	p := &Provider{binary: "pi", opts: Options{
		CatalogProbe: func(_ context.Context, _, _, _, _, _ string) (string, error) {
			return "provider  model\n", nil
		},
	}}
	_, err := p.prepare(context.Background(), agent.Spec{
		Prompt: "hi",
		Cwd:    t.TempDir(),
		Model:  "zai/glm-9.9-does-not-exist",
	})
	if err == nil {
		t.Fatal("prepare succeeded for an unbound catalog miss; want a spawn denial")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("prepare error does not wrap agent.ErrSpawnFailed: %v", err)
	}
}

// TestSpawn_CatalogMiss_FallsBackToInjectedProvider drives the fallback
// through a full Spawn on a direct provider endpoint pinned to an id the
// scripted catalog lacks: the session spawns through the injected provider
// instead of denying.
func TestSpawn_CatalogMiss_FallsBackToInjectedProvider(t *testing.T) {
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
			return "provider  model\n", nil // confirmed miss
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := handshakeEvent("h1") + getStateResponse("ses_fallback") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	go func() { _, _ = io.WriteString(pw, body) }()
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:         "hi",
		Cwd:            t.TempDir(),
		Model:          "google/gemini-4-pro",
		Env:            map[string]string{PiKeyEnvVar: "cell-key"},
		Endpoint:       directGoogleEndpoint(),
		ProviderConfig: map[string]any{"maxOutputTokens": 64000},
	})
	if err != nil {
		t.Fatalf("Spawn through the injected fallback: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	drain(t, h)
}
