package muse

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/stagewire"
)

func museTestSkill(id string) stagewire.Skill {
	return agent.SealSkillPackage(stagewire.Skill{
		ID: id, Name: id, Version: "1.0.0", Body: id + " fallback",
		Files: []stagewire.SkillFile{
			agent.NewSkillFile("SKILL.md", []byte("---\nname: "+id+"\ndescription: Test skill.\n---\nBody.\n"), 0o644),
		},
	})
}

func museSkillSpec(t *testing.T, cwd string, provider *Provider, policy *stagewire.RepoSkills, skills ...stagewire.Skill) agent.Spec {
	t.Helper()
	manifest := provider.Manifest()
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: agent.PromptModeAutonomous, Skills: skills, RepoSkills: policy,
	})
	if err != nil {
		t.Fatalf("PlanSkillDelivery: %v", err)
	}
	return agent.Spec{Cwd: cwd, Prompt: "go", Skills: delivery}
}

// TestSkillPlanGolden_Muse pins this harness's plan: a native skill goes to
// the reserved workspace directory and trusts the workspace, which is why
// the repository policy is achieved as all; with no native skill the
// workspace stays untrusted and repository skills do not load (none).
func TestSkillPlanGolden_Muse(t *testing.T) {
	t.Parallel()
	none := &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone}
	cases := []struct {
		name     string
		provider *Provider
		skills   []stagewire.Skill
		policy   *stagewire.RepoSkills
		want     string
	}{
		{
			"native skill trusts", &Provider{},
			[]stagewire.Skill{museTestSkill("review")},
			none,
			"harness=muse layout=workspace scripts=allowed trust=true repo=none->all\n" +
				"review native dir=donmai-review files=SKILL.md:0644",
		},
		{
			"body-only skill stays untrusted", &Provider{},
			[]stagewire.Skill{{ID: "legacy", Body: "b"}},
			&stagewire.RepoSkills{},
			"harness=muse layout=workspace scripts=allowed trust=false repo=all->none\n" +
				"legacy prompt reason=no_files",
		},
		{
			"trusted host loads repository skills", &Provider{opts: Options{TrustWorkspace: true}},
			[]stagewire.Skill{{ID: "legacy", Body: "b"}},
			none,
			"harness=muse layout=workspace scripts=allowed trust=false repo=none->all\n" +
				"legacy prompt reason=no_files",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := museSkillSpec(t, "/work/repo", tc.provider, tc.policy, tc.skills...)
			if got := spec.Skills.Plan.Summary(); got != tc.want {
				t.Fatalf("plan =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestSkillMaterializer_WorkspaceLayout writes the reserved directory with
// its marker in a real checkout, keeps it out of git status, never
// overwrites the repository's own directory, and on cleanup removes what
// it wrote while leaving the repository's skills in place.
func TestSkillMaterializer_WorkspaceLayout(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cwd := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}} {
		if out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // G204: test fixture.
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repoSkill := filepath.Join(cwd, ".agents", "skills", "conventions")
	if err := os.MkdirAll(repoSkill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoSkill, "SKILL.md"), []byte("repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := museSkillSpec(t, cwd, &Provider{}, nil, museTestSkill("review"))
	got, err := skillMaterializer{}.MaterializeSkills(spec)
	if err != nil {
		t.Fatalf("MaterializeSkills: %v", err)
	}
	if !got.TrustWorkspace || got.Args != nil || got.Cleanup == nil {
		t.Fatalf("result = %+v, want trust and a cleanup, no argv", got)
	}
	dir := filepath.Join(cwd, ".agents", "skills", "donmai-review")
	for _, name := range []string{"SKILL.md", agent.SkillMarkerFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	status, err := exec.Command("git", "-C", cwd, "status", "--porcelain", "--untracked-files=all").CombinedOutput() //nolint:gosec // G204: test fixture.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(status), "donmai-review") || !strings.Contains(string(status), "conventions") {
		t.Fatalf("git status = %s; want the materialized dir excluded and the repository skill visible", status)
	}
	// A second materialization replaces its own marked directory.
	again, err := skillMaterializer{}.MaterializeSkills(spec)
	if err != nil {
		t.Fatalf("re-materialize over the marked dir: %v", err)
	}
	if err := again.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left %s: %v", dir, err)
	}
	if _, err := os.Lstat(filepath.Join(repoSkill, "SKILL.md")); err != nil {
		t.Fatalf("cleanup removed the repository skill: %v", err)
	}

	// The repository's own donmai-<id> directory (no marker) is never
	// overwritten: the materialization refuses.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("repository content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (skillMaterializer{}).MaterializeSkills(spec); err == nil {
		t.Fatal("materialization overwrote an unmarked repository directory")
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(body) != "repository content" { //nolint:gosec // G304: test-local.
		t.Fatalf("repository directory changed: %q", body)
	}
}

// TestSkillMaterializer_CleanupRemovesCreatedParents proves a workspace
// with no .agents directory is left as it was found.
func TestSkillMaterializer_CleanupRemovesCreatedParents(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	got, err := skillMaterializer{}.MaterializeSkills(museSkillSpec(t, cwd, &Provider{}, nil, museTestSkill("review")))
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cwd, ".agents")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".agents left behind: %v", err)
	}
}

// TestSpawn_TrustsWorkspaceOnlyForNativeSkills drives the real serve spawn
// against a fake host that records its argv and whether the skill
// directory exists, then exits (the spawn fails at the handshake). A native
// skill adds --trust-workspace with the directory in place, and the failed
// spawn removes it; a session without one is never trusted.
func TestSpawn_TrustsWorkspaceOnlyForNativeSkills(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		skills    []stagewire.Skill
		wantTrust bool
	}{
		{"native skill", []stagewire.Skill{museTestSkill("review")}, true},
		{"body-only skill", []stagewire.Skill{{ID: "legacy", Body: "b"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			record := filepath.Join(dir, "argv")
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			skillDir := filepath.Join(cwd, ".agents", "skills", "donmai-review")
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = \"--version\" ]; then printf 'Muse Code 1.4.9 (1.4.9-R6297.1)\\n'; exit 0; fi\n" +
				"echo \"$@\" > " + shellQuote(record) + "\n" +
				"if [ -f " + shellQuote(filepath.Join(skillDir, "SKILL.md")) + " ]; then echo skill-present >> " + shellQuote(record) + "; fi\n" +
				"exit 0\n"
			bin := filepath.Join(dir, "fake-muse.sh")
			if err := testexec.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
				t.Fatal(err)
			}
			p, err := New(Options{MuseBin: bin})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			spec := museSkillSpec(t, cwd, p, nil, tc.skills...)
			if _, err := p.Spawn(t.Context(), spec); err == nil {
				t.Fatal("Spawn against a fake that exits succeeded")
			}
			raw, err := os.ReadFile(record) //nolint:gosec // G304: test-local.
			if err != nil {
				t.Fatalf("fake host never ran: %v", err)
			}
			got := string(raw)
			if strings.Contains(got, "--trust-workspace") != tc.wantTrust {
				t.Fatalf("serve argv %q: --trust-workspace present = %v, want %v", got, !tc.wantTrust, tc.wantTrust)
			}
			if strings.Contains(got, "skill-present") != tc.wantTrust {
				t.Fatalf("skill directory present at spawn = %v, want %v", !tc.wantTrust, tc.wantTrust)
			}
			if _, err := os.Lstat(skillDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed spawn left %s: %v", skillDir, err)
			}
		})
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
