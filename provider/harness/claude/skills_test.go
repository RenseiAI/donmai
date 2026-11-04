package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

// claudeTestSkills is one native-capable package, one legacy body-only
// skill and one prompt_only skill.
func claudeTestSkills() []stagewire.Skill {
	review := agent.SealSkillPackage(stagewire.Skill{
		ID: "review", Name: "review", Version: "1.0.0", Body: "review fallback",
		Files: []stagewire.SkillFile{
			agent.NewSkillFile("SKILL.md", []byte("---\nname: review\ndescription: Review a change.\n---\nCheck the diff.\n"), 0o644),
			agent.NewSkillFile("scripts/lint.sh", []byte("#!/bin/sh\n"), 0o755),
		},
	})
	return []stagewire.Skill{
		review,
		{ID: "legacy", Body: "legacy body"},
		{ID: "notes", Body: "notes body", Delivery: stagewire.SkillDeliveryPromptOnly},
	}
}

func claudeSkillDelivery(t *testing.T, mode agent.PromptSessionMode, policy *stagewire.RepoSkills) (*agent.SkillDelivery, []stagewire.Skill) {
	t.Helper()
	manifest := (&Provider{}).Manifest()
	delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: mode,
		Skills: claudeTestSkills(), RepoSkills: policy,
	})
	if err != nil {
		t.Fatalf("PlanSkillDelivery: %v", err)
	}
	return delivery, residue
}

// TestSkillPlanGolden_Claude pins this harness's plan: the package goes to
// the plugin directory, body-only and prompt_only skills ride the prompt,
// and the repository policy is recorded as all whatever was requested.
func TestSkillPlanGolden_Claude(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mode agent.PromptSessionMode
		want string
	}{
		{"headless", agent.PromptModeAutonomous, "harness=claude-code layout=plugin_dir scripts=allowed trust=false repo=none->all\n" +
			"review native dir=review files=SKILL.md:0644,scripts/lint.sh:0755\n" +
			"legacy prompt reason=no_files\n" +
			"notes prompt reason=prompt_only"},
		{"interactive", agent.PromptModeHumanControlled, "harness=claude-code layout= scripts=allowed trust=false repo=none->all\n" +
			"review prompt reason=session_mode_prompt_only\n" +
			"legacy prompt reason=session_mode_prompt_only\n" +
			"notes prompt reason=prompt_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			delivery, _ := claudeSkillDelivery(t, tc.mode, &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone})
			if got := delivery.Plan.Summary(); got != tc.want {
				t.Fatalf("plan =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestSkillMaterializer_PluginLayout writes the plugin directory outside
// any worktree and checks its manifest, skill files and argv.
func TestSkillMaterializer_PluginLayout(t *testing.T) {
	t.Parallel()
	delivery, _ := claudeSkillDelivery(t, agent.PromptModeAutonomous, nil)
	tmp := t.TempDir()
	got, err := skillMaterializer{tempDir: tmp}.MaterializeSkills(agent.Spec{Skills: delivery})
	if err != nil {
		t.Fatalf("MaterializeSkills: %v", err)
	}
	if len(got.Args) != 2 || got.Args[0] != "--plugin-dir" || !strings.HasPrefix(filepath.Base(got.Args[1]), skillPluginDirPrefix) || !filepath.IsAbs(got.Args[1]) {
		t.Fatalf("Args = %q, want --plugin-dir <abs %s*>", got.Args, skillPluginDirPrefix)
	}
	dir := got.Args[1]
	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")) //nolint:gosec // G304: test-local.
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest["name"] != skillPluginName {
		t.Fatalf("plugin.json = %s (%v), want name %q", raw, err, skillPluginName)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "skills", "review", "SKILL.md")) //nolint:gosec // G304: test-local.
	if err != nil || !strings.Contains(string(skill), "Check the diff.") {
		t.Fatalf("SKILL.md = %q, %v", skill, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "skills", "review", "scripts", "lint.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("script mode: %v %v", info, err)
	}
	if err := got.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left %s: %v", dir, err)
	}

	// No native entry, no directory and no argv.
	none, err := skillMaterializer{tempDir: tmp}.MaterializeSkills(agent.Spec{})
	if err != nil || none.Args != nil || none.Cleanup != nil {
		t.Fatalf("empty spec: %+v %v", none, err)
	}
}

// TestBuildArgs_PluginDirGolden pins where --plugin-dir sits in the argv of
// a fresh spawn and of a resume.
func TestBuildArgs_PluginDirGolden(t *testing.T) {
	t.Parallel()
	skillArgs := []string{"--plugin-dir", "/tmp/donmai-skills-1"}
	spec := agent.Spec{Prompt: "go", Cwd: "/work/repo", SystemPromptAppend: "sys"}
	cases := []struct {
		name            string
		resume, ownedID string
		want            []string
	}{
		{"fresh", "", "owned-1", []string{
			"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
			"--add-dir", "/work/repo", "--plugin-dir", "/tmp/donmai-skills-1",
			"--session-id", "owned-1", "--append-system-prompt", "sys",
		}},
		{"resume", "sess-9", "", []string{
			"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
			"--add-dir", "/work/repo", "--plugin-dir", "/tmp/donmai-skills-1",
			"--resume", "sess-9", "--append-system-prompt", "sys",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, stdin := buildArgsWithSkills(spec, "", tc.resume, tc.ownedID, skillArgs)
			if !slices.Equal(got, tc.want) || stdin != "go" {
				t.Fatalf("argv =\n%q\nwant\n%q", got, tc.want)
			}
			plain, _ := buildArgs(spec, "", tc.resume, tc.ownedID)
			if slices.Contains(plain, "--plugin-dir") {
				t.Fatalf("buildArgs without skills emitted --plugin-dir: %q", plain)
			}
		})
	}
}

// TestSpawn_PluginDirRidesSpawnResumeAndInject drives the real Spawn,
// Inject and Resume paths against a fake CLI that records each
// invocation's argv and whether the plugin directory's SKILL.md exists at
// that moment. Every invocation must carry --plugin-dir with the skill in
// place, and Stop must remove the directory.
func TestSpawn_PluginDirRidesSpawnResumeAndInject(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	log := filepath.Join(workdir, "argv.log")
	script := "#!/bin/sh\n" +
		"set -eu\n" +
		"prev=''\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--plugin-dir\" ]; then\n" +
		"    if [ -f \"$a/skills/review/SKILL.md\" ]; then echo \"plugin-dir present\" >> " + shQuote(log) + "; else echo \"plugin-dir missing\" >> " + shQuote(log) + "; fi\n" +
		"  fi\n" +
		"  if [ \"$a\" = \"--resume\" ]; then echo resume >> " + shQuote(log) + "; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"echo --- >> " + shQuote(log) + "\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-skills"}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	cli := writeFakeCLI(t, "fake-claude-skills.sh", script)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.skillTempDir = t.TempDir()
	delivery, _ := claudeSkillDelivery(t, agent.PromptModeAutonomous, nil)
	spec := agent.Spec{Prompt: "use the skill", Cwd: workdir, Skills: delivery}

	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainUntilResult(t, h.Events(), 30*time.Second)
	if err := h.Inject(t.Context(), "follow up"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	_ = drainUntilResult(t, h.Events(), 30*time.Second)
	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if entries, _ := os.ReadDir(p.skillTempDir); len(entries) != 0 {
		t.Fatalf("plugin directory not removed at Stop: %v", entries)
	}

	r, err := p.Resume(t.Context(), "sess-skills", spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainUntilResult(t, r.Events(), 30*time.Second)
	_ = r.Stop(t.Context())

	raw, err := os.ReadFile(log) //nolint:gosec // G304: test-local.
	if err != nil {
		t.Fatal(err)
	}
	runs := strings.Split(strings.TrimSuffix(string(raw), "---\n"), "---\n")
	want := []string{"plugin-dir present\n", "plugin-dir present\nresume\n", "plugin-dir present\nresume\n"}
	if !slices.Equal(runs, want) {
		t.Fatalf("invocations (spawn, inject, resume) = %q, want %q", runs, want)
	}
}

// drainUntilResult reads events until a terminal result.
func drainUntilResult(t *testing.T, events <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
			if _, isResult := ev.(agent.ResultEvent); isResult {
				return out
			}
		case <-deadline:
			t.Fatalf("no result within %s (got %d events)", timeout, len(out))
		}
	}
}
