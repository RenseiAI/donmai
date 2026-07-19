package gemini

import (
	"strings"
	"testing"
)

func TestToolExecutorProcessEnv_StripsRunnerOnlyAttachControls(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	exec := newToolExecutor("", map[string]string{
		"ATTACH_TOKEN":      "override-secret",
		"ATTACH_TOKEN_FILE": "/override/token",
		"ATTACH_URL":        "wss://override.invalid/v1/rooms/room-1",
		"SAFE":              "kept",
	}, nil)
	got := exec.processEnv()
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "ATTACH_") {
		t.Fatal("runner-only attach controls reached Gemini Bash environment")
	}
	if !strings.Contains(joined, "SAFE=kept") {
		t.Fatalf("safe session environment missing: %v", got)
	}
}
