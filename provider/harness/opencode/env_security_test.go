package opencode

import (
	"strings"
	"testing"
)

func TestComposeEnv_StripsRunnerOnlyAttachControls(t *testing.T) {
	t.Parallel()

	got := composeEnv(
		[]string{"PATH=/usr/bin", "ATTACH_TOKEN=parent-secret", "ATTACH_URL=wss://parent.invalid"},
		map[string]string{
			"ATTACH_TOKEN":      "override-secret",
			"ATTACH_TOKEN_FILE": "/override/token",
			"SAFE":              "kept",
		},
	)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "ATTACH_") {
		t.Fatal("runner-only attach controls reached opencode child environment")
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "SAFE=kept") {
		t.Fatalf("safe environment entries missing: %v", got)
	}
}
