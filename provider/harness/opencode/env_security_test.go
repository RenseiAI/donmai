package opencode

import (
	"strings"
	"testing"
)

func TestComposeEnv_StripsInheritedWorkerControls(t *testing.T) {
	t.Parallel()

	got := composeEnv(
		[]string{
			"PATH=/usr/bin",
			"ATTACH_TOKEN=parent-secret",
			"ATTACH_URL=wss://parent.invalid",
			"DONMAI_GATEWAY_UPSTREAM_API_KEY=worker-secret",
			"DONMAI_GATEWAY_UPSTREAM_BASE_URL=https://worker.invalid/private",
			"OPENAI_API_KEY=parent-provider-secret",
		},
		map[string]string{
			"ATTACH_TOKEN":      "override-secret",
			"ATTACH_TOKEN_FILE": "/override/token",
			"OPENAI_API_KEY":    "session-provider-secret",
			"SAFE":              "kept",
		},
	)
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{
		"ATTACH_",
		"DONMAI_GATEWAY_UPSTREAM_API_KEY=",
		"DONMAI_GATEWAY_UPSTREAM_BASE_URL=",
		"OPENAI_API_KEY=parent-provider-secret",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("inherited worker control %q reached opencode child environment", forbidden)
		}
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=session-provider-secret",
		"SAFE=kept",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected environment entry %q missing: %v", want, got)
		}
	}
}

func TestFilterEndpointControls_StripsAmbientRoutesAndCredentials(t *testing.T) {
	t.Parallel()

	got := filterEndpointControls([]string{
		"PATH=/usr/bin",
		OCConfigEnvVar + "=/tmp/ambient-opencode.json",
		OCKeyEnvVar + "=ambient-session-key",
		EnvEndpoint + "=http://ambient.invalid",
		EnvAPIKey + "=ambient-api-key",
		"OPENAI_API_KEY=ambient-openai-key",
		"OPENAI_BASE_URL=http://ambient-openai.invalid/v1",
		"OPENAI_MODEL=ambient-model",
		"OPENCODE_MODEL=ambient-provider/model",
		"SAFE=kept",
	})
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{
		OCConfigEnvVar + "=", OCKeyEnvVar + "=", EnvEndpoint + "=", EnvAPIKey + "=",
		"OPENAI_API_KEY=", "OPENAI_BASE_URL=", "OPENAI_MODEL=", "OPENCODE_MODEL=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("ambient endpoint control %q survived: %v", forbidden, got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "SAFE=kept"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("unrelated environment entry %q missing: %v", want, got)
		}
	}
}
