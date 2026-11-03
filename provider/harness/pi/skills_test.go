package pi

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

func piTestSkill(id string) stagewire.Skill {
	return agent.SealSkillPackage(stagewire.Skill{
		ID: id, Name: id, Version: "1.0.0", Body: id + " fallback",
		Files: []stagewire.SkillFile{
			agent.NewSkillFile("SKILL.md", []byte("---\nname: "+id+"\ndescription: Test skill.\n---\nBody.\n"), 0o644),
		},
	})
}

func piSkillSpec(t *testing.T, cwd string, mode agent.PromptSessionMode, policy *stagewire.RepoSkills, skills ...stagewire.Skill) agent.Spec {
	t.Helper()
	manifest := (&Provider{}).Manifest()
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: mode, Skills: skills, RepoSkills: policy,
	})
	if err != nil {
		t.Fatalf("PlanSkillDelivery: %v", err)
	}
	return agent.Spec{Cwd: cwd, Skills: delivery}
}

// TestSkillPlanGolden_Pi pins this harness's plan: native skills in the
// state directory, and every repository-skill policy achieved headless.
func TestSkillPlanGolden_Pi(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mode   agent.PromptSessionMode
		policy *stagewire.RepoSkills
		want   string
	}{
		{
			"headless allowlist", agent.PromptModeAutonomous, &stagewire.RepoSkills{Policy: stagewire.RepoSkillsAllowlist, Allow: []string{"conventions"}},
			"harness=pi layout=state_dir scripts=allowed trust=false repo=allowlist->allowlist allow=conventions\n" +
				"review native dir=review files=SKILL.md:0644",
		},
		{
			"headless default", agent.PromptModeAutonomous, nil,
			"harness=pi layout=state_dir scripts=allowed trust=false repo=all->all\n" +
				"review native dir=review files=SKILL.md:0644",
		},
		{
			"interactive", agent.PromptModeHumanControlled, &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone},
			"harness=pi layout= scripts=allowed trust=false repo=none->all\n" +
				"review prompt reason=session_mode_prompt_only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := piSkillSpec(t, "/work/repo", tc.mode, tc.policy, piTestSkill("review"))
			if got := spec.Skills.Plan.Summary(); got != tc.want {
				t.Fatalf("plan =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestRPCArgs_SkillGolden pins the skill flags in the headless argv on a
// fresh launch and on a resume.
func TestRPCArgs_SkillGolden(t *testing.T) {
	t.Parallel()
	layout := sessionLayout{root: "/work/repo/.pi-repo", extension: "/work/repo/.pi-repo/donmai-policy.ts"}
	skillArgs := []string{"--no-skills", "--skill", "/work/repo/.pi-repo/skills-injected/review"}
	cases := []struct {
		name      string
		mode      launchMode
		sessionID string
		want      []string
	}{
		{"prompt", launchPrompt, "", []string{
			"--mode", "rpc", "-e", layout.extension, "--no-extensions", "--approve", "--session-dir", layout.root,
			"--no-skills", "--skill", "/work/repo/.pi-repo/skills-injected/review",
		}},
		{"resume", launchResume, "sess-1", []string{
			"--mode", "rpc", "-e", layout.extension, "--no-extensions", "--approve", "--session-dir", layout.root,
			"--no-skills", "--skill", "/work/repo/.pi-repo/skills-injected/review", "--session", "sess-1",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rpcArgsWithSkills(layout, []string{layout.extension}, tc.mode, tc.sessionID, agent.Spec{}, skillArgs)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("argv =\n%q\nwant\n%q", got, tc.want)
			}
			if plain := rpcArgs(layout, []string{layout.extension}, tc.mode, tc.sessionID, agent.Spec{}); slices.Contains(plain, "--skill") {
				t.Fatalf("rpcArgs without skills emitted --skill: %q", plain)
			}
		})
	}
}

// TestSkillMaterializer_StateDirAndPolicy writes native skills under the
// state root and renders each repository policy: all passes no flag; none
// disables discovery; allowlist disables discovery and loads the allowed
// repository skills by path, skipping a symlinked one and any name an
// attached skill already holds.
func TestSkillMaterializer_StateDirAndPolicy(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	repoSkill := func(rel ...string) {
		dir := filepath.Join(append([]string{cwd}, rel...)...)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("repo"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repoSkill(".pi", "skills", "conventions")
	repoSkill(".agents", "skills", "release")
	repoSkill(".agents", "skills", "review") // clashes with the attached skill
	if err := os.Symlink(t.TempDir(), filepath.Join(cwd, ".agents", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	injected := filepath.Join(sessionStateRoot(agent.Spec{Cwd: cwd}), injectedSkillsDir, "review")
	cases := []struct {
		name   string
		policy *stagewire.RepoSkills
		want   []string
	}{
		{"all", nil, []string{"--skill", injected}},
		{"none", &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone}, []string{"--no-skills", "--skill", injected}},
		{
			"allowlist", &stagewire.RepoSkills{Policy: stagewire.RepoSkillsAllowlist, Allow: []string{"release", "conventions", "review", "linked", "absent"}},
			[]string{
				"--no-skills", "--skill", injected,
				"--skill", filepath.Join(cwd, ".pi", "skills", "conventions"),
				"--skill", filepath.Join(cwd, ".agents", "skills", "release"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := piSkillSpec(t, cwd, agent.PromptModeAutonomous, tc.policy, piTestSkill("review"))
			got, err := skillMaterializer{}.MaterializeSkills(spec)
			if err != nil {
				t.Fatalf("MaterializeSkills: %v", err)
			}
			if !slices.Equal(got.Args, tc.want) {
				t.Fatalf("args =\n%q\nwant\n%q", got.Args, tc.want)
			}
			body, err := os.ReadFile(filepath.Join(injected, "SKILL.md")) //nolint:gosec // G304: test-local.
			if err != nil || !strings.Contains(string(body), "name: review") {
				t.Fatalf("injected SKILL.md = %q, %v", body, err)
			}
		})
	}
}

// TestSkillMaterializer_ResumeRewritesTamperedState proves a relaunch does
// not trust what the previous run left in its agent-writable state: the
// directory is rewritten from the plan.
func TestSkillMaterializer_ResumeRewritesTamperedState(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	spec := piSkillSpec(t, cwd, agent.PromptModeAutonomous, nil, piTestSkill("review"))
	if _, err := (skillMaterializer{}).MaterializeSkills(spec); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sessionStateRoot(spec), injectedSkillsDir, "review", "SKILL.md")
	if err := os.WriteFile(target, []byte("planted"), 0o644); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatal(err)
	}
	if _, err := (skillMaterializer{}).MaterializeSkills(spec); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if body, _ := os.ReadFile(target); !strings.Contains(string(body), "name: review") { //nolint:gosec // G304: test-local.
		t.Fatalf("relaunch kept the planted content: %q", body)
	}
}

// TestSpawn_ChildArgvCarriesSkillFlags drives the real Provider.Spawn
// against a fake pi that records its argv: the launch must materialize the
// native skill and hand the child --no-skills (policy none) and --skill
// <state dir>. The spawn itself fails (the fake never answers the policy
// handshake); the recorded argv is what matters.
func TestSpawn_ChildArgvCarriesSkillFlags(t *testing.T) {
	// Not parallel: spawns a real child process with a fixed recording dir.
	dir := t.TempDir()
	fake := fakeArgvRecorderPi(t, dir)
	p, err := New(Options{
		PiBin:            fake,
		HandshakeTimeout: 30 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	cwd := t.TempDir()
	spec := piSkillSpec(t, cwd, agent.PromptModeAutonomous, &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone}, piTestSkill("review"))
	spec.Prompt = "use the skill"
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	spawnErr := make(chan error, 1)
	go func() {
		_, err := p.Spawn(ctx, spec)
		spawnErr <- err
	}()
	argv := waitForArgvRecord(t, filepath.Join(dir, "argv"), 20*time.Second)
	cancel()
	select {
	case <-spawnErr:
	case <-time.After(20 * time.Second):
		t.Fatal("Spawn did not return after context cancellation")
	}
	injected := filepath.Join(sessionStateRoot(spec), injectedSkillsDir, "review")
	for _, want := range []string{"--no-skills", "--skill " + injected} {
		if !strings.Contains(strings.ReplaceAll(argv, "\n", " "), want) {
			t.Fatalf("child argv lacks %q:\n%s", want, argv)
		}
	}
}
