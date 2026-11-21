//go:build codex_integration

package codex

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestIntegration_RealCodexAcceptsDenyTranslation checks the produced params
// against the installed app-server, which validates the whole thread config
// (features.shell_tool and the permission profile),
// and the staged rule file against Codex's own exec-policy evaluator. Neither
// step reaches a model.
func TestIntegration_RealCodexAcceptsDenyTranslation(t *testing.T) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("requires codex on PATH: %v", err)
	}
	cwd := t.TempDir()
	spec := denySpec(agent.SandboxWorkspaceWrite, "Bash(git push*)", "Bash(env)", "Bash(git * --force)", "Read(~/.ssh/**)", "Edit(docs/**)")
	spec.Cwd = cwd
	plan := NewSpawnPlan(spec)
	if plan.DenyErr != nil {
		t.Fatal(plan.DenyErr)
	}
	p, err := New(Options{CodexBin: binary, Cwd: cwd, HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	p.startMu.Lock()
	err = p.stageExecPolicyRulesLocked(plan.ExecPolicyRules)
	p.startMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ensureStarted(); err != nil {
		t.Fatal(err)
	}
	raw, err := p.client.Request(t.Context(), "thread/start", plan.ThreadStart, 30*time.Second)
	if err != nil {
		t.Fatalf("thread/start refused the deny translation: %v", err)
	}
	var started struct {
		Profile struct {
			ID      string `json:"id"`
			Extends string `json:"extends"`
		} `json:"activePermissionProfile"`
		Sandbox map[string]any `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	if started.Profile.ID != codexDenyProfileID || started.Profile.Extends != ":workspace" || started.Sandbox["networkAccess"] != true {
		t.Fatalf("app-server echoed profile %+v sandbox %v", started.Profile, started.Sandbox)
	}

	rules := filepath.Join(p.config.home, "rules", codexDenyRulesFile)
	for argv, want := range map[string]string{"git push origin main": "forbidden", "env": "prompt", "git status": ""} {
		out, err := exec.Command(binary, append([]string{"execpolicy", "check", "--rules", rules, "--"}, strings.Fields(argv)...)...).Output() //nolint:gosec // G204: test-resolved codex binary
		if err != nil {
			t.Fatalf("execpolicy check %q: %v", argv, err)
		}
		var verdict struct {
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(out, &verdict); err != nil || verdict.Decision != want {
			t.Errorf("execpolicy %q = %s (%v), want %q", argv, out, err, want)
		}
	}
}
