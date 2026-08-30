package pi

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// lossCommandLine is the VERBATIM bash one-liner a pi seat ran on 2026-08-29.
// Its third segment removed the live session's own storage; the session then
// ran on against paths that no longer existed, showing "Working…" forever
// instead of failing. This exact string is the control for the guard: before
// the guard it was allowed, after it, it is refused.
const lossCommandLine = `cat .gitignore | grep ".pi" 2>&1 | head -5; ls .pi/ 2>&1 | head -20; rm -rf .pi 2>&1; git status --short 2>&1`

// TestPolicy_StateDirDeletionDenied pins the guard through the full
// adjudicator, with an allow-everything configuration in place: like the other
// built-in safety rules, no allow pattern may override it.
func TestPolicy_StateDirDeletionDenied(t *testing.T) {
	t.Parallel()
	cwd := "/work/repo"
	stateDir := filepath.Join(cwd, piStateDir)

	// An allow-all config, so a passing case proves the built-in rule fired
	// rather than the default decision.
	engine := NewPolicyEngine(agent.Spec{
		Cwd:        cwd,
		Autonomous: true,
		PermissionConfig: &agent.PermissionConfig{
			AllowPatterns:   []string{".*"},
			DefaultDecision: "allow",
		},
	})

	cases := []struct {
		name      string
		command   string
		wantAllow bool
	}{
		// --- the incident, and the deletion shapes around it ---
		{"the 2026-08-29 command line", lossCommandLine, false},
		{"bare rm -rf", "rm -rf .pi", false},
		{"dot-slash form", "rm -rf ./.pi", false},
		{"trailing slash form", "rm -rf .pi/", false},
		{"quoted operand", `rm -rf ".pi"`, false},
		{"single-quoted operand", "rm -rf '.pi'", false},
		{"absolute layout path", "rm -rf " + stateDir, false},
		{"a file inside the state dir", "rm " + filepath.Join(stateDir, "session.jsonl"), false},
		{"a subdirectory of the state dir", "rm -rf .pi/agent-home", false},
		{"glob inside the state dir", "rm -rf .pi/*", false},
		{"rmdir", "rmdir .pi", false},
		{"unlink on the session file", "unlink .pi/session.jsonl", false},
		{"env-prefixed", "FOO=1 rm -rf .pi", false},
		{"leading path to the binary", "/bin/rm -rf .pi", false},
		{"deletion in a later segment", "echo hi && cd /tmp || rm -rf .pi", false},
		{"deletion behind a redirect", "rm -rf .pi > /dev/null 2>&1", false},
		{"moving the state dir away", "mv .pi /tmp/parked", false},
		{"find -delete rooted at the state dir", "find .pi -name '*.jsonl' -delete", false},
		{"find -exec rm rooted at the state dir", "find .pi -type f -exec rm {} +", false},
		{"forced git clean naming the state dir", "git clean -fd .pi", false},
		{"unrestricted forced git clean", "git clean -fdx", false},
		{"unrestricted forced git clean, long flag", "git clean --force -d", false},
		{"git clean via -C", "git -C /work/repo clean -fd", false},

		// --- negatives: unrelated deletions stay allowed ---
		{"rm of a build directory", "rm -rf build/", true},
		{"rm of a similarly-named directory", "rm -rf pipeline/", true},
		{"rm of a prefix-sharing directory", "rm -rf .pi-cache", true},
		{"rm of a directory named pi", "rm -rf pi/", true},
		{"rm of a nested path that merely mentions pi", "rm -rf src/pipeline/.pipe", true},
		{"reading the state dir", "ls .pi", true},
		{"listing the state dir with a flag", "ls -la .pi/", true},
		{"grepping the state dir", "grep -r foo .pi/", true},
		{"catting a file in the state dir", "cat .pi/session.jsonl", true},
		{"the user's own pi home is not this session's", "rm -rf ~/.pi", true},
		{"git clean dry run", "git clean -n", true},
		{"git clean without force does nothing", "git clean -d", true},
		{"forced git clean of an unrelated path", "git clean -fd build/", true},
		{"git status", "git status --short", true},
		{"find -delete elsewhere", "find build -name '*.o' -delete", true},
		{"find without a delete action", "find .pi -name '*.jsonl'", true},
		{"mv INTO an unrelated destination", "mv build/out /tmp/x", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := engine.Evaluate(ToolCall{Kind: ToolBash, Command: tc.command, Cwd: cwd})
			if got.Allow != tc.wantAllow {
				t.Fatalf("Evaluate(%q).Allow = %v, want %v (reason %q)", tc.command, got.Allow, tc.wantAllow, got.Reason)
			}
			if tc.wantAllow {
				return
			}
			// The refusal the model reads must name the directory and say the
			// harness needs it — a bare "blocked" teaches it nothing.
			if !strings.Contains(got.Reason, piStateDir) {
				t.Errorf("refusal reason does not name %s: %q", piStateDir, got.Reason)
			}
			if !strings.Contains(got.Reason, "harness") {
				t.Errorf("refusal reason does not explain the directory is harness state: %q", got.Reason)
			}
		})
	}
}

// TestPolicy_StateDirGuardWithoutCwd proves the guard still fires when the
// engine has no worktree root to resolve against: the relative form is
// compared lexically rather than silently allowed.
func TestPolicy_StateDirGuardWithoutCwd(t *testing.T) {
	t.Parallel()
	engine := NewPolicyEngine(agent.Spec{})
	if d := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "rm -rf .pi"}); d.Allow {
		t.Fatalf("rm -rf .pi allowed with no cwd configured: %+v", d)
	}
	if d := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "rm -rf pipeline"}); !d.Allow {
		t.Fatalf("rm -rf pipeline denied with no cwd configured: %+v", d)
	}
}

// TestPolicy_StateDirGuardUsesEngineCwd proves a tool call cannot relocate the
// boundary it is judged against by declaring a different Cwd.
func TestPolicy_StateDirGuardUsesEngineCwd(t *testing.T) {
	t.Parallel()
	engine := NewPolicyEngine(agent.Spec{Cwd: "/work/repo"})
	d := engine.Evaluate(ToolCall{
		Kind:    ToolBash,
		Command: "rm -rf /work/repo/.pi",
		Cwd:     "/somewhere/else",
	})
	if d.Allow {
		t.Fatalf("state-dir deletion allowed when the call declared a different cwd: %+v", d)
	}
}

// TestShellTokens covers the small quoting approximation the guard relies on.
func TestShellTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "rm -rf .pi", []string{"rm", "-rf", ".pi"}},
		{"double quotes", `grep ".pi" file`, []string{"grep", ".pi", "file"}},
		{"single quotes", `grep '.pi x' file`, []string{"grep", ".pi x", "file"}},
		{"escaped space", `rm my\ dir`, []string{"rm", "my dir"}},
		{"empty quoted operand is kept", `rm ""`, []string{"rm", ""}},
		{"collapsing whitespace", "  rm   -rf   .pi  ", []string{"rm", "-rf", ".pi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shellTokens(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("shellTokens(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("shellTokens(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}
