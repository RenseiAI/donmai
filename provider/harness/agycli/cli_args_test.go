package agycli

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestBuildArgs(t *testing.T) {
	t.Parallel()
	argv := buildArgs(agent.Spec{Prompt: "do the thing"}, false)
	// Expect: -p <prompt> --dangerously-skip-permissions
	if len(argv) != 3 {
		t.Fatalf("argv len = %d, want 3: %v", len(argv), argv)
	}
	if argv[0] != "-p" {
		t.Errorf("argv[0] = %q, want -p", argv[0])
	}
	if argv[1] != "do the thing" {
		t.Errorf("argv[1] = %q, want prompt value", argv[1])
	}
	if argv[2] != "--dangerously-skip-permissions" {
		t.Errorf("argv[2] = %q, want --dangerously-skip-permissions", argv[2])
	}
}

func TestBuildArgs_NoModelFlag(t *testing.T) {
	t.Parallel()
	// agy has no --model flag; spec.Model must NOT leak into argv.
	argv := buildArgs(agent.Spec{Prompt: "x", Model: "gemini-3-pro"}, false)
	for _, a := range argv {
		if a == "--model" || a == "gemini-3-pro" {
			t.Fatalf("model leaked into argv: %v", argv)
		}
	}
}

func TestBuildPrompt_Inject(t *testing.T) {
	t.Parallel()
	got := buildPrompt("base task", true)
	if !strings.Contains(got, "base task") {
		t.Errorf("injected prompt lost the base: %q", got)
	}
	if !strings.Contains(got, resultEnvelopeBegin) || !strings.Contains(got, resultEnvelopeEnd) {
		t.Errorf("injected prompt missing envelope markers: %q", got)
	}
}

func TestBuildPrompt_NoInject(t *testing.T) {
	t.Parallel()
	got := buildPrompt("base task", false)
	if got != "base task" {
		t.Errorf("no-inject changed prompt: %q", got)
	}
}

func TestBuildPrompt_Idempotent(t *testing.T) {
	t.Parallel()
	// A prompt that already contains the begin marker must not be double-injected.
	pre := "task\n" + resultEnvelopeBegin + "\n{}\n" + resultEnvelopeEnd
	got := buildPrompt(pre, true)
	if got != pre {
		t.Errorf("double-injected an existing envelope: %q", got)
	}
	if strings.Count(got, resultEnvelopeBegin) != 1 {
		t.Errorf("expected exactly one begin marker, got %d", strings.Count(got, resultEnvelopeBegin))
	}
}

func TestComposeEnv_NoKeyInjected(t *testing.T) {
	t.Parallel()
	parent := []string{"PATH=/usr/bin", "HOME=/home/u"}
	out := composeEnv(parent, map[string]string{"FOO": "bar", "BAZ": "qux"})
	joined := strings.Join(out, "\n")
	// parent preserved
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("parent env dropped: %v", out)
	}
	// spec.Env merged, deterministic (sorted) order: BAZ before FOO
	bazIdx := indexOfPrefix(out, "BAZ=")
	fooIdx := indexOfPrefix(out, "FOO=")
	if bazIdx < 0 || fooIdx < 0 || bazIdx > fooIdx {
		t.Errorf("spec.Env not merged in sorted order: %v", out)
	}
	// No API key injected by the provider.
	if strings.Contains(joined, "GEMINI_API_KEY") || strings.Contains(joined, "API_KEY=") {
		t.Errorf("provider injected an API key: %v", out)
	}
}

func indexOfPrefix(ss []string, prefix string) int {
	for i, s := range ss {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}
