package muse

import (
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
