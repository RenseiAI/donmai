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
