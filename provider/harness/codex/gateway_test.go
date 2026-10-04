package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func gatewayTestSpec(baseURL, key string) agent.Spec {
	env := map[string]string{}
	if key != "" {
		env[codexGatewayEnvKey] = key
	}
	return agent.Spec{
		Endpoint: &agent.EndpointBinding{
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIResponses,
		},
		Env: env,
	}
}

func TestGatewayBinding_NoBaseURLUsesTodayConfig(t *testing.T) {
	t.Parallel()
	_, _, ok, err := gatewayBinding(agent.Spec{})
	if err != nil || ok {
		t.Fatalf("gatewayBinding(empty) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestGatewayBinding_BaseURLNeedsResponsesProtocol(t *testing.T) {
	t.Parallel()
	spec := gatewayTestSpec("https://gateway.example/codex/v1", "k")
	spec.Endpoint.Protocol = agent.ProtoOpenAIChat
	if _, _, _, err := gatewayBinding(spec); err == nil {
		t.Fatal("gatewayBinding(chat protocol) = nil error, want a loud refusal")
	}
}

func TestGatewayBinding_BaseURLNeedsKey(t *testing.T) {
	t.Parallel()
	if _, _, _, err := gatewayBinding(gatewayTestSpec("https://gateway.example/codex/v1", "")); err == nil {
		t.Fatal("gatewayBinding without key = nil error, want a loud refusal")
	}
}

func TestGatewayProviderBlock_RoundTrip(t *testing.T) {
	t.Parallel()
	boundary, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })

	baseURL := "https://gateway.example/codex/v1"
	env, err := boundary.appendGatewayProviderBlock(baseURL, map[string]string{codexGatewayEnvKey: "k"})
	if err != nil {
		t.Fatalf("append block: %v", err)
	}
	if env[codexGatewayEnvKey] != "k" {
		t.Fatalf("projected env lost the key: %v", env)
	}
	body, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`model_provider = "donmai-gateway"`,
		`[model_providers.donmai-gateway]`,
		`base_url = "https://gateway.example/codex/v1"`,
		`env_key = "AI_GATEWAY_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	// Idempotent: a second append leaves the single block alone.
	before := text
	if _, err := boundary.appendGatewayProviderBlock(baseURL, map[string]string{}); err != nil {
		t.Fatalf("second append: %v", err)
	}
	after, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != before {
		t.Fatalf("second append changed the config:\n%s", after)
	}
}

func TestGatewayBinding_EndpointEnvBeatsSpecEnv(t *testing.T) {
	t.Parallel()
	spec := gatewayTestSpec("https://gateway.example/codex/v1", "spec-key")
	spec.Endpoint.Env = map[string]string{codexGatewayEnvKey: "cell-key"}
	_, _, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
}
