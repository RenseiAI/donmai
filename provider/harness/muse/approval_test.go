package muse

import (
	"os"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestApprovalBridge_ClaudePermissionForms pins each Claude-grammar form the
// runner hands this bridge.
func TestApprovalBridge_ClaudePermissionForms(t *testing.T) {
	t.Parallel()
	cmd := func(s string) ApprovalRequest { return ApprovalRequest{Kind: ApprovalKindCommand, Command: s} }
	file := func(p string) ApprovalRequest { return ApprovalRequest{Kind: ApprovalKindFileAccess, Path: p} }
	for _, tc := range []struct {
		name string
		cfg  agent.PermissionConfig
		req  ApprovalRequest
		want ApprovalAction
	}{
		{"prefix:* allow covers its command", agent.PermissionConfig{AllowPatterns: []string{"Bash(git:*)"}}, cmd("git status"), ActionAllow},
		{"prefix:* allow does not cover every command", agent.PermissionConfig{AllowPatterns: []string{"Bash(git:*)"}}, cmd("rm -rf src"), ActionDecline},
		{"prefix:* allow stops at a word boundary", agent.PermissionConfig{AllowPatterns: []string{"Bash(git:*)"}}, cmd("gitx status"), ActionDecline},
		{"multi-word prefix:* allow", agent.PermissionConfig{AllowPatterns: []string{"Bash(npm run:*)"}}, cmd("npm run build"), ActionAllow},
		{"prefix:* deny declines its command", agent.PermissionConfig{DisallowPatterns: []string{"Bash(rm:*)"}}, cmd("rm x"), ActionDecline},
		{"prefix:* deny does not decline every command", agent.PermissionConfig{DisallowPatterns: []string{"Bash(rm:*)"}}, cmd("ls"), ActionAllow},
		{"multi-word prefix:* deny", agent.PermissionConfig{DisallowPatterns: []string{"Bash(git push:*)"}}, cmd("git push origin main"), ActionDecline},
		{"bare Bash deny declines every command", agent.PermissionConfig{DisallowPatterns: []string{"Bash"}}, cmd("ls"), ActionDecline},
		{"bare Bash deny leaves tool approvals alone", agent.PermissionConfig{DisallowPatterns: []string{"Bash"}}, ApprovalRequest{Kind: ApprovalKindTool, Target: "fetch"}, ActionAllow},
		{"bare Bash allow keeps its legacy meaning", agent.PermissionConfig{AllowPatterns: []string{"Bash"}, DefaultDecision: "deny"}, cmd("git status"), ActionDecline},
		{"bare Edit deny declines file access", agent.PermissionConfig{DisallowPatterns: []string{"Edit"}}, file("/w/a.go"), ActionDecline},
		{"bare Write deny declines file access", agent.PermissionConfig{DisallowPatterns: []string{"Write"}}, file("/w/a.go"), ActionDecline},
		{"bare Edit deny leaves commands alone", agent.PermissionConfig{DisallowPatterns: []string{"Edit"}}, cmd("ls"), ActionAllow},
		{"bare Edit allow covers file access", agent.PermissionConfig{AllowPatterns: []string{"Edit"}}, file("/w/a.go"), ActionAllow},
		{"bare Edit allow authorizes no command", agent.PermissionConfig{AllowPatterns: []string{"Edit"}}, cmd("ls"), ActionDecline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			if got := NewApprovalBridge(&cfg).Evaluate(tc.req); got.Action != tc.want {
				t.Errorf("%s %q = %s (%s), want %s", tc.req.Kind, tc.req.Command+tc.req.Path+tc.req.Target, got.Action, got.Reason, tc.want)
			}
		})
	}
}

// TestApprovalBridge_PathDenies pins path-scoped Read/Edit/Write denies on file
// access: globs, ~ expansion, root-anchored relative paths, and fail-closed.
func TestApprovalBridge_PathDenies(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	file := func(p string) ApprovalRequest { return ApprovalRequest{Kind: ApprovalKindFileAccess, Path: p} }
	for _, tc := range []struct {
		name string
		deny []string
		root string
		req  ApprovalRequest
		want ApprovalAction
	}{
		{"home glob read deny", []string{"Read(~/.ssh/**)"}, "/w", file(home + "/.ssh/id_rsa"), ActionDecline},
		{"absolute ** read deny", []string{"Read(/srv/**/.env*)"}, "/w", file("/srv/a/.env.local"), ActionDecline},
		{"relative edit deny", []string{"Edit(docs/**)"}, "/w", file("/w/docs/a.md"), ActionDecline},
		{"relative edit deny, relative path", []string{"Edit(docs/**)"}, "/w", file("docs/a.md"), ActionDecline},
		{"path deny leaves other paths", []string{"Edit(docs/**)", "Read(~/.ssh/**)"}, "/w", file("/w/src/a.go"), ActionAllow},
		{"path deny leaves commands", []string{"Read(~/.ssh/**)"}, "/w", ApprovalRequest{Kind: ApprovalKindCommand, Command: "ls"}, ActionAllow},
		{"unparseable path deny fails closed", []string{"Read(/s/[ab.key)"}, "/w", file("/w/src/a.go"), ActionDecline},
		{"relative deny without a root fails closed", []string{"Edit(docs/**)"}, "", file("/w/src/a.go"), ActionDecline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bridge := newApprovalBridgeAt(&agent.PermissionConfig{DisallowPatterns: tc.deny}, tc.root)
			if got := bridge.Evaluate(tc.req); got.Action != tc.want {
				t.Errorf("%s %q = %s (%s), want %s", tc.req.Kind, tc.req.Command+tc.req.Path, got.Action, got.Reason, tc.want)
			}
		})
	}
}
