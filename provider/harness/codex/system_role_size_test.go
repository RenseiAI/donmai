package codex

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

func TestSpawn_LongRoleReachesThreadStart(t *testing.T) {
	for _, size := range []int{32768, 38912, 80000} {
		role := sizedRole(t, size)
		fs, stdinW, stdoutR := newFakeServer()
		p, err := New(Options{skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR, verifyMCPReadback: true, configTempDir: t.TempDir()})
		if err != nil {
			fs.close()
			t.Fatalf("size %d: New: %v", size, err)
		}
		go fs.run(t, "thread-role")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: t.TempDir(), Autonomous: true, SystemPromptAppend: role})
		if err != nil {
			cancel()
			fs.close()
			t.Fatalf("size %d: Spawn: %v", size, err)
		}
		_ = drainEvents(t, h.Events(), 10*time.Second)
		_ = h.Stop(context.Background())
		_ = p.Shutdown(context.Background())
		fs.close()
		cancel()
		if got := developerInstructions(t, fs); got != role {
			t.Fatalf("size %d: developerInstructions mismatch", size)
		}
	}
}
