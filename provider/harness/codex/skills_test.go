package codex

import (
	"context"
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

// codexTestSkill is a sealed package whose SKILL.md and fallback body name
// id, plus any extra files.
func codexTestSkill(id string, extra ...stagewire.SkillFile) stagewire.Skill {
	files := append([]stagewire.SkillFile{
		agent.NewSkillFile("SKILL.md", []byte("---\nname: "+id+"\ndescription: Test skill.\n---\nBody of "+id+".\n"), 0o644),
	}, extra...)
	return agent.SealSkillPackage(stagewire.Skill{ID: id, Name: id, Version: "1.0.0", Body: id + " fallback body", Files: files})
}

func codexSkillDelivery(t *testing.T, mode agent.PromptSessionMode, skills ...stagewire.Skill) *agent.SkillDelivery {
	t.Helper()
	manifest := (&Provider{}).Manifest()
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: mode, Skills: skills,
	})
	if err != nil {
		t.Fatalf("PlanSkillDelivery: %v", err)
	}
	return delivery
}

func codexSkillSpec(t *testing.T, cwd string, skills ...stagewire.Skill) agent.Spec {
	t.Helper()
	return agent.Spec{
		Prompt: "use the skills", Cwd: cwd, SystemPromptAppend: "base instructions",
		Skills: codexSkillDelivery(t, agent.PromptModeAutonomous, skills...),
	}
}

// newSkillProvider builds a Provider over the fake app-server WITHOUT
// starting it, so the first Spawn/Resume starts it the way production does.
// configure runs with the private home before the fake starts reading.
func newSkillProvider(t *testing.T, configure func(fs *fakeServer, home string)) (*Provider, *fakeServer) {
	t.Helper()
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR,
		verifyMCPReadback: true, configTempDir: t.TempDir(),
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	if configure != nil {
		configure(fs, p.config.home)
	}
	go fs.run(t, "thread-S")
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	return p, fs
}

// listHomeSkills answers skills/list the way codex does for its user skills
// root: every <home>/skills/*/SKILL.md, at user scope, enabled unless
// enabled says otherwise.
func listHomeSkills(home string, enabled func(path string) bool) func(map[string]any) (any, int) {
	return func(params map[string]any) (any, int) {
		matches, _ := filepath.Glob(filepath.Join(home, codexSkillsRoot, "*", "SKILL.md"))
		skills := []map[string]any{}
		for _, path := range matches {
			skills = append(skills, map[string]any{
				"name": filepath.Base(filepath.Dir(path)), "description": "d", "path": path,
				"enabled": enabled == nil || enabled(path), "scope": "user",
			})
		}
		cwd := ""
		if cwds, ok := params["cwds"].([]any); ok && len(cwds) > 0 {
			cwd, _ = cwds[0].(string)
		}
		return map[string]any{"data": []map[string]any{{"cwd": cwd, "errors": []any{}, "skills": skills}}}, 0
	}
}

func recordedParams(fs *fakeServer, method string) []map[string]any {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []map[string]any
	for i, m := range fs.methods {
		if m == method {
			out = append(out, fs.params[i])
		}
	}
	return out
}

func methodIndex(fs *fakeServer, method string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return slices.Index(fs.methods, method)
}

func developerInstructions(t *testing.T, fs *fakeServer) string {
	t.Helper()
	starts := recordedParams(fs, "thread/start")
	if len(starts) != 1 {
		t.Fatalf("thread/start count = %d, want 1", len(starts))
	}
	text, _ := starts[0]["developerInstructions"].(string)
	return text
}

func turnInputText(params map[string]any) string {
	var parts []string
	input, _ := params["input"].([]any)
	for _, raw := range input {
		if part, ok := raw.(map[string]any); ok {
			text, _ := part["text"].(string)
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// TestSkillPlanGolden_Codex pins this harness's plan: a package goes to a
// reserved directory in the private home, a body-only skill rides the
// prompt, the repository policy is recorded as all, and an interactive
// session (a PTY with no app-server listing) keeps every skill on the
// prompt and refuses a native_required one.
func TestSkillPlanGolden_Codex(t *testing.T) {
	t.Parallel()
	manifest := (&Provider{}).Manifest()
	pkg := codexTestSkill("review", agent.NewSkillFile("scripts/lint.sh", []byte("#!/bin/sh\n"), 0o755))
	cases := []struct {
		name string
		mode agent.PromptSessionMode
		want string
	}{
		{"headless", agent.PromptModeAutonomous, "harness=codex layout=harness_home scripts=allowed trust=false repo=none->all\n" +
			"review native dir=donmai-review files=SKILL.md:0644,scripts/lint.sh:0755\n" +
			"legacy prompt reason=no_files"},
		{"interactive", agent.PromptModeHumanControlled, "harness=codex layout= scripts=allowed trust=false repo=none->all\n" +
			"review prompt reason=session_mode_prompt_only\n" +
			"legacy prompt reason=session_mode_prompt_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			delivery, residue, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
				Harness: manifest.Name, Profile: manifest.Skills, Mode: tc.mode,
				Skills:     []stagewire.Skill{pkg, {ID: "legacy", Body: "legacy body"}},
				RepoSkills: &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := delivery.Plan.Summary(); got != tc.want {
				t.Fatalf("plan =\n%s\nwant\n%s", got, tc.want)
			}
			if wantResidue := strings.Count(tc.want, " prompt "); len(residue) != wantResidue {
				t.Fatalf("residue = %d, want %d", len(residue), wantResidue)
			}
		})
	}

	required := pkg
	required.Delivery = stagewire.SkillDeliveryNativeRequired
	if _, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: manifest.Name, Profile: manifest.Skills, Mode: agent.PromptModeAutonomous, Skills: []stagewire.Skill{required}}); err != nil {
		t.Fatalf("headless native_required: %v, want a native plan", err)
	}
	_, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{Harness: manifest.Name, Profile: manifest.Skills, Mode: agent.PromptModeHumanControlled, Skills: []stagewire.Skill{required}})
	var deliveryErr *agent.SkillDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Reason != agent.SkillReasonModePromptOnly {
		t.Fatalf("interactive native_required: err = %v, want a session_mode_prompt_only refusal", err)
	}
}

// TestSkillMaterializer_WritesHomeLayoutWithReadback writes the native
// entries to <home>/skills/donmai-<id>, with the planned modes, and reads
// them back against the plan.
func TestSkillMaterializer_WritesHomeLayoutWithReadback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	delivery := codexSkillDelivery(t, agent.PromptModeAutonomous,
		codexTestSkill("review", agent.NewSkillFile("scripts/lint.sh", []byte("#!/bin/sh\n"), 0o755)))
	got, err := skillMaterializer{home: home}.MaterializeSkills(agent.Spec{Skills: delivery})
	if err != nil {
		t.Fatalf("MaterializeSkills: %v", err)
	}
	if got.Args != nil || got.TrustWorkspace {
		t.Fatalf("materialized = %+v, want no argv and no trust", got)
	}
	dir := filepath.Join(home, "skills", "donmai-review")
	body, err := os.ReadFile(filepath.Join(dir, "SKILL.md")) //nolint:gosec // G304: test-local.
	if err != nil || !strings.Contains(string(body), "Body of review.") {
		t.Fatalf("SKILL.md = %q, %v", body, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "scripts", "lint.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("script: %v %v", info, err)
	}
	native, err := delivery.Native()
	if err != nil {
		t.Fatal(err)
	}
	if verified, fallback := (skillMaterializer{home: home}).verify(native); len(verified) != 1 || len(fallback) != 0 {
		t.Fatalf("readback: verified %d, fallback %+v", len(verified), fallback)
	}
	if err := got.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left %s: %v", dir, err)
	}

	// No native entry: nothing written.
	if none, err := (skillMaterializer{home: home}).MaterializeSkills(agent.Spec{}); err != nil || none.Cleanup != nil {
		t.Fatalf("empty spec: %+v %v", none, err)
	}
}

// TestSkillMaterializer_DetectsRemovedOrCorruptFile re-reads skills an
// earlier launch wrote: a removed file and a changed file both fail the
// readback, fall back, and the changed directory is removed so codex cannot
// keep loading it.
func TestSkillMaterializer_DetectsRemovedOrCorruptFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	delivery := codexSkillDelivery(t, agent.PromptModeAutonomous,
		codexTestSkill("alpha"),
		codexTestSkill("beta", agent.NewSkillFile("notes.md", []byte("notes"), 0o644)),
		codexTestSkill("gamma"))
	native, err := delivery.Native()
	if err != nil {
		t.Fatal(err)
	}
	m := skillMaterializer{home: home}
	if written, fallback := m.write(native); len(written) != 3 || len(fallback) != 0 {
		t.Fatalf("write: written %d, fallback %+v", len(written), fallback)
	}
	if err := os.WriteFile(filepath.Join(home, "skills", "donmai-alpha", "SKILL.md"), []byte("planted"), 0o644); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "skills", "donmai-beta", "notes.md")); err != nil {
		t.Fatal(err)
	}
	verified, fallback := m.verify(native)
	if ids := skillIDs(verified); !slices.Equal(ids, []string{"gamma"}) {
		t.Fatalf("verified = %v, want [gamma]", ids)
	}
	var failed []string
	for _, f := range fallback {
		if f.reason != agent.SkillReasonNativeUnconfirmed {
			t.Fatalf("fallback reason = %s, want %s", f.reason, agent.SkillReasonNativeUnconfirmed)
		}
		failed = append(failed, f.skill.Entry.ID)
	}
	if !slices.Equal(failed, []string{"alpha", "beta"}) {
		t.Fatalf("fallback = %v, want [alpha beta]", failed)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := os.Lstat(filepath.Join(home, "skills", "donmai-"+id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("changed skill %s left in the home: %v", id, err)
		}
	}
}

// TestSkillMaterializer_RefusesTraversalAndOversize keeps an unsafe package
// out of the home twice over: the planner never makes it native, and an
// entry that reaches the materializer anyway (its plan and content
// travelled apart) is refused before anything is written.
func TestSkillMaterializer_RefusesTraversalAndOversize(t *testing.T) {
	t.Parallel()
	manifest := (&Provider{}).Manifest()
	escape := codexTestSkill("escape", agent.NewSkillFile("../escape.md", []byte("x"), 0o644))
	huge := codexTestSkill("huge", agent.NewSkillFile("big.bin", make([]byte, agent.MaxSkillBytes+1), 0o644))
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: manifest.Name, Profile: manifest.Skills, Mode: agent.PromptModeAutonomous,
		Skills: []stagewire.Skill{escape, huge},
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantPlan = "harness=codex layout=harness_home scripts=allowed trust=false repo=all->all\n" +
		"escape prompt reason=invalid_package\n" +
		"huge prompt reason=invalid_package"
	if got := delivery.Plan.Summary(); got != wantPlan {
		t.Fatalf("plan =\n%s\nwant\n%s", got, wantPlan)
	}

	// Hand-built entries that skip the planner.
	base := codexSkillDelivery(t, agent.PromptModeAutonomous, codexTestSkill("review"))
	native, err := base.Native()
	if err != nil {
		t.Fatal(err)
	}
	withFile := func(id string, file stagewire.SkillFile) agent.NativeSkill {
		skill := native[0]
		skill.Entry.ID, skill.Entry.Dir = id, agent.InTreeSkillDirPrefix+id
		skill.Entry.Files = append(append([]agent.SkillPlannedFile(nil), skill.Entry.Files...), agent.SkillPlannedFile{Path: file.Path, Mode: 0o644, SHA256: file.SHA256})
		skill.Package.Files = append(append([]stagewire.SkillFile(nil), skill.Package.Files...), file)
		return skill
	}
	traversalFile := withFile("trav", agent.NewSkillFile("../../escape.md", []byte("x"), 0o644))
	traversalDir := native[0]
	traversalDir.Entry.Dir = "../outside"
	oversize := withFile("big", agent.NewSkillFile("big.bin", make([]byte, agent.MaxSkillBytes+1), 0o644))

	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	written, fallback := skillMaterializer{home: home}.write([]agent.NativeSkill{traversalFile, traversalDir, oversize})
	if len(written) != 0 || len(fallback) != 3 {
		t.Fatalf("written %d, fallback %+v, want every entry refused", len(written), fallback)
	}
	for _, f := range fallback {
		if f.reason != agent.SkillReasonInvalidPackage {
			t.Fatalf("%s: reason %s (%s), want %s", f.skill.Entry.ID, f.reason, f.detail, agent.SkillReasonInvalidPackage)
		}
	}
	for _, dir := range []string{parent, home} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != "home" {
				t.Fatalf("%s holds %q after refused writes", dir, entry.Name())
			}
		}
	}
}

// TestProvider_SkillsWrittenBeforeStartAndConfirmedBeforeThread drives the
// real Spawn: the skill is in the private home when the app-server
// initializes, skills/list (forceReload, the session cwd) confirms it
// before thread/start, its body stays off the prompt, and the working
// directory is left exactly as it was.
func TestProvider_SkillsWrittenBeforeStartAndConfirmedBeforeThread(t *testing.T) {
	t.Parallel()
	var atInit []string
	p, fs := newSkillProvider(t, func(fs *fakeServer, home string) {
		fs.onInitialize = func() {
			atInit, _ = filepath.Glob(filepath.Join(home, "skills", "*", "SKILL.md"))
		}
		fs.skillsList = listHomeSkills(home, nil)
	})
	cwd := t.TempDir()
	h, err := p.Spawn(t.Context(), codexSkillSpec(t, cwd, codexTestSkill("review")))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)

	want := filepath.Join(p.config.home, "skills", "donmai-review", "SKILL.md")
	if !slices.Equal(atInit, []string{want}) {
		t.Fatalf("skills at initialize = %v, want [%s]", atInit, want)
	}
	if body, err := os.ReadFile(want); err != nil || !strings.Contains(string(body), "Body of review.") { //nolint:gosec // G304: test-local.
		t.Fatalf("SKILL.md = %q, %v", body, err)
	}
	listAt, threadAt := methodIndex(fs, "skills/list"), methodIndex(fs, "thread/start")
	if listAt < 0 || threadAt < listAt {
		t.Fatalf("skills/list at %d, thread/start at %d: want the listing first", listAt, threadAt)
	}
	list := recordedParams(fs, "skills/list")[0]
	if cwds, _ := list["cwds"].([]any); list["forceReload"] != true || len(cwds) != 1 || cwds[0] != cwd {
		t.Fatalf("skills/list params = %v, want forceReload and cwds [%s]", list, cwd)
	}
	if got := developerInstructions(t, fs); got != "base instructions" {
		t.Fatalf("developerInstructions = %q, want the native skill off the prompt", got)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory holds %v (%v), want nothing written", entries, err)
	}
}

// TestProvider_UnconfirmedSkillFallsBackToPrompt pins the readback gate: a
// skill skills/list does not report as an enabled user skill, or every
// skill when the listing fails, rides developerInstructions instead; a
// native_required one refuses the spawn before thread/start.
func TestProvider_UnconfirmedSkillFallsBackToPrompt(t *testing.T) {
	t.Parallel()
	notBeta := func(path string) bool { return !strings.Contains(path, "donmai-beta") }
	cases := []struct {
		name      string
		list      func(home string) func(map[string]any) (any, int)
		inPrompt  []string
		offPrompt []string
	}{
		{
			"not listed",
			func(home string) func(map[string]any) (any, int) {
				all := listHomeSkills(home, nil)
				return func(params map[string]any) (any, int) {
					result, _ := all(params)
					data := result.(map[string]any)["data"].([]map[string]any)
					skills := data[0]["skills"].([]map[string]any)
					data[0]["skills"] = slices.DeleteFunc(skills, func(s map[string]any) bool { return !notBeta(s["path"].(string)) })
					return result, 0
				}
			},
			[]string{"beta fallback body"},
			[]string{"alpha fallback body"},
		},
		{
			"listed disabled",
			func(home string) func(map[string]any) (any, int) { return listHomeSkills(home, notBeta) },
			[]string{"beta fallback body"},
			[]string{"alpha fallback body"},
		},
		{
			"listing failed",
			func(string) func(map[string]any) (any, int) {
				return func(map[string]any) (any, int) { return nil, -32601 }
			},
			[]string{"alpha fallback body", "beta fallback body"},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, fs := newSkillProvider(t, func(fs *fakeServer, home string) { fs.skillsList = tc.list(home) })
			h, err := p.Spawn(t.Context(), codexSkillSpec(t, t.TempDir(), codexTestSkill("alpha"), codexTestSkill("beta")))
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			_ = drainEvents(t, h.Events(), 5*time.Second)
			got := developerInstructions(t, fs)
			if !strings.HasPrefix(got, "base instructions\n\n") {
				t.Fatalf("developerInstructions = %q, want the fallback after the base", got)
			}
			for _, body := range tc.inPrompt {
				if !strings.Contains(got, body) {
					t.Fatalf("developerInstructions = %q, want %q", got, body)
				}
			}
			for _, body := range tc.offPrompt {
				if strings.Contains(got, body) {
					t.Fatalf("developerInstructions = %q, confirmed skill %q is on the prompt", got, body)
				}
			}
		})
	}

	t.Run("native_required refuses", func(t *testing.T) {
		t.Parallel()
		p, fs := newSkillProvider(t, func(fs *fakeServer, home string) { fs.skillsList = listHomeSkills(home, notBeta) })
		beta := codexTestSkill("beta")
		beta.Delivery = stagewire.SkillDeliveryNativeRequired
		_, err := p.Spawn(t.Context(), codexSkillSpec(t, t.TempDir(), codexTestSkill("alpha"), beta))
		var deliveryErr *agent.SkillDeliveryError
		if !errors.Is(err, agent.ErrSpawnFailed) || !errors.As(err, &deliveryErr) || deliveryErr.SkillID != "beta" || deliveryErr.Reason != agent.SkillReasonNativeUnconfirmed {
			t.Fatalf("Spawn error = %v, want a native_unconfirmed refusal for beta", err)
		}
		if methodIndex(fs, "thread/start") >= 0 {
			t.Fatal("thread/start was sent after a native_required skill was not confirmed")
		}
	})
}

// TestProvider_ResumeRereadsSkillsAndFallsBackOnTamper reuses the started
// app-server: the identical skill set is re-read, not rewritten; a changed
// or removed file falls back to the resumed turn's input, and the changed
// directories are removed.
func TestProvider_ResumeRereadsSkillsAndFallsBackOnTamper(t *testing.T) {
	t.Parallel()
	var home string
	p, fs := newSkillProvider(t, func(fs *fakeServer, h string) {
		home = h
		fs.skillsList = listHomeSkills(h, nil)
	})
	spec := codexSkillSpec(t, t.TempDir(),
		codexTestSkill("alpha"),
		codexTestSkill("beta", agent.NewSkillFile("notes.md", []byte("notes"), 0o644)),
		codexTestSkill("gamma"))
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)
	if got := developerInstructions(t, fs); got != "base instructions" {
		t.Fatalf("developerInstructions = %q, want every skill native", got)
	}

	if err := os.WriteFile(filepath.Join(home, "skills", "donmai-alpha", "SKILL.md"), []byte("planted"), 0o644); err != nil { //nolint:gosec // G306: test fixture.
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "skills", "donmai-beta", "notes.md")); err != nil {
		t.Fatal(err)
	}
	r, err := p.Resume(t.Context(), "thread-S", spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainEvents(t, r.Events(), 5*time.Second)

	turns := recordedParams(fs, "turn/start")
	if len(turns) != 2 {
		t.Fatalf("turn/start count = %d, want 2", len(turns))
	}
	resumed := turnInputText(turns[1])
	for _, body := range []string{"alpha fallback body", "beta fallback body"} {
		if !strings.Contains(resumed, body) {
			t.Fatalf("resumed turn input = %q, want %q", resumed, body)
		}
	}
	if strings.Contains(resumed, "gamma fallback body") || strings.Contains(turnInputText(turns[0]), "fallback body") {
		t.Fatalf("an intact native skill reached a turn's input: %q / %q", turnInputText(turns[0]), resumed)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := os.Lstat(filepath.Join(home, "skills", "donmai-"+id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tampered skill %s left in the home: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "donmai-gamma", "SKILL.md")); err != nil {
		t.Fatalf("intact skill gamma: %v", err)
	}
}

// TestProvider_StartedAppServerUsesPromptFallback covers an app-server that
// started before the session's skills could be written (here a probe start):
// nothing is written into its home and every native skill rides the prompt.
func TestProvider_StartedAppServerUsesPromptFallback(t *testing.T) {
	t.Parallel()
	p, fs := newSkillProvider(t, func(fs *fakeServer, home string) { fs.skillsList = listHomeSkills(home, nil) })
	if err := p.ensureStarted(); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	h, err := p.Spawn(t.Context(), codexSkillSpec(t, t.TempDir(), codexTestSkill("review")))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)
	if got := developerInstructions(t, fs); got != "base instructions\n\nreview fallback body" {
		t.Fatalf("developerInstructions = %q, want the body after the base", got)
	}
	if _, err := os.Lstat(filepath.Join(p.config.home, "skills", "donmai-review")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skill written into a started app-server's home: %v", err)
	}
}

// TestSkillAuthority_CodexBindsNativeFileDigests pins that the harness
// authority digest binds each native skill file's sha256 for this harness,
// as for every harness with a native layout.
func TestSkillAuthority_CodexBindsNativeFileDigests(t *testing.T) {
	t.Parallel()
	manifest := (&Provider{}).Manifest()
	spec := codexSkillSpec(t, "/work/repo", codexTestSkill("review"))
	entries := spec.Skills.Plan.Entries
	if len(entries) != 1 || entries[0].Delivery != agent.SkillDeliveredNative || len(entries[0].Files) != 1 || entries[0].Files[0].SHA256 == "" {
		t.Fatalf("plan entries = %+v, want one native entry with its file digest", entries)
	}
	compile := func(spec agent.Spec) map[string]string {
		t.Helper()
		plan, _ := agent.CompilePreparedHarness(spec, manifest, "operational", nil, nil)
		if plan == nil || plan.AuthorityFieldDigests["skills"] == "" {
			t.Fatalf("prepared harness = %+v, want a skills authority digest", plan)
		}
		return plan.AuthorityFieldDigests
	}
	before := compile(spec)
	tampered := spec
	tampered.Skills = spec.Skills.Clone()
	tampered.Skills.Plan.Entries[0].Files[0].SHA256 = strings.Repeat("0", 64)
	after := compile(tampered)
	if before["skills"] == after["skills"] {
		t.Fatal("changing a native file's sha256 left the skills authority digest unchanged")
	}
}
