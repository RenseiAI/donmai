package pi

import (
	"os"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestPolicyEngine_ClaudePermissionForms pins each deny and allow form the
// runner's flat tool lists carry, one block per class of gap the shared
// toolpolicy matcher closes.
func TestPolicyEngine_ClaudePermissionForms(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	bash := func(s string) ToolCall { return ToolCall{Kind: ToolBash, Command: s} }
	file := func(k ToolKind, p string) ToolCall { return ToolCall{Kind: k, Path: p} }
	for _, tc := range []struct {
		name        string
		deny, allow []string
		call        ToolCall
		wantAllowed bool
	}{
		// Chained and wrapped commands.
		{"deny prefix direct", []string{"Bash(git push*)"}, nil, bash("git push origin"), false},
		{"deny prefix chained", []string{"Bash(git push*)"}, nil, bash("cd x && git push"), false},
		{"deny prefix through env", []string{"Bash(git push*)"}, nil, bash("env FOO=1 git push"), false},
		{"deny prefix through bash -c", []string{"Bash(git push*)"}, nil, bash("bash -c 'git push'"), false},
		{"deny prefix leaves others", []string{"Bash(git push*)"}, nil, bash("git status"), true},
		// Exact versus prefix.
		{"exact deny is exact", []string{"Bash(env)"}, nil, bash("envsubst < a"), true},
		{"exact deny matches", []string{"Bash(env)"}, nil, bash("env"), false},
		{"word prefix deny covers the bare word", []string{"Bash(pnpm *)"}, nil, bash("pnpm"), false},
		// Path globs over every read and write kind.
		{"home glob read deny", []string{"Read(~/.ssh/**)"}, nil, file(ToolRead, home+"/.ssh/id_rsa"), false},
		{"absolute ** read deny", []string{"Read(/srv/**/.env*)"}, nil, file(ToolRead, "/srv/a/.env.local"), false},
		{"relative read deny", []string{"Read(secret/**)"}, nil, file(ToolRead, "/w/secret/k"), false},
		{"read deny covers grep", []string{"Read(/w/secret/**)"}, nil, file(ToolGrep, "/w/secret/k"), false},
		{"read deny covers ls of the tree", []string{"Read(/w/secret/**)"}, nil, file(ToolLs, "/w/secret"), false},
		{"read deny covers walking an ancestor", []string{"Read(/w/secret/**)"}, nil, ToolCall{Kind: ToolFind}, false},
		{"read deny leaves other reads", []string{"Read(/w/secret/**)"}, nil, file(ToolRead, "/w/src/a.go"), true},
		{"edit deny covers write", []string{"Edit(docs/**)"}, nil, file(ToolWrite, "/w/docs/a.md"), false},
		{"edit deny covers edit", []string{"Edit(docs/**)"}, nil, file(ToolEdit, "/w/docs/a.md"), false},
		{"Glob deny gates find", []string{"Glob"}, nil, file(ToolFind, "/w"), false},
		{"bare Grep deny leaves Read", []string{"Grep"}, nil, file(ToolRead, "/w/a.go"), true},
		{"unparseable path deny fails closed", []string{"Read(~other/.ssh)"}, nil, file(ToolRead, "/w/a.go"), false},
		// Allows cover the whole line and every chained command.
		{"prefix allow covers its command", nil, []string{"Bash(git:*)"}, bash("git status"), true},
		{"prefix allow not a chained command", nil, []string{"Bash(git:*)"}, bash("git status && rm -rf src"), false},
		{"prefix allow stops at a word", nil, []string{"Bash(git:*)"}, bash("gitx"), false},
		{"allow keeps quoted separators", nil, []string{"Bash(git *)"}, bash(`git commit -m "a; b"`), true},
		{"path allow covers its kind", nil, []string{"Read(/w/docs/**)"}, file(ToolRead, "/w/docs/a.md"), true},
		{"path allow not another path", nil, []string{"Read(/w/docs/**)"}, file(ToolRead, "/w/src/a.go"), false},
		{"path allow not another kind", nil, []string{"Read(/w/docs/**)"}, file(ToolGrep, "/w/docs/a.md"), false},
		{"bare Read allow keeps its meaning", nil, []string{"Read"}, file(ToolRead, "/w/src/a.go"), true},
		// Tool names match case-insensitively, as the bare-name patterns do.
		{"lower-case prefix deny", []string{"bash(git push*)"}, nil, bash("git push origin"), false},
		{"lower-case word deny chained", []string{"bash(rm:*)"}, nil, bash("ls; rm -rf src"), false},
		{"upper-case exact deny", []string{"BASH(env)"}, nil, bash("env"), false},
		{"lower-case deny leaves others", []string{"bash(git push*)"}, nil, bash("git status"), true},
		{"lower-case prefix allow", nil, []string{"bash(git:*)"}, bash("git status"), true},
		{"lower-case allow not a chained command", nil, []string{"bash(git:*)"}, bash("git status && rm -rf src"), false},
		{"lower-case path deny", []string{"read(/w/secret/**)"}, nil, file(ToolGrep, "/w/secret/k"), false},
		{"lower-case path allow authorizes no command", nil, []string{"read(/w/docs)"}, bash("cat read/w/docs"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			engine := NewPolicyEngine(agent.Spec{Cwd: "/w", DisallowedTools: tc.deny, AllowedTools: tc.allow})
			if got := engine.Evaluate(tc.call); got.Allow != tc.wantAllowed {
				t.Errorf("%+v: allowed = %v (%s), want %v", tc.call, got.Allow, got.Reason, tc.wantAllowed)
			}
		})
	}
}

// TestPolicyEngine_RawRegexAllowNeedsEveryCommand: a raw PermissionConfig
// allow regex does not approve a bash line on its own; the whole line and
// every chained command must be covered.
func TestPolicyEngine_RawRegexAllowNeedsEveryCommand(t *testing.T) {
	t.Parallel()
	engine := NewPolicyEngine(agent.Spec{Cwd: "/w", PermissionConfig: &agent.PermissionConfig{AllowPatterns: []string{"^git"}}})
	if got := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "git status && rm -rf x"}); got.Allow {
		t.Errorf("chained command allowed by a raw regex allow, want deny")
	}
	if got := engine.Evaluate(ToolCall{Kind: ToolBash, Command: "git status"}); !got.Allow {
		t.Errorf("covered command denied (%s), want allow", got.Reason)
	}
}

// TestParseAdjudicateCall_ExpandsHome: a "~/" path is judged as the home path,
// never as a "~" directory under cwd.
func TestParseAdjudicateCall_ExpandsHome(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	call := parseAdjudicateCall(map[string]any{"toolName": "read", "input": map[string]any{"path": "~/.ssh/id_rsa"}}, "/w")
	if call.Path != home+"/.ssh/id_rsa" {
		t.Fatalf("path = %q, want the home path", call.Path)
	}
	if NewPolicyEngine(agent.Spec{Cwd: "/w", DisallowedTools: []string{"Read(~/.ssh/**)"}}).Evaluate(call).Allow {
		t.Fatal("home read through ~/ was allowed under a ~/.ssh deny")
	}
}
