package skillfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

// nativeSkill plans one valid package natively and returns it.
func nativeSkill(t *testing.T, id string) agent.NativeSkill {
	t.Helper()
	skill := agent.SealSkillPackage(stagewire.Skill{
		ID: id, Body: "fallback",
		Files: []stagewire.SkillFile{
			agent.NewSkillFile("SKILL.md", []byte("---\nname: "+id+"\ndescription: d\n---\nbody\n"), 0o644),
			agent.NewSkillFile("refs/deep/guide.md", []byte("guide"), 0o644),
			agent.NewSkillFile("scripts/run.sh", []byte("#!/bin/sh\necho ok\n"), 0o755),
		},
	})
	delivery, _, err := agent.PlanSkillDelivery(agent.SkillPlanInput{
		Harness: agent.HarnessPi, Mode: agent.PromptModeAutonomous,
		Profile: agent.SkillDeliveryProfile{Layout: agent.SkillLayoutStateDir},
		Skills:  []stagewire.Skill{skill},
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := delivery.Native()
	if err != nil || len(native) != 1 {
		t.Fatalf("plan: %d native, %v", len(native), err)
	}
	return native[0]
}

// TestWrite_LayoutAndModes writes a package and checks every file's bytes
// and exact mode, including nested directories and the executable bit.
func TestWrite_LayoutAndModes(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	skill := nativeSkill(t, "review")
	if err := Write(base, "state/skills/review", skill, Options{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, tc := range []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{"SKILL.md", "---\nname: review\ndescription: d\n---\nbody\n", 0o644},
		{"refs/deep/guide.md", "guide", 0o644},
		{"scripts/run.sh", "#!/bin/sh\necho ok\n", 0o755},
	} {
		full := filepath.Join(base, "state", "skills", "review", filepath.FromSlash(tc.path))
		got, err := os.ReadFile(full) //nolint:gosec // G304: test-local path under t.TempDir().
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if string(got) != tc.content {
			t.Errorf("%s content = %q, want %q", tc.path, got, tc.content)
		}
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Errorf("%s mode = %04o, want %04o", tc.path, info.Mode().Perm(), tc.mode)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "state", "skills", "review", agent.SkillMarkerFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker written without Options.Marker: %v", err)
	}
}

// TestWrite_DetectsTamperBetweenWriteAndReadBack is the TOCTOU rule: a file
// rewritten after Write wrote it and before the read-back fails the write,
// and the half-made target is removed.
func TestWrite_DetectsTamperBetweenWriteAndReadBack(t *testing.T) {
	base := t.TempDir()
	skill := nativeSkill(t, "review")
	afterWrite = func(b, dir string) {
		target := filepath.Join(b, filepath.FromSlash(dir), "SKILL.md")
		if err := os.WriteFile(target, []byte("swapped"), 0o644); err != nil { //nolint:gosec // G306: test fixture matching the planned mode.
			t.Errorf("tamper: %v", err)
		}
	}
	t.Cleanup(func() { afterWrite = nil })
	err := Write(base, "skills/review", skill, Options{})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("Write after tamper: err = %v, want ErrVerify", err)
	}
	if _, statErr := os.Lstat(filepath.Join(base, "skills", "review")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tampered target left behind: %v", statErr)
	}
}

// TestVerify_DetectsLaterTamper covers a resumed session's re-check: content
// or mode changed after a good write reads as a mismatch.
func TestVerify_DetectsLaterTamper(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tamper func(dir string) error
	}{
		{"content", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "refs", "deep", "guide.md"), []byte("evil"), 0o644) //nolint:gosec // G306: test fixture.
		}},
		{"mode", func(dir string) error {
			return os.Chmod(filepath.Join(dir, "SKILL.md"), 0o755) //nolint:gosec // G302: the tamper under test is a widened mode.
		}},
		{"replaced by link", func(dir string) error {
			target := filepath.Join(dir, "SKILL.md")
			if err := os.Remove(target); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(dir, "refs", "deep", "guide.md"), target)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			skill := nativeSkill(t, "review")
			if err := Write(base, "skills/review", skill, Options{}); err != nil {
				t.Fatal(err)
			}
			if err := Verify(base, "skills/review", skill.Entry); err != nil {
				t.Fatalf("Verify of an untouched write: %v", err)
			}
			if err := tc.tamper(filepath.Join(base, "skills", "review")); err != nil {
				t.Fatal(err)
			}
			if err := Verify(base, "skills/review", skill.Entry); !errors.Is(err, ErrVerify) {
				t.Fatalf("Verify after %s tamper: err = %v, want ErrVerify", tc.name, err)
			}
		})
	}
}

// TestWrite_RejectsTraversalAndLinks proves nothing escapes base: a target
// path with a traversal segment, a planned file path with one, and a
// symbolic link where a directory belongs are all refused before any byte
// lands outside.
func TestWrite_RejectsTraversalAndLinks(t *testing.T) {
	t.Parallel()
	t.Run("traversal in target dir", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		for _, dir := range []string{"../escape", "skills/../../escape", "/abs", "skills//x", `skills\x`} {
			if err := Write(base, dir, nativeSkill(t, "review"), Options{}); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Write(%q): err = %v, want ErrUnsafePath", dir, err)
			}
		}
	})
	t.Run("traversal in a planned file path", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		base := filepath.Join(outside, "base")
		if err := os.Mkdir(base, 0o700); err != nil {
			t.Fatal(err)
		}
		skill := nativeSkill(t, "review")
		// A plan that bypassed validation still cannot write outside.
		skill.Entry.Files = append(skill.Entry.Files, agent.SkillPlannedFile{Path: "../../escaped.md", Mode: 0o644, SHA256: skill.Entry.Files[0].SHA256})
		if err := Write(base, "skills/review", skill, Options{}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "escaped.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a file escaped base: %v", err)
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		t.Parallel()
		base, elsewhere := t.TempDir(), t.TempDir()
		if err := os.Symlink(elsewhere, filepath.Join(base, "skills")); err != nil {
			t.Fatal(err)
		}
		if err := Write(base, "skills/review", nativeSkill(t, "review"), Options{Replace: true}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatalf("write followed the link: %v", entries)
		}
	})
	t.Run("symlinked target", func(t *testing.T) {
		t.Parallel()
		base, elsewhere := t.TempDir(), t.TempDir()
		if err := os.Mkdir(filepath.Join(base, "skills"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(base, "skills", "review")); err != nil {
			t.Fatal(err)
		}
		if err := Write(base, "skills/review", nativeSkill(t, "review"), Options{Replace: true}); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})
}

// TestWrite_ExistingTarget proves a repository's own skill directory is
// never overwritten, a runner-marked directory is, and Replace owns the
// location outright.
func TestWrite_ExistingTarget(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, marker bool) (string, string) {
		t.Helper()
		base := t.TempDir()
		dir := filepath.Join(base, ".agents", "skills", "donmai-review")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("the repository's own"), 0o600); err != nil {
			t.Fatal(err)
		}
		if marker {
			if err := os.WriteFile(filepath.Join(dir, agent.SkillMarkerFile), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return base, dir
	}
	t.Run("unmarked repository directory is kept", func(t *testing.T) {
		t.Parallel()
		base, dir := seed(t, false)
		err := Write(base, ".agents/skills/donmai-review", nativeSkill(t, "review"), Options{Marker: true})
		if !errors.Is(err, ErrTargetExists) {
			t.Fatalf("err = %v, want ErrTargetExists", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(got) != "the repository's own" { //nolint:gosec // G304: test-local.
			t.Fatalf("repository skill overwritten: %q", got)
		}
	})
	t.Run("marked directory is replaced", func(t *testing.T) {
		t.Parallel()
		base, dir := seed(t, true)
		skill := nativeSkill(t, "review")
		if err := Write(base, ".agents/skills/donmai-review", skill, Options{Marker: true}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		marker, err := os.ReadFile(filepath.Join(dir, agent.SkillMarkerFile)) //nolint:gosec // G304: test-local.
		if err != nil || strings.TrimSpace(string(marker)) != skill.Entry.Digest {
			t.Fatalf("marker = %q, %v; want the package digest", marker, err)
		}
	})
	t.Run("replace owns the location", func(t *testing.T) {
		t.Parallel()
		base, _ := seed(t, false)
		if err := Write(base, ".agents/skills/donmai-review", nativeSkill(t, "review"), Options{Replace: true}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})
}

// TestRemove_StaysInsideBase proves Remove deletes the target and refuses a
// path that leaves base.
func TestRemove_StaysInsideBase(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := Write(base, "skills/review", nativeSkill(t, "review"), Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(base, "skills/review"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "skills", "review")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target still present: %v", err)
	}
	if err := Remove(base, "../x"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Remove(../x): err = %v", err)
	}
}
