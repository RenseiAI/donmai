package codex

import (
	"strings"
	"testing"
)

func TestMergeEnv_StripsRunnerOnlyAttachControls(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	ownedHome := t.TempDir()
	got := mergeEnv(map[string]string{
		"ATTACH_TOKEN":      "override-secret",
		"ATTACH_TOKEN_FILE": "/override/token",
		"ATTACH_URL":        "wss://override.invalid/v1/rooms/room-1",
		"CODEX_HOME":        "/untrusted/override",
		"SAFE":              "kept",
	}, ownedHome)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "ATTACH_") {
		t.Fatal("runner-only attach controls reached codex app-server environment")
	}
	if !strings.Contains(joined, "SAFE=kept") {
		t.Fatalf("safe override missing from codex environment: %v", got)
	}
	var codexHomes []string
	for _, entry := range got {
		if strings.HasPrefix(entry, "CODEX_HOME=") {
			codexHomes = append(codexHomes, strings.TrimPrefix(entry, "CODEX_HOME="))
		}
	}
	if len(codexHomes) == 0 || codexHomes[len(codexHomes)-1] != ownedHome {
		t.Fatal("owned CODEX_HOME did not win child environment composition")
	}
}
