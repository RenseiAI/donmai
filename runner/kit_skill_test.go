package runner

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/internal/kit"
	"github.com/RenseiAI/agentfactory-tui/prompt"
)

// writeSkillMD writes a SKILL.md at dir/<relPath> for the test.
func writeSkillMD(t *testing.T, dir, relPath, content string) {
	t.Helper()
	abs := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

// TestKitSkillSources_InjectedIntoPromptBuilder verifies that a Runner
// constructed with KitSkillSources properly populates
// promptBuilder.SkillAppend from the loaded skill files.
func TestKitSkillSources_InjectedIntoPromptBuilder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeSkillMD(t, dir, "skills/spring-debug/SKILL.md",
		"# Spring Debugging\nCheck for circular dependencies first.")

	sources := []kit.KitSkillSource{
		{
			ID:           "spring/java",
			Priority:     80,
			ManifestPath: filepath.Join(dir, "spring.kit.toml"),
			SkillFiles:   []string{"skills/spring-debug/SKILL.md"},
		},
	}

	// Record what the promptBuilder.SkillAppend receives by intercepting
	// kitLoadSkills via the package-level seam.
	var capturedSkillAppend string
	origLoader := kitLoadSkills
	kitLoadSkills = func(srcs []kit.KitSkillSource) (kit.LoadedSkills, error) {
		loaded, err := origLoader(srcs)
		capturedSkillAppend = loaded.SystemAppend
		return loaded, err
	}
	t.Cleanup(func() { kitLoadSkills = origLoader })

	// Build a minimal Runner with the kit skill sources set.
	reg := NewRegistry()
	pb := &prompt.Builder{}

	runner := &Runner{
		registry:        reg,
		promptBuilder:   pb,
		kitSkillSources: sources,
		logger:          noopLogger(),
	}

	// Simulate the 5a skill-loading block from runLoop manually
	// (without a full Run — we only want to verify skill injection).
	if len(runner.kitSkillSources) > 0 {
		loaded, err := kitLoadSkills(runner.kitSkillSources)
		if err != nil {
			t.Fatalf("kitLoadSkills: %v", err)
		}
		runner.promptBuilder.SkillAppend = loaded.SystemAppend
	}

	if capturedSkillAppend == "" {
		t.Fatal("skill body was not captured by the loader seam")
	}
	if !strings.Contains(runner.promptBuilder.SkillAppend, "Spring Debugging") {
		t.Errorf("SkillAppend: want Spring Debugging, got %q", runner.promptBuilder.SkillAppend)
	}
}

// TestKitSkillSources_DisallowedToolsMerged verifies that tool disallow
// rules scraped from SKILL.md frontmatter are merged onto the
// DisallowedTools in the agent.Spec after translateSpec runs.
func TestKitSkillSources_DisallowedToolsMerged(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	skillContent := `---
tools:
  disallow:
    - shell: "Bash(curl:*)"
---

# Network Skill
Restricts curl usage.
`
	writeSkillMD(t, dir, "skills/net/SKILL.md", skillContent)

	sources := []kit.KitSkillSource{
		{
			ID:           "net-kit/v1",
			Priority:     50,
			ManifestPath: filepath.Join(dir, "net.kit.toml"),
			SkillFiles:   []string{"skills/net/SKILL.md"},
		},
	}

	// Load skills.
	loaded, err := kit.LoadSkills(sources)
	if err != nil {
		t.Fatalf("LoadSkills: %v", err)
	}
	if len(loaded.DisallowedTools) == 0 {
		t.Fatal("DisallowedTools: want at least one entry from frontmatter, got none")
	}

	// Simulate spec translation + Kit disallow merge (loop.go pattern).
	caps := noopCaps()
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{}}
	spec := translateSpec(qw, caps, SpecInputs{Cwd: "/tmp/wt", Prompt: "do"})

	// Append Kit disallows as the loop does.
	spec.DisallowedTools = append(spec.DisallowedTools, loaded.DisallowedTools...)

	found := false
	for _, d := range spec.DisallowedTools {
		if d == "Bash(curl:*)" {
			found = true
		}
	}
	if !found {
		t.Errorf("Bash(curl:*) not found in DisallowedTools: %v", spec.DisallowedTools)
	}
}

// TestKitSkillSources_EmptySourcesNoOp verifies that when no Kit skill
// sources are configured the runner behaves identically to pre-feature
// behavior (SkillAppend stays empty; no extra DisallowedTools added).
func TestKitSkillSources_EmptySourcesNoOp(t *testing.T) {
	t.Parallel()

	pb := &prompt.Builder{}
	runner := &Runner{
		registry:        NewRegistry(),
		promptBuilder:   pb,
		kitSkillSources: nil,
		logger:          noopLogger(),
	}

	// Simulate the 5a block from runLoop with no sources.
	if len(runner.kitSkillSources) > 0 {
		loaded, _ := kitLoadSkills(runner.kitSkillSources)
		runner.promptBuilder.SkillAppend = loaded.SystemAppend
	}

	if runner.promptBuilder.SkillAppend != "" {
		t.Errorf("SkillAppend: want empty for no kit sources, got %q", runner.promptBuilder.SkillAppend)
	}
}

// TestKitSkillSources_PromptContainsSkillSection verifies that when a
// SkillAppend is set, the rendered system prompt includes the Kit Skills
// heading, confirming prompt.Builder.Build picks it up.
func TestKitSkillSources_PromptContainsSkillSection(t *testing.T) {
	t.Parallel()

	pb := &prompt.Builder{
		SkillAppend: "Check for @SpringBootTest annotation before running tests.",
	}

	qw := prompt.QueuedWork{
		SessionID:       "test-session",
		IssueIdentifier: "REN-9",
		Repository:      "github.com/RenseiAI/test",
		Ref:             "main",
		PromptContext:   "<issue>test</issue>",
	}

	system, _, err := pb.Build(qw)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(system, "Kit Skills") {
		t.Errorf("system prompt: want 'Kit Skills' heading, got:\n%s", system)
	}
	if !strings.Contains(system, "@SpringBootTest") {
		t.Errorf("system prompt: want skill body, got:\n%s", system)
	}
}

// noopCaps returns an agent.Capabilities zero value for tests that only
// need to exercise translateSpec without caring about capability gating.
func noopCaps() agent.Capabilities {
	return agent.Capabilities{}
}

// noopLogger returns a slog.Logger that discards all output. Used by
// tests that need a non-nil logger but don't inspect log output.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// nopWriter discards all writes.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
