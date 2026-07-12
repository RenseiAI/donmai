package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestInteractiveArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{name: "empty prompt starts bare REPL", prompt: "", want: nil},
		{name: "non-empty prompt seeds the REPL", prompt: "fix the bug", want: []string{"fix the bug"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interactiveArgs(agent.Spec{Prompt: tt.prompt})
			if len(got) != len(tt.want) {
				t.Fatalf("interactiveArgs(%q) = %v, want %v", tt.prompt, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("interactiveArgs(%q)[%d] = %q, want %q", tt.prompt, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// newFakeInteractiveProvider builds a Provider whose binary is a fake-claude
// script, run under a real PTY by ptycli. Mirrors the PATH-shim technique in
// provider/harness/agycli's handle_test.go newFakeProvider.
func newFakeInteractiveProvider(t *testing.T, script string) *Provider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	p, err := New(Options{Binary: bin, LookPath: func(string) (string, error) { return bin, nil }})
	if err != nil {
		t.Fatalf("New(fake): %v", err)
	}
	return p
}

// TestSpawn_Interactive_RoutesThroughPTYNotHeadlessJSONL proves
// Spec.Interactive routes claude's Spawn through the ptycli path — not the
// headless clijsonl -p/--output-format stream-json path — end to end: the
// fake binary echoes its own argv (so a stray "-p" or "--output-format"
// would be visible) and the returned handle must satisfy
// agent.InteractiveCapable.
func TestSpawn_Interactive_RoutesThroughPTYNotHeadlessJSONL(t *testing.T) {
	t.Parallel()
	p := newFakeInteractiveProvider(t, `echo "argv: $@"`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}

	deadline := time.After(15 * time.Second)
	gotInit, gotResult := false, false
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			switch ev.(type) {
			case agent.InitEvent:
				gotInit = true
			case agent.ResultEvent:
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for events")
		}
	}
	if !gotInit {
		t.Error("never observed an InitEvent")
	}
}
