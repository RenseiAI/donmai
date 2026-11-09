package opencode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func headerSpec(env map[string]string) agent.Spec {
	return agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: "gpt-4o-mini", BaseURL: "https://compat.example/v1",
			Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect, Mechanism: agent.AuthAPIKey,
			Env: map[string]string{"OPENAI_API_KEY": "k"},
		},
		Env: env,
	}
}

// TestBuildConfig_ProviderHeaders proves declared headers land in the injected
// provider's options.headers, and that no declaration renders the provider
// block byte-identical to a session that never carried the env var.
func TestBuildConfig_ProviderHeaders(t *testing.T) {
	t.Parallel()
	render := func(env map[string]string) string {
		t.Helper()
		spec, err := applyEndpoint(headerSpec(env))
		if err != nil {
			t.Fatalf("applyEndpoint: %v", err)
		}
		raw, err := json.Marshal(buildConfig(spec).Provider)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	today := `{"donmai":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"https://compat.example/v1","apiKey":"{env:` + OCKeyEnvVar + `}"},"models":{"gpt-4o-mini":{}}}}`
	if got := render(map[string]string{"OTHER": "v"}); got != today {
		t.Fatalf("absent declaration changed the provider block:\n%s\nwant\n%s", got, today)
	}
	got := render(map[string]string{agent.ProviderHTTPHeadersEnv: "x-title: Example App\nhttp-referer: https://example.com"})
	if want := `"headers":{"http-referer":"https://example.com","x-title":"Example App"}`; !strings.Contains(got, want) {
		t.Fatalf("provider block %s missing %s", got, want)
	}
}

func TestApplyEndpoint_RefusesProviderHeaderSubstitutionAndMalformed(t *testing.T) {
	t.Parallel()
	var typed *agent.ProviderHTTPHeadersError
	for _, decl := range []string{"x-title: {env:OPENAI_API_KEY}", "x-title: {file:~/.ssh/id_ed25519}"} {
		if _, err := applyEndpoint(headerSpec(map[string]string{agent.ProviderHTTPHeadersEnv: decl})); !errors.As(err, &typed) || !strings.Contains(err.Error(), "substitution") {
			t.Errorf("applyEndpoint(%q) err = %v; want a typed substitution refusal", decl, err)
		}
	}
	if _, err := applyEndpoint(agent.Spec{Env: map[string]string{agent.ProviderHTTPHeadersEnv: "host: example.com"}}); !errors.As(err, &typed) {
		t.Fatalf("unbound applyEndpoint err = %v; want *agent.ProviderHTTPHeadersError", err)
	}
}
