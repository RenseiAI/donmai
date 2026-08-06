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
	// Model selection is intentionally not wired by this provider yet; Spec.Model
	// must not leak into argv until it has a dedicated compatibility contract.
	argv := buildArgs(agent.Spec{Prompt: "x", Model: "gemini-3-pro"}, false)
	for _, a := range argv {
		if a == "--model" || a == "gemini-3-pro" {
			t.Fatalf("model leaked into argv: %v", argv)
		}
	}
}

func TestBuildArgs_CwdAddsOnlyExplicitWorktree(t *testing.T) {
	t.Parallel()
	worktree := "/tmp/worktree with spaces; literal"
	argv := buildArgs(agent.Spec{Prompt: "x", Cwd: worktree}, false)
	want := []string{"-p", "x", "--dangerously-skip-permissions", "--add-dir", worktree}
	if len(argv) != len(want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q; argv=%#v", i, argv[i], want[i], argv)
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

func TestBuildPrompt_UserMarkerDoesNotSuppressHarnessEnvelope(t *testing.T) {
	t.Parallel()
	// A user marker is untrusted data, not authority to suppress the harness
	// protocol instruction.
	pre := "task\n" + resultEnvelopeBegin + "\n{}\n" + resultEnvelopeEnd
	got := buildPrompt(pre, true)
	if !strings.HasPrefix(got, pre) {
		t.Errorf("injected prompt lost caller text: %q", got)
	}
	if strings.Count(got, resultEnvelopeBegin) != 2 {
		t.Errorf("expected caller marker plus harness marker, got %d", strings.Count(got, resultEnvelopeBegin))
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

func TestComposeEnv_StripsRunnerOnlyControls(t *testing.T) {
	t.Parallel()

	out := composeEnv(
		[]string{"PATH=/usr/bin", "ATTACH_TOKEN=parent-secret", "ATTACH_URL=wss://parent.invalid"},
		map[string]string{
			"ATTACH_TOKEN":      "spec-secret",
			"ATTACH_TOKEN_FILE": "/tmp/token",
			"SAFE":              "kept",
		},
	)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "ATTACH_") {
		t.Fatalf("runner-only attach controls reached agy child: %v", out)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "SAFE=kept") {
		t.Fatalf("composeEnv dropped safe entries: %v", out)
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
