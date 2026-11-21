package codex

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func denySpec(level agent.SandboxLevel, denies ...string) agent.Spec {
	return agent.Spec{
		Cwd: "/w", Prompt: "p", SandboxLevel: level, Autonomous: true,
		PermissionConfig: &agent.PermissionConfig{DisallowPatterns: denies},
	}
}

const wrapperPromptRules = `prefix_rule(pattern=["bash"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["command"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["dash"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["env"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["eval"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["exec"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["ksh"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["nice"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["nohup"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["sh"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["time"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["timeout"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["xargs"], decision="prompt", justification="denied by the session tool policy")
prefix_rule(pattern=["zsh"], decision="prompt", justification="denied by the session tool policy")
`

// TestDenyTranslationCodexParams pins the exact Codex request params and rule
// file each deny form produces.
func TestDenyTranslationCodexParams(t *testing.T) {
	t.Parallel()
	noShell := map[string]any{"features": map[string]any{"shell_tool": false}}
	for _, tc := range []struct {
		name       string
		spec       agent.Spec
		config     map[string]any
		rules      string
		statuses   []DenyStatus
		wantNotice bool
	}{
		{name: "bare Bash disables the shell tool", spec: denySpec(agent.SandboxWorkspaceWrite, "Bash"), config: noShell, statuses: []DenyStatus{DenyApplied}},
		{name: "Bash(*) disables the shell tool", spec: denySpec(agent.SandboxWorkspaceWrite, "Bash(*)"), config: noShell, statuses: []DenyStatus{DenyApplied}},
		{
			name: "star and colon-star prefixes are forbidden rules", spec: denySpec(agent.SandboxWorkspaceWrite, "Bash(git push*)", "Bash(gh pr merge:*)", "Bash(pnpm *)"),
			rules: `prefix_rule(pattern=["git", "push"], decision="forbidden", justification="denied by the session tool policy")
prefix_rule(pattern=["gh", "pr", "merge"], decision="forbidden", justification="denied by the session tool policy")
prefix_rule(pattern=["pnpm"], decision="forbidden", justification="denied by the session tool policy")
` + wrapperPromptRules,
			statuses: []DenyStatus{DenyApplied, DenyApplied, DenyApplied},
		},
		{
			name: "an exact command is a prompt rule", spec: denySpec(agent.SandboxWorkspaceWrite, "Bash(env)"),
			rules:    `prefix_rule(pattern=["env"], decision="prompt", justification="denied by the session tool policy")` + "\n" + wrapperPromptRules,
			statuses: []DenyStatus{DenyApplied},
		},
		{name: "a mid-pattern wildcard disables the shell tool", spec: denySpec(agent.SandboxWorkspaceWrite, "Bash(git * --force)"), config: noShell, statuses: []DenyStatus{DenyApproximated}, wantNotice: true},
		{name: "path denies are reported until rendered", spec: denySpec(agent.SandboxWorkspaceWrite, "Read(~/.ssh/**)"), statuses: []DenyStatus{DenyUnsupported}, wantNotice: true},
		{name: "tool names Codex lacks are reported", spec: denySpec(agent.SandboxWorkspaceWrite, "AskUserQuestion", "Edit"), statuses: []DenyStatus{DenyUnsupported, DenyApproximated}, wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := NewSpawnPlan(tc.spec)
			if plan.DenyErr != nil {
				t.Fatalf("DenyErr = %v", plan.DenyErr)
			}
			if got, _ := plan.ThreadStart["config"].(map[string]any); !reflect.DeepEqual(got, tc.config) {
				t.Errorf("thread/start config = %#v, want %#v", got, tc.config)
			}
			if got, _ := plan.ThreadResume["config"].(map[string]any); !reflect.DeepEqual(got, tc.config) {
				t.Errorf("thread/resume config = %#v, want %#v", got, tc.config)
			}
			if plan.ExecPolicyRules != tc.rules {
				t.Errorf("rules =\n%s\nwant\n%s", plan.ExecPolicyRules, tc.rules)
			}
			var statuses []DenyStatus
			for _, line := range plan.DenyReport {
				statuses = append(statuses, line.Status)
			}
			if !reflect.DeepEqual(statuses, tc.statuses) {
				t.Errorf("report statuses = %v, want %v", statuses, tc.statuses)
			}
			if got := plan.denyNotice != nil; got != tc.wantNotice {
				t.Errorf("warning notice = %v, want %v", got, tc.wantNotice)
			}
		})
	}
}

// TestSpawnStagesAndPinsExecPolicyRules: the rule file exists before start and
// a later session asking for different rules is refused.
func TestSpawnStagesAndPinsExecPolicyRules(t *testing.T) {
	p, _ := codexEnvProbe(t)
	spec := denySpec(agent.SandboxWorkspaceWrite, "Bash(git push*)")
	spec.Cwd = t.TempDir()
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	body, err := os.ReadFile(filepath.Join(p.config.home, "rules", codexDenyRulesFile)) //nolint:gosec // G304: test-owned home
	if err != nil || !strings.HasPrefix(string(body), `prefix_rule(pattern=["git", "push"], decision="forbidden"`) {
		t.Fatalf("staged rules = %q, %v", body, err)
	}
	other := denySpec(agent.SandboxWorkspaceWrite, "Bash(gh api*)")
	other.Cwd = spec.Cwd
	if _, err := p.Spawn(t.Context(), other); !errors.Is(err, ErrDenyUntranslatable) {
		t.Fatalf("divergent rules: err = %v, want ErrDenyUntranslatable", err)
	}
}
