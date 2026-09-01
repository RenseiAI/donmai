package pi

import (
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestRPCArgs_ProjectsSessionNameAtStartup(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/tmp/session"}
	got := rpcArgs(layout, nil, launchPrompt, "", agent.Spec{SessionName: "chief-of-staff"})
	want := []string{"--mode", "rpc", "--no-extensions", "--approve", "--session-dir", "/tmp/session", "--name", "chief-of-staff"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rpcArgs = %q; want %q", got, want)
	}
}

func TestInteractiveArgs_ProjectsSessionNameAtStartup(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/tmp/session"}
	got := interactiveArgs(agent.Spec{SessionName: "chief-of-staff", Prompt: "coordinate"}, layout, nil)
	want := []string{"--no-extensions", "--approve", "--session-dir", "/tmp/session", "--name", "chief-of-staff", "coordinate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interactiveArgs = %q; want %q", got, want)
	}
}

// TestCodeIntelEnforcementNote_UnsetReturnsNil pins the non-note half of the
// contract: no CodeIntelEnforcement configured, no note (nothing to name).
func TestCodeIntelEnforcementNote_UnsetReturnsNil(t *testing.T) {
	t.Parallel()
	if note := codeIntelEnforcementNote(agent.Spec{}); note != nil {
		t.Errorf("codeIntelEnforcementNote(unset) = %+v, want nil", note)
	}
}

// TestCodeIntelEnforcementNote_SetNamesTheField pins the typed half: a set
// CodeIntelEnforcement returns a SpecFieldNote naming the field, not a nil
// (the silent-drop shape this codex-pattern note replaces).
func TestCodeIntelEnforcementNote_SetNamesTheField(t *testing.T) {
	t.Parallel()
	note := codeIntelEnforcementNote(agent.Spec{
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
	})
	if note == nil {
		t.Fatal("codeIntelEnforcementNote(set) = nil, want a typed note")
	}
	if note.Field != "CodeIntelEnforcement" {
		t.Errorf("note.Field = %q, want %q", note.Field, "CodeIntelEnforcement")
	}
	if note.Reason == "" {
		t.Error("note.Reason is empty; a note with no reason is not an improvement on silence")
	}
}

// TestNativeProviderPin is the table-driven proof of requirement 1's
// classification: a "<provider>/<model>" pin whose provider names one of
// pi's built-in providers routes NATIVELY (bareModel stripped, useNative
// true) unless the cell is a translating loopback gateway (Host:gateway —
// the "metered/gateway modes" the injected provider must carry); an
// unprefixed or unrecognized-prefix pin is never split at all, matching the
// pre-fix behavior byte-for-byte (requirement 3's "unchanged un-prefixed
// pins").
func TestNativeProviderPin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		model        string
		ep           *agent.EndpointBinding
		wantProvider string
		wantBare     string
		wantNative   bool
	}{
		{
			name:  "zai pin, no endpoint bound, routes natively",
			model: "zai/glm-5.3", ep: nil,
			wantProvider: "zai", wantBare: "glm-5.3", wantNative: true,
		},
		{
			name:         "zai pin, direct-host endpoint, routes natively",
			model:        "zai/glm-5.3",
			ep:           &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://api.z.ai/api/coding/paas/v4"},
			wantProvider: "zai", wantBare: "glm-5.3", wantNative: true,
		},
		{
			// provider is still reported (the pin WAS recognized) — only
			// useNative flips false; every production call site gates on
			// useNative before ever reading provider in this branch.
			name:         "builtin-prefixed pin on a translating loopback gateway stays on the injected provider",
			model:        "zai/glm-5.3",
			ep:           &agent.EndpointBinding{Host: agent.HostGateway, BaseURL: "http://127.0.0.1:7734/v1"},
			wantProvider: "zai", wantBare: "glm-5.3", wantNative: false,
		},
		{
			name:         "unprefixed pin (claude-opus-4-8) is never split, bound or not",
			model:        "claude-opus-4-8",
			ep:           &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://api.anthropic.com"},
			wantProvider: "", wantBare: "claude-opus-4-8", wantNative: false,
		},
		{
			name:  "unprefixed pin (gpt-5.4) is never split",
			model: "gpt-5.4", ep: nil,
			wantProvider: "", wantBare: "gpt-5.4", wantNative: false,
		},
		{
			name:         "unrecognized aggregator-style prefix is never split",
			model:        "agg-vendor/claude-3-haiku",
			ep:           &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://ai-gateway.invalid/v1"},
			wantProvider: "", wantBare: "agg-vendor/claude-3-haiku", wantNative: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, bare, native := nativeProviderPin(tc.model, tc.ep)
			if provider != tc.wantProvider || bare != tc.wantBare || native != tc.wantNative {
				t.Errorf("nativeProviderPin(%q, %+v) = (%q, %q, %v), want (%q, %q, %v)",
					tc.model, tc.ep, provider, bare, native, tc.wantProvider, tc.wantBare, tc.wantNative)
			}
		})
	}
}

// TestModelPinArgs_NativeBuiltinProviderRouting pins requirement 1's argv
// half: a builtin-prefixed pin on a non-gateway cell selects the built-in
// provider natively with the BARE model id (never "--provider donmai" and
// never the prefixed string), and the reasoning-effort suffix still applies
// to the bare id.
func TestModelPinArgs_NativeBuiltinProviderRouting(t *testing.T) {
	t.Parallel()
	got := modelPinArgs(agent.Spec{
		Model:  "zai/glm-5.3",
		Effort: agent.EffortHigh,
		Endpoint: &agent.EndpointBinding{
			Host: agent.HostDirect, BaseURL: "https://api.z.ai/api/coding/paas/v4",
		},
	})
	want := []string{"--provider", "zai", "--model", "glm-5.3:high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelPinArgs = %q; want %q", got, want)
	}
}

// TestModelPinArgs_GatewayKeepsInjectedProviderWithBareModel pins
// requirement 1's second sentence: a builtin-prefixed pin on a translating
// loopback gateway cell stays on "--provider donmai" (the injected
// provider), but the model half is ALWAYS bare — never the prefixed string
// this package fixes sent upstream verbatim.
func TestModelPinArgs_GatewayKeepsInjectedProviderWithBareModel(t *testing.T) {
	t.Parallel()
	got := modelPinArgs(agent.Spec{
		Model: "zai/glm-5.3",
		Endpoint: &agent.EndpointBinding{
			Host: agent.HostGateway, BaseURL: "http://127.0.0.1:7734/v1",
		},
	})
	want := []string{"--provider", pinnedProviderName, "--model", "glm-5.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelPinArgs = %q; want %q — the injected provider must never see the prefixed model id", got, want)
	}
}

// TestModelPinArgs_UnprefixedPinsUnchanged is requirement 3's explicit
// "unchanged un-prefixed pins" proof: claude-opus-4-8 and gpt-5.4, bound and
// unbound, produce the exact argv this package emitted before this fix —
// nativeProviderPin never engages for a pin with no recognized
// "<builtin-provider>/" prefix.
func TestModelPinArgs_UnprefixedPinsUnchanged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec agent.Spec
		want []string
	}{
		{
			name: "claude-opus-4-8, unbound",
			spec: agent.Spec{Model: "claude-opus-4-8"},
			want: []string{"--model", "claude-opus-4-8"},
		},
		{
			name: "claude-opus-4-8, bound to a direct endpoint",
			spec: agent.Spec{Model: "claude-opus-4-8", Endpoint: &agent.EndpointBinding{Host: agent.HostDirect, BaseURL: "https://api.anthropic.com"}},
			want: []string{"--provider", pinnedProviderName, "--model", "claude-opus-4-8"},
		},
		{
			name: "gpt-5.4, unbound",
			spec: agent.Spec{Model: "gpt-5.4"},
			want: []string{"--model", "gpt-5.4"},
		},
		{
			name: "gpt-5.4, bound to a gateway endpoint",
			spec: agent.Spec{Model: "gpt-5.4", Endpoint: &agent.EndpointBinding{Host: agent.HostGateway, BaseURL: "http://127.0.0.1:7734/v1"}},
			want: []string{"--provider", pinnedProviderName, "--model", "gpt-5.4"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := modelPinArgs(tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("modelPinArgs(%+v) = %q, want %q", tc.spec, got, tc.want)
			}
		})
	}
}

// TestApplyEndpoint_MirrorsBuiltinProviderCredential is requirement 1's
// credential-routing half: a builtin-prefixed pin on a non-gateway cell gets
// the resolved key ALSO mirrored onto the built-in provider's own env var
// (BYOK), in addition to the existing PiKeyEnvVar mirror (harmless — the
// injected provider stays registered either way).
func TestApplyEndpoint_MirrorsBuiltinProviderCredential(t *testing.T) {
	t.Parallel()
	spec, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyOpenAI,
			BaseURL:  "https://api.z.ai/api/coding/paas/v4",
			Host:     agent.HostDirect,
			Protocol: agent.ProtoOpenAIChat,
			Model:    "zai/glm-5.3",
			Env:      map[string]string{"OPENAI_API_KEY": "resolved-control-plane-key"},
		},
	})
	if err != nil {
		t.Fatalf("applyEndpoint: %v", err)
	}
	if spec.Env["ZAI_API_KEY"] != "resolved-control-plane-key" {
		t.Errorf("resolved key not mirrored onto ZAI_API_KEY (BYOK): %v", spec.Env)
	}
	if spec.Env[PiKeyEnvVar] != "resolved-control-plane-key" {
		t.Errorf("PiKeyEnvVar mirror regressed: %v", spec.Env)
	}
	if spec.Model != "zai/glm-5.3" {
		t.Errorf("applyEndpoint must not itself strip the pin — that's providerPinEnv/modelPinArgs' job: spec.Model = %q", spec.Model)
	}
}

// TestApplyEndpoint_GatewayDoesNotMirrorBuiltinProviderCredential proves the
// negative: on a translating loopback gateway cell, the SAME builtin-prefixed
// pin must NOT get a native-provider credential mirror — selecting pi's own
// "zai" provider natively would silently bypass the gateway's loopback
// baseURL instead of routing through it.
func TestApplyEndpoint_GatewayDoesNotMirrorBuiltinProviderCredential(t *testing.T) {
	t.Parallel()
	spec, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyOpenAI,
			BaseURL:  "http://127.0.0.1:7734/v1",
			Host:     agent.HostGateway,
			Protocol: agent.ProtoOpenAIChat,
			Model:    "zai/glm-5.3",
			Env:      map[string]string{"OPENAI_API_KEY": "resolved-control-plane-key"},
		},
	})
	if err != nil {
		t.Fatalf("applyEndpoint: %v", err)
	}
	if _, present := spec.Env["ZAI_API_KEY"]; present {
		t.Errorf("gateway cell must not get a native builtin-provider credential mirror: %v", spec.Env)
	}
	if spec.Env[PiKeyEnvVar] != "resolved-control-plane-key" {
		t.Errorf("PiKeyEnvVar mirror regressed for the gateway cell: %v", spec.Env)
	}
}

// TestSpawn_CodeIntelEnforcementDeniedPreTurn is the live-session half of the
// same claim: a Spec carrying CodeIntelEnforcement must surface the typed
// denial on the event stream, before the prompt command reaches the wire —
// the "pre-spawn" half of "typed pre-spawn denial". A caller draining events
// for this session sees the drop; nothing here silently proceeds as if the
// field had been honored.
func TestSpawn_CodeIntelEnforcementDeniedPreTurn(t *testing.T) {
	t.Parallel()
	body := getStateResponse("ses_ci") +
		event(map[string]any{"type": "agent_start"}) +
		event(map[string]any{"type": "agent_settled"})
	cmds, h, err := spawnScripted(t, agent.Spec{
		Prompt:               "hi",
		CodeIntelEnforcement: &agent.CodeIntelEnforcement{EnforceUsage: true},
	}, handshakeEvent("h1"), body)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	evs := drain(t, h)

	var found bool
	for _, ev := range evs {
		sys, ok := ev.(agent.SystemEvent)
		if !ok {
			continue
		}
		if sys.Subtype == codeIntelEnforcementUnsupportedSubtype {
			found = true
			if sys.Message == "" {
				t.Errorf("code-intel-enforcement SystemEvent carries no Message")
			}
		}
	}
	if !found {
		t.Fatalf("no %q SystemEvent in the drained stream (%d events); CodeIntelEnforcement was silently dropped", codeIntelEnforcementUnsupportedSubtype, len(evs))
	}

	// "Pre-spawn" means the notice is decided and emitted before the turn is
	// dispatched, not merely before the caller happens to notice: launch()
	// calls h.emit for this note before it writes the "prompt" wire command,
	// so the prompt command must still have gone out (the field is a denial
	// of ITSELF, never of the session).
	var sawPrompt bool
	for _, cmd := range cmds.commands() {
		if cmd["type"] == "prompt" {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("prompt command never sent; CodeIntelEnforcement must not block the session, only its own enforcement")
	}
}
