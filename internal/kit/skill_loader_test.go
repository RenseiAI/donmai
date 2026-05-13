package kit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/internal/kit"
)

// writeSkillFile writes content to dir/<name> for the test, creating
// intermediate directories as needed.
func writeSkillFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// fakeManifestPath returns a fake .kit.toml path rooted at dir so the
// skill loader resolves relative skill paths against dir.
func fakeManifestPath(dir string) string {
	return filepath.Join(dir, "spring.kit.toml")
}

const skillBodyPlain = `# Spring Test Debugging

When a Spring test fails with a BeanCreationException check for:
1. Missing @MockBean on collaborators.
2. Circular dependency in the test context.
`

const skillBodyWithTomlFrontmatter = `+++
[tools]
disallow = [{shell = "Bash(rm -rf *)"}]
+++

# Dangerous Skill

This skill restricts destructive shell commands.
`

// SKILL.md with YAML frontmatter following the agentskills.io spec.
const skillBodyWithYAMLFrontmatter = `---
tools:
  disallow:
    - shell: "Bash(curl:*)"
    - shell: "Bash(wget:*)"
---

# Network-Restricted Skill

This skill disallows outbound network fetch tools.
`

// SKILL.md with TOML-style frontmatter between --- delimiters.
// parseFrontmatterDisallows tries TOML first, then falls back to YAML line
// scanning; TOML-formatted frontmatter is less common but valid.
const skillBodyWithTOMLFrontmatter = `---
[tools]
[[tools.disallow]]
shell = "Bash(npm publish *)"
---

# Publish Guard Skill

This skill guards against accidental npm publishes.
`

func TestLoadSkills_Empty(t *testing.T) {
	t.Parallel()
	result, err := kit.LoadSkills(nil)
	if err != nil {
		t.Fatalf("LoadSkills(nil): unexpected error: %v", err)
	}
	if result.SystemAppend != "" {
		t.Errorf("SystemAppend: want empty, got %q", result.SystemAppend)
	}
	if len(result.DisallowedTools) != 0 {
		t.Errorf("DisallowedTools: want empty, got %v", result.DisallowedTools)
	}
}

func TestLoadSkills_SingleKit_NoFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/spring-test-debugging/SKILL.md", skillBodyPlain)

	sources := []kit.KitSkillSource{
		{
			ID:           "spring/java",
			Priority:     80,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/spring-test-debugging/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	if !strings.Contains(result.SystemAppend, "Spring Test Debugging") {
		t.Errorf("SystemAppend: want Spring Test Debugging body, got %q", result.SystemAppend)
	}
	if len(result.DisallowedTools) != 0 {
		t.Errorf("DisallowedTools: want empty, got %v", result.DisallowedTools)
	}
}

func TestLoadSkills_YAMLFrontmatterDisallows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/net-restricted/SKILL.md", skillBodyWithYAMLFrontmatter)

	sources := []kit.KitSkillSource{
		{
			ID:           "net-kit/v1",
			Priority:     50,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/net-restricted/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	if !strings.Contains(result.SystemAppend, "Network-Restricted Skill") {
		t.Errorf("SystemAppend: want body without frontmatter, got %q", result.SystemAppend)
	}
	// Frontmatter YAML keys must not appear verbatim in body (the "---" delimiter block).
	if strings.Contains(result.SystemAppend, "---") {
		t.Errorf("SystemAppend: YAML delimiter leaked into body: %q", result.SystemAppend)
	}
	wantDisallows := []string{"Bash(curl:*)", "Bash(wget:*)"}
	if len(result.DisallowedTools) != len(wantDisallows) {
		t.Fatalf("DisallowedTools: want %v, got %v", wantDisallows, result.DisallowedTools)
	}
	for i, want := range wantDisallows {
		if result.DisallowedTools[i] != want {
			t.Errorf("DisallowedTools[%d]: want %q, got %q", i, want, result.DisallowedTools[i])
		}
	}
}

func TestLoadSkills_TOMLFrontmatterDisallows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/publish-guard/SKILL.md", skillBodyWithTOMLFrontmatter)

	sources := []kit.KitSkillSource{
		{
			ID:           "publish-kit/v1",
			Priority:     60,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/publish-guard/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	if len(result.DisallowedTools) != 1 || result.DisallowedTools[0] != "Bash(npm publish *)" {
		t.Errorf("DisallowedTools: want [Bash(npm publish *)], got %v", result.DisallowedTools)
	}
}

func TestLoadSkills_PriorityOrdering(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/high/SKILL.md", "# High Priority Skill\nhigh-content")
	writeSkillFile(t, dir, "skills/low/SKILL.md", "# Low Priority Skill\nlow-content")

	// Low priority kit first in the slice — ordering must be by priority, not slice order.
	sources := []kit.KitSkillSource{
		{
			ID:           "low-kit",
			Priority:     10,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/low/SKILL.md"},
		},
		{
			ID:           "high-kit",
			Priority:     90,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/high/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	highIdx := strings.Index(result.SystemAppend, "high-content")
	lowIdx := strings.Index(result.SystemAppend, "low-content")
	if highIdx < 0 || lowIdx < 0 {
		t.Fatalf("SystemAppend missing expected content: %q", result.SystemAppend)
	}
	if highIdx > lowIdx {
		t.Errorf("priority order wrong: high-content should appear before low-content\n%s", result.SystemAppend)
	}
}

func TestLoadSkills_UnreadableFileSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/good/SKILL.md", "# Good Skill\ngood-content")
	// "bad/SKILL.md" intentionally not written.

	sources := []kit.KitSkillSource{
		{
			ID:           "mixed-kit",
			Priority:     50,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/bad/SKILL.md", "skills/good/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	// A non-nil error is expected (unreadable file) but must not prevent
	// the good skill from being loaded.
	if err == nil {
		t.Fatal("LoadSkills: want error for unreadable file, got nil")
	}
	if !strings.Contains(result.SystemAppend, "Good Skill") {
		t.Errorf("SystemAppend: good skill missing after bad-file skip: %q", result.SystemAppend)
	}
}

func TestLoadSkills_DeduplicatesDisallowedTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Both skill files disallow the same tool.
	skillA := "---\ntools:\n  disallow:\n    - shell: \"Bash(rm:*)\"\n---\n# A"
	skillB := "---\ntools:\n  disallow:\n    - shell: \"Bash(rm:*)\"\n---\n# B"
	writeSkillFile(t, dir, "skills/a/SKILL.md", skillA)
	writeSkillFile(t, dir, "skills/b/SKILL.md", skillB)

	sources := []kit.KitSkillSource{
		{
			ID:           "dup-kit",
			Priority:     50,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/a/SKILL.md", "skills/b/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	if len(result.DisallowedTools) != 1 {
		t.Errorf("DisallowedTools: want 1 deduplicated entry, got %v", result.DisallowedTools)
	}
}

func TestLoadSkills_MultipleSkillsPerKit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkillFile(t, dir, "skills/first/SKILL.md", "# First\nfirst-body")
	writeSkillFile(t, dir, "skills/second/SKILL.md", "# Second\nsecond-body")

	sources := []kit.KitSkillSource{
		{
			ID:           "multi-skill-kit",
			Priority:     80,
			ManifestPath: fakeManifestPath(dir),
			SkillFiles:   []string{"skills/first/SKILL.md", "skills/second/SKILL.md"},
		},
	}
	result, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: unexpected error: %v", err)
	}
	if !strings.Contains(result.SystemAppend, "first-body") {
		t.Errorf("SystemAppend: missing first body: %q", result.SystemAppend)
	}
	if !strings.Contains(result.SystemAppend, "second-body") {
		t.Errorf("SystemAppend: missing second body: %q", result.SystemAppend)
	}
}
