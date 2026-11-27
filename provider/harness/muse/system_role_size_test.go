package muse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// No declared role-byte cap on this adapter; each successful probe below is
// a lower bound only. Units are UTF-8 bytes of the framed role.
func sizedRole(t *testing.T, n int) string {
	t.Helper()
	start := "[[ROLEBEGIN-9f3a]]"
	end := "]]-9f3a-ROLEEND-\u2713"
	chunk := "q\u00e9\u2713\n"
	overhead := len(start) + len(end)
	if n <= overhead {
		t.Fatalf("fixture size %d too small", n)
	}
	middleLen := n - overhead
	role := start + strings.Repeat(chunk, middleLen/len(chunk)) + strings.Repeat("q", middleLen%len(chunk)) + end
	if len(role) != n {
		t.Fatalf("fixture is %d bytes, want %d", len(role), n)
	}
	return role
}

func turnLeadingText(t *testing.T, fh *fakeHost) string {
	t.Helper()
	fh.mu.Lock()
	defer fh.mu.Unlock()
	for i, m := range fh.methods {
		if m != "turn/start" {
			continue
		}
		input, _ := fh.params[i]["input"].([]any)
		if len(input) == 0 {
			continue
		}
		first, _ := input[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}
	t.Fatal("no turn/start with input reached the host")
	return ""
}

func TestSpawn_LongRoleReachesTurnStart(t *testing.T) {
	for _, size := range []int{32768, 38912, 80000} {
		role := sizedRole(t, size)
		p, fh := newTestProvider(t, "single-turn")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true, SystemPromptAppend: role})
		if err != nil {
			cancel()
			t.Fatalf("size %d: Spawn: %v", size, err)
		}
		_ = drainEvents(t, h.Events(), 10*time.Second)
		_ = h.Stop(context.Background())
		cancel()
		if got := turnLeadingText(t, fh); got != role {
			t.Fatalf("size %d: leading text mismatch", size)
		}
	}
}
