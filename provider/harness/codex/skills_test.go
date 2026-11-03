package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestDeliverCardSkills_WritesExpectedFiles pins the exact SKILL.md bytes:
// frontmatter with the namespaced directory name and a derived
// single-line description, then the card body verbatim. Tool
// restrictions stay out of the file — they are enforced runner-side.
func TestDeliverCardSkills_WritesExpectedFiles(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	skills := []agent.CardSkill{
		{
			ID:              "Spring-Debug",
			Body:            "# Spring Debugging\nCheck circular deps first.",
			DisallowedTools: []string{"Bash(rm:*)"},
		},
		{
			ID:   "net",
			Body: "Restricts \"curl\" \\ usage.\nSecond line stays in the body.",
		},
	}
	if err := deliverCardSkills(home, skills); err != nil {
		t.Fatalf("deliverCardSkills: %v", err)
	}

	wantSpring := "---\nname: donmai-spring-debug\ndescription: \"# Spring Debugging\"\n---\n# Spring Debugging\nCheck circular deps first.\n"
	gotSpring, err := os.ReadFile(filepath.Join(home, "skills", "donmai-spring-debug", "SKILL.md"))
	if err != nil {
		t.Fatalf("read spring skill: %v", err)
	}
	if string(gotSpring) != wantSpring {
		t.Errorf("spring SKILL.md:\n got %q\nwant %q", gotSpring, wantSpring)
	}
	if strings.Contains(string(gotSpring), "Bash(rm") {
		t.Errorf("tool restrictions must stay runner-side, got %q", gotSpring)
	}

	wantNet := "---\nname: donmai-net\ndescription: \"Restricts \\\"curl\\\" \\\\ usage.\"\n---\nRestricts \"curl\" \\ usage.\nSecond line stays in the body.\n"
	gotNet, err := os.ReadFile(filepath.Join(home, "skills", "donmai-net", "SKILL.md"))
	if err != nil {
		t.Fatalf("read net skill: %v", err)
	}
	if string(gotNet) != wantNet {
		t.Errorf("net SKILL.md:\n got %q\nwant %q", gotNet, wantNet)
	}

	// Boundary modes: dirs 0700, files 0600.
	for _, dir := range []string{
		filepath.Join(home, "skills"),
		filepath.Join(home, "skills", "donmai-spring-debug"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != codexHomeMode {
			t.Errorf("%s mode = %04o, want %04o", dir, info.Mode().Perm(), codexHomeMode)
		}
	}
	info, err := os.Stat(filepath.Join(home, "skills", "donmai-spring-debug", "SKILL.md"))
	if err != nil {
		t.Fatalf("stat SKILL.md: %v", err)
	}
	if info.Mode().Perm() != codexConfigMode {
		t.Errorf("SKILL.md mode = %04o, want %04o", info.Mode().Perm(), codexConfigMode)
	}
}

// TestDeliverCardSkills_EmptyIsNoOp pins that skill-less sessions keep
// the byte-identical fresh-home shape: delivery touches nothing.
func TestDeliverCardSkills_EmptyIsNoOp(t *testing.T) {
	t.Parallel()

	for _, skills := range [][]agent.CardSkill{nil, {}} {
		home := t.TempDir()
		if err := deliverCardSkills(home, skills); err != nil {
			t.Fatalf("deliverCardSkills(%d skills): %v", len(skills), err)
		}
		entries, err := os.ReadDir(home)
		if err != nil {
			t.Fatalf("read home: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("empty delivery wrote %d entries: %v", len(entries), entries)
		}
	}
}

// TestCardSkillDirName_Sanitizes pins the wire-id → directory mapping:
// path-safe, YAML-safe, within the CLI's 64-character name limit, and
// never the CLI's own `.system` subtree.
func TestCardSkillDirName_Sanitizes(t *testing.T) {
	t.Parallel()

	longID := strings.Repeat("a", 100)
	cases := []struct {
		id   string
		want string
	}{
		{"spring-debug", "donmai-spring-debug"},
		{"Spring/Debug!", "donmai-spring-debug"},
		{"../escape", "donmai-escape"},
		{"a/../../b", "donmai-a-b"},
		{".system", "donmai-system"},
		{"", "donmai-skill"},
		{"...", "donmai-skill"},
		{"UPPER_09", "donmai-upper-09"},
		{longID, "donmai-" + strings.Repeat("a", 57)},
	}
	for _, tc := range cases {
		got := cardSkillDirName(tc.id, map[string]struct{}{})
		if got != tc.want {
			t.Errorf("cardSkillDirName(%q) = %q, want %q", tc.id, got, tc.want)
		}
		if len(got) > maxSkillNameLen {
			t.Errorf("cardSkillDirName(%q) = %d chars, over the %d limit", tc.id, len(got), maxSkillNameLen)
		}
		if strings.ContainsAny(got, "./\\") {
			t.Errorf("cardSkillDirName(%q) = %q, not path-safe", tc.id, got)
		}
	}
}

// TestDeliverCardSkills_DedupesCollisions pins deterministic
// disambiguation when distinct card ids sanitize alike: input order
// wins, later collisions take a numeric suffix, and every skill still
// lands on disk.
func TestDeliverCardSkills_DedupesCollisions(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	skills := []agent.CardSkill{
		{ID: "a", Body: "first"},
		{ID: "A", Body: "second"},
		{ID: "a!", Body: "third"},
	}
	if err := deliverCardSkills(home, skills); err != nil {
		t.Fatalf("deliverCardSkills: %v", err)
	}
	for dir, body := range map[string]string{
		"donmai-a":   "first",
		"donmai-a-2": "second",
		"donmai-a-3": "third",
	} {
		got, err := os.ReadFile(filepath.Join(home, "skills", dir, "SKILL.md"))
		if err != nil {
			t.Errorf("read %s: %v", dir, err)
			continue
		}
		if !strings.Contains(string(got), body) || !strings.Contains(string(got), "name: "+dir) {
			t.Errorf("%s SKILL.md missing body/name: %q", dir, got)
		}
	}
}

// TestCardSkillDescription_Edges pins the frontmatter description
// derivation: first non-empty line, whitespace-collapsed, rune-capped
// without splitting UTF-8, and never empty (the CLI drops a skill
// whose description is missing).
func TestCardSkillDescription_Edges(t *testing.T) {
	t.Parallel()

	if got := cardSkillDescription("donmai-x", ""); got != "Card skill donmai-x." {
		t.Errorf("empty body description = %q", got)
	}
	if got := cardSkillDescription("donmai-x", "   \n\t\n"); got != "Card skill donmai-x." {
		t.Errorf("whitespace body description = %q", got)
	}
	if got := cardSkillDescription("donmai-x", "\n\n  Real  line\there  \nsecond"); got != "Real line here" {
		t.Errorf("multiline description = %q", got)
	}
	long := strings.Repeat("é", 600)
	got := cardSkillDescription("donmai-x", long)
	if n := len([]rune(got)); n != maxSkillDescriptionRunes {
		t.Errorf("capped description = %d runes, want %d", n, maxSkillDescriptionRunes)
	}
	if got != strings.Repeat("é", 500) {
		t.Errorf("capped description mangled multibyte text")
	}
	if got := cardSkillDescription("donmai-x", "a\x00b\x07c"); got != "abc" {
		t.Errorf("control-char description = %q, want %q", got, "abc")
	}
}

// TestVerifySkillFileReadback_Mismatch pins the fail-closed readback: a
// tampered or missing file never verifies.
func TestVerifySkillFileReadback_Mismatch(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	content := renderCardSkillFile("donmai-x", "body")
	path := filepath.Join(home, "SKILL.md")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifySkillFileReadback(path, content); err != nil {
		t.Fatalf("pristine readback: %v", err)
	}
	if err := os.WriteFile(path, append(content, 'x'), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := verifySkillFileReadback(path, content); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered readback err = %v, want a mismatch", err)
	}
	if err := verifySkillFileReadback(filepath.Join(home, "absent.md"), content); err == nil {
		t.Fatal("missing file readback succeeded")
	}
}

// TestDeliverCardSkills_RefusesSymlinks pins that a planted link in the
// skills tree denies delivery: a resumed session re-delivers into a
// home its previous agent could have learned, so a link must fail the
// spawn rather than redirect the write.
func TestDeliverCardSkills_RefusesSymlinks(t *testing.T) {
	t.Parallel()

	// Link standing where a skill directory should be.
	home := t.TempDir()
	root := filepath.Join(home, "skills")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "donmai-x")); err != nil {
		t.Fatalf("plant dir link: %v", err)
	}
	err := deliverCardSkills(home, []agent.CardSkill{{ID: "x", Body: "b"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("dir-link delivery err = %v, want a symlink refusal", err)
	}

	// Link standing where a SKILL.md should be.
	home2 := t.TempDir()
	dir := filepath.Join(home2, "skills", "donmai-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.md")
	if err := os.WriteFile(target, []byte("elsewhere"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatalf("plant file link: %v", err)
	}
	err = deliverCardSkills(home2, []agent.CardSkill{{ID: "x", Body: "b"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("file-link delivery err = %v, want a symlink refusal", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "elsewhere" {
		t.Errorf("link target rewritten to %q", got)
	}
}

// TestDeliverCardSkills_UnwritableHome pins that an I/O failure fails
// the batch instead of delivering silently partial skills.
func TestDeliverCardSkills_UnwritableHome(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	if err := deliverCardSkills(home, []agent.CardSkill{{ID: "x", Body: "b"}}); err == nil {
		t.Fatal("delivery into a read-only home succeeded")
	}
}

// TestSpawn_DeliversCardSkillsToIsolatedHome is the wiring proof: a
// headless Spawn renders Spec.Skills into the session's isolated home
// (and nowhere near the worktree). Remove the deliverSpecCardSkills
// call from Spawn and this goes red.
func TestSpawn_DeliversCardSkillsToIsolatedHome(t *testing.T) {
	t.Parallel()

	fs, stdinW, stdoutR := newFakeServer()
	t.Cleanup(fs.close)
	go fs.run(t, "thread-skills-1")

	boundaryParent := t.TempDir()
	p, err := New(Options{
		skipProcess:    true,
		stdinOverride:  stdinW,
		stdoutOverride: stdoutR,
		configTempDir:  boundaryParent,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	worktree := t.TempDir()
	skills := []agent.CardSkill{
		{ID: "go-review", Body: "Review carefully: check SKILL_BODY_MARKER.", DisallowedTools: []string{"Bash(git push:*)"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{Prompt: "do work", Cwd: worktree, Skills: skills})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	skillPath := filepath.Join(p.config.home, "skills", "donmai-go-review", "SKILL.md")
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read delivered skill: %v", err)
	}
	if !strings.Contains(string(got), "SKILL_BODY_MARKER") || !strings.Contains(string(got), "name: donmai-go-review") {
		t.Errorf("delivered SKILL.md missing body/name: %q", got)
	}
	// Nothing lands in the worktree diff.
	entries, err := os.ReadDir(worktree)
	if err != nil {
		t.Fatalf("read worktree: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("delivery polluted the worktree: %v", entries)
	}
}

// TestSpawn_CardSkillDeliveryFailureDeniesSpawn pins fail-closed
// admission: when the skills tree cannot be written, the spawn is
// refused with a typed error before any side effect.
func TestSpawn_CardSkillDeliveryFailureDeniesSpawn(t *testing.T) {
	t.Parallel()

	p, err := New(Options{skipProcess: true, configTempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// Sabotage the skills root: a regular file where the directory
	// must be, so delivery fails before the handshake runs.
	if err := os.WriteFile(filepath.Join(p.config.home, "skills"), []byte("blocker"), 0o600); err != nil {
		t.Fatalf("plant blocker: %v", err)
	}
	_, err = p.Spawn(t.Context(), agent.Spec{
		Prompt: "do work",
		Cwd:    t.TempDir(),
		Skills: []agent.CardSkill{{ID: "x", Body: "b"}},
	})
	if err == nil {
		t.Fatal("Spawn with undeliverable skills succeeded")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("err = %v, want %v", err, agent.ErrSpawnFailed)
	}
	if !strings.Contains(err.Error(), "card skills") {
		t.Errorf("err = %v, want it to name card skills", err)
	}
}

// TestResume_RedeliversCardSkills pins that a resume re-delivers into a
// fresh home: remove the deliverSpecCardSkills call from Resume and
// this goes red.
func TestResume_RedeliversCardSkills(t *testing.T) {
	t.Parallel()

	const threadID = "thread-skills-resume"
	p, _ := newResumeTestProvider(t, threadID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, threadID, agent.Spec{
		Prompt:     "continue",
		Cwd:        "/tmp/wt",
		Autonomous: true,
		Skills:     []agent.CardSkill{{ID: "go-review", Body: "RESUME_SKILL_MARKER body."}},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	got, err := os.ReadFile(filepath.Join(p.config.home, "skills", "donmai-go-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read re-delivered skill: %v", err)
	}
	if !strings.Contains(string(got), "RESUME_SKILL_MARKER") {
		t.Errorf("re-delivered SKILL.md missing body: %q", got)
	}
}

// TestDeliverSpecCardSkills_MissingBoundary pins fail-closed delivery
// when the Provider has no isolated home: no skills, no error; skills
// without a boundary, a typed refusal — never a write to an unintended
// location.
func TestDeliverSpecCardSkills_MissingBoundary(t *testing.T) {
	t.Parallel()

	var nilProvider *Provider
	if err := nilProvider.deliverSpecCardSkills(agent.Spec{Skills: []agent.CardSkill{{ID: "x"}}}); err == nil {
		t.Fatal("nil provider delivery succeeded")
	}
	if err := (&Provider{}).deliverSpecCardSkills(agent.Spec{Skills: []agent.CardSkill{{ID: "x"}}}); err == nil {
		t.Fatal("boundary-less provider delivery succeeded")
	}
	if err := (&Provider{}).deliverSpecCardSkills(agent.Spec{}); err != nil {
		t.Fatalf("skill-less delivery on a boundary-less provider: %v", err)
	}
}
