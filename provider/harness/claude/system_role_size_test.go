package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func fakeNulArgvCLI(t *testing.T, dir string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv.raw")
	script := "#!/bin/sh\n" +
		"printf '%s\\0' \"$@\" > " + shQuote(argvPath) + "\n" +
		"cat > /dev/null\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-role"}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	return writeFakeCLI(t, "fake-claude-role.sh", script)
}

func TestSpawn_LongRoleReachesChildArgv(t *testing.T) {
	for _, size := range []int{32768, 38912, 80000} {
		role := sizedRole(t, size)
		dir := t.TempDir()
		cli := fakeNulArgvCLI(t, dir)
		p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		h, err := p.Spawn(ctx, agent.Spec{Prompt: "work", Cwd: dir, Autonomous: true, SystemPromptAppend: role})
		if err != nil {
			cancel()
			t.Fatalf("size %d: Spawn: %v", size, err)
		}
		_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 30*time.Second)
		_ = h.Stop(context.Background())
		cancel()
		raw, err := os.ReadFile(filepath.Join(dir, "argv.raw"))
		if err != nil {
			t.Fatalf("size %d: read argv: %v", size, err)
		}
		parts := bytes.Split(raw, []byte{0})
		found := 0
		for i, part := range parts {
			if string(part) == "--append-system-prompt" && i+1 < len(parts) && string(parts[i+1]) == role {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("size %d: role occurrences %d, want 1", size, found)
		}
	}
}
