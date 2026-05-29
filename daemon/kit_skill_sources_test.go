package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/RenseiAI/donmai/runner"
)

// writeSkillKit writes a .kit.toml manifest plus its referenced skill files
// into scanDir. The manifest declares a [detect] files matcher so the kit
// applies only when the repo contains detectFile, mirroring how real kits
// gate on repo markers (e.g. go.mod).
func writeSkillKit(t *testing.T, scanDir, id string, priority int, detectFile string, skillFiles []string) {
	t.Helper()
	b := "api = \"v1\"\n\n[kit]\nid = \"" + id + "\"\nname = \"" + id + "\"\nversion = \"1.0.0\"\n"
	if priority != 0 {
		b += "priority = " + strconv.Itoa(priority) + "\n"
	}
	if detectFile != "" {
		b += "\n[detect]\nfiles = [\"" + detectFile + "\"]\n"
	}
	if len(skillFiles) > 0 {
		b += "\n[provide]\n"
		for _, sf := range skillFiles {
			b += "[[provide.skills]]\nfile = \"" + sf + "\"\n"
		}
	}
	manifestPath := filepath.Join(scanDir, sanitizeKitFilename(id)+".kit.toml")
	if err := os.WriteFile(manifestPath, []byte(b), 0o600); err != nil {
		t.Fatalf("write manifest %s: %v", id, err)
	}
	// Materialize the skill files next to the manifest so the resolution-root
	// contract (paths relative to the manifest dir) is honored if exercised.
	for _, sf := range skillFiles {
		p := filepath.Join(scanDir, sf)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir for skill %s: %v", sf, err)
		}
		if err := os.WriteFile(p, []byte("# skill "+sf+"\nbody\n"), 0o600); err != nil {
			t.Fatalf("write skill %s: %v", sf, err)
		}
	}
}

func TestKitRegistry_SkillSourcesForRepo(t *testing.T) {
	scanDir := t.TempDir()
	// go-kit: applies when go.mod present, declares two skills, priority 50.
	writeSkillKit(t, scanDir, "go-kit", 50, "go.mod", []string{"skills/go.md", "skills/lint.md"})
	// noskill-kit: applicable (same detect file) but declares no skills.
	writeSkillKit(t, scanDir, "noskill-kit", 10, "go.mod", nil)

	reg := NewKitRegistry([]string{scanDir})

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	sources, err := reg.SkillSourcesForRepo(repo, "linux")
	if err != nil {
		t.Fatalf("SkillSourcesForRepo: %v", err)
	}
	// noskill-kit must be omitted (no [provide.skills]); only go-kit remains.
	if len(sources) != 1 {
		t.Fatalf("want 1 skill source, got %d: %+v", len(sources), sources)
	}
	src := sources[0]
	if src.ID != "go-kit" {
		t.Errorf("ID = %q, want go-kit", src.ID)
	}
	if src.Priority != 50 {
		t.Errorf("Priority = %d, want 50", src.Priority)
	}
	wantManifest := filepath.Join(scanDir, "go-kit.kit.toml")
	if src.ManifestPath != wantManifest {
		t.Errorf("ManifestPath = %q, want %q", src.ManifestPath, wantManifest)
	}
	if len(src.SkillFiles) != 2 || src.SkillFiles[0] != "skills/go.md" || src.SkillFiles[1] != "skills/lint.md" {
		t.Errorf("SkillFiles = %v, want [skills/go.md skills/lint.md]", src.SkillFiles)
	}
}

func TestKitRegistry_SkillSourcesForRepo_NoMatch(t *testing.T) {
	scanDir := t.TempDir()
	writeSkillKit(t, scanDir, "go-kit", 0, "go.mod", []string{"skills/go.md"})
	reg := NewKitRegistry([]string{scanDir})

	// Repo without go.mod: go-kit's [detect] does not match → no sources.
	empty := t.TempDir()
	sources, err := reg.SkillSourcesForRepo(empty, "linux")
	if err != nil {
		t.Fatalf("SkillSourcesForRepo: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("want 0 skill sources for non-matching repo, got %+v", sources)
	}
}

func TestKitRegistry_SkillSourcesForRepo_DisabledOmitted(t *testing.T) {
	scanDir := t.TempDir()
	writeSkillKit(t, scanDir, "go-kit", 0, "go.mod", []string{"skills/go.md"})
	reg := NewKitRegistry([]string{scanDir})

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	// Active: one source.
	if got, err := reg.SkillSourcesForRepo(repo, "linux"); err != nil || len(got) != 1 {
		t.Fatalf("active: want 1 source, got %d (err=%v)", len(got), err)
	}

	// Disable the kit → DetectForRepo filters it out → no sources.
	if _, err := reg.Disable("go-kit"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, err := reg.SkillSourcesForRepo(repo, "linux")
	if err != nil {
		t.Fatalf("SkillSourcesForRepo after disable: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("disabled kit should yield no sources, got %+v", got)
	}
}

// TestKitRegistry_SkillSourcesForRepo_FlowsIntoRunnerOptions verifies the
// end-to-end hand-off: what SkillSourcesForRepo returns is exactly what
// populates runner.Options.KitSkillSources at the runner-construction site
// (afcli/agent_run.go runAgentRun). The runner consumes these at loop step 5a.
func TestKitRegistry_SkillSourcesForRepo_FlowsIntoRunnerOptions(t *testing.T) {
	scanDir := t.TempDir()
	writeSkillKit(t, scanDir, "go-kit", 0, "go.mod", []string{"skills/go.md"})
	reg := NewKitRegistry([]string{scanDir})

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	sources, err := reg.SkillSourcesForRepo(repo, "linux")
	if err != nil {
		t.Fatalf("SkillSourcesForRepo: %v", err)
	}

	// Construct runner.Options the way runAgentRun does.
	opts := runner.Options{
		KitSkillSources: sources,
		KitTargetOS:     "linux",
	}
	if len(opts.KitSkillSources) != 1 {
		t.Fatalf("runner.Options.KitSkillSources len = %d, want 1: %+v",
			len(opts.KitSkillSources), opts.KitSkillSources)
	}
	wantManifest := filepath.Join(scanDir, "go-kit.kit.toml")
	if opts.KitSkillSources[0].ManifestPath != wantManifest {
		t.Errorf("ManifestPath = %q, want %q", opts.KitSkillSources[0].ManifestPath, wantManifest)
	}
	if len(opts.KitSkillSources[0].SkillFiles) != 1 || opts.KitSkillSources[0].SkillFiles[0] != "skills/go.md" {
		t.Errorf("SkillFiles = %v, want [skills/go.md]", opts.KitSkillSources[0].SkillFiles)
	}
}
