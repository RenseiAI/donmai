package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestInteractiveArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec agent.Spec
		want []string
	}{
		{
			name: "empty prompt starts bare REPL",
			spec: agent.Spec{},
			want: nil,
		},
		{
			name: "non-empty prompt seeds the REPL",
			spec: agent.Spec{Prompt: "fix the bug"},
			want: []string{"fix the bug"},
		},
		{
			name: "autonomous adds bypassPermissions before the prompt",
			spec: agent.Spec{Prompt: "fix the bug", Autonomous: true},
			want: []string{"--permission-mode", "bypassPermissions", "fix the bug"},
		},
		{
			name: "autonomous with empty prompt is still a valid flag-only argv",
			spec: agent.Spec{Autonomous: true},
			want: []string{"--permission-mode", "bypassPermissions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := interactiveArgs(tt.spec)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("interactiveArgs(%+v) = %q, want %q", tt.spec, got, tt.want)
			}
		})
	}
}

// TestInteractiveArgs_AutonomousMatchesHeadlessPermissionMode is the
// regression guard for the defect this mapping fixes: Spec.Autonomous used to
// be DROPPED on the interactive path, so the same Spec produced a bypassing
// headless run and a default-permission-mode interactive REPL. It asserts the
// two spawn modes agree by comparing against buildArgs' own output rather than
// a hard-coded string, so a future edit to the headless flag pair cannot
// silently re-open the divergence.
func TestInteractiveArgs_AutonomousMatchesHeadlessPermissionMode(t *testing.T) {
	t.Parallel()

	const flag = "--permission-mode"

	permissionModeOf := func(argv []string) (string, bool) {
		i := slices.Index(argv, flag)
		if i < 0 || i+1 >= len(argv) {
			return "", false
		}
		return argv[i+1], true
	}

	t.Run("autonomous", func(t *testing.T) {
		t.Parallel()
		spec := agent.Spec{Prompt: "ship it", Autonomous: true}

		headless, _ := buildArgs(spec, "", "")
		headlessMode, ok := permissionModeOf(headless)
		if !ok {
			t.Fatalf("headless buildArgs dropped %s for an autonomous spec: %q", flag, headless)
		}

		interactiveMode, ok := permissionModeOf(interactiveArgs(spec))
		if !ok {
			t.Fatalf("interactiveArgs dropped %s for an autonomous spec: %q", flag, interactiveArgs(spec))
		}
		if interactiveMode != headlessMode {
			t.Errorf("permission-mode divergence: interactive=%q headless=%q", interactiveMode, headlessMode)
		}
		if interactiveMode != "bypassPermissions" {
			t.Errorf("permission-mode = %q, want %q", interactiveMode, "bypassPermissions")
		}
	})

	t.Run("not autonomous", func(t *testing.T) {
		t.Parallel()
		spec := agent.Spec{Prompt: "ship it"}

		headless, _ := buildArgs(spec, "", "")
		if _, ok := permissionModeOf(headless); ok {
			t.Errorf("headless buildArgs emitted %s for a non-autonomous spec: %q", flag, headless)
		}
		got := interactiveArgs(spec)
		if slices.Contains(got, flag) {
			t.Errorf("interactiveArgs emitted %s for a non-autonomous spec: %q", flag, got)
		}
		// The REPL must be left to the CLI's own default, not silently
		// widened — no permission flag of any shape.
		for _, a := range got {
			if strings.HasPrefix(a, "--permission") || a == "--dangerously-skip-permissions" {
				t.Errorf("non-autonomous interactive argv carries a permission flag %q: %q", a, got)
			}
		}
	})
}

// TestInteractiveArgs_PromptIsAlwaysLast pins the positional-argument
// invariant: claude parses a bare prompt positionally, so any flag emitted
// after it would swallow it (or the prompt would be read as a flag value).
func TestInteractiveArgs_PromptIsAlwaysLast(t *testing.T) {
	t.Parallel()
	const prompt = "fix the bug"
	for _, autonomous := range []bool{false, true} {
		t.Run(map[bool]string{false: "not autonomous", true: "autonomous"}[autonomous], func(t *testing.T) {
			t.Parallel()
			got := interactiveArgs(agent.Spec{Prompt: prompt, Autonomous: autonomous})
			if len(got) == 0 {
				t.Fatal("interactiveArgs returned an empty argv for a non-empty prompt")
			}
			if last := got[len(got)-1]; last != prompt {
				t.Errorf("last argv element = %q, want the prompt %q (full argv: %q)", last, prompt, got)
			}
			if n := slices.Index(got, prompt); n != len(got)-1 {
				t.Errorf("prompt appears at index %d, want %d (full argv: %q)", n, len(got)-1, got)
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
