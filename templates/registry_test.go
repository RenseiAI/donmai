package templates_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/templates"
)

// TestNew_LoadsBuiltins verifies that New() successfully loads the embedded
// YAML files and registers at least the four canonical work-type templates.
func TestNew_LoadsBuiltins(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}

	want := []string{"system_base", "user_development", "user_qa", "user_research"}
	for _, name := range want {
		if !reg.Has(name) {
			t.Errorf("registry missing expected template %q; registered: %v", name, reg.Names())
		}
	}
}

// TestRender_SystemBase verifies basic variable interpolation in system_base.
func TestRender_SystemBase(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := map[string]interface{}{
		"sessionID":      "ses-abc123",
		"organizationID": "org-xyz",
		"projectName":    "MyProject",
		"repository":     "github.com/example/repo",
		"ref":            "main",
	}
	out, err := reg.Render("system_base", ctx)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	checkContains(t, out, "ses-abc123", "sessionID not interpolated")
	checkContains(t, out, "org-xyz", "organizationID not interpolated")
	checkContains(t, out, "MyProject", "projectName not interpolated")
	checkContains(t, out, "github.com/example/repo", "repository not interpolated")
	checkContains(t, out, "main", "ref not interpolated")
}

// TestRender_SystemBase_Defaults verifies that missing optional fields fall
// back to the "<unassigned>" / "<unspecified>" defaults.
func TestRender_SystemBase_Defaults(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	out, err := reg.Render("system_base", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	checkContains(t, out, "<unassigned>", "sessionID default missing")
	checkContains(t, out, "<unspecified>", "organizationID/projectName default missing")
	checkContains(t, out, "<n/a>", "repository default missing")
	checkContains(t, out, "<default>", "ref default missing")
}

// TestRender_SystemBase_AppendBlocks verifies conditional append/skillAppend
// sections appear only when the context fields are non-empty.
func TestRender_SystemBase_AppendBlocks(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Without append blocks: headings must be absent.
	outNoAppend, err := reg.Render("system_base", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Render (no append) error: %v", err)
	}
	if strings.Contains(outNoAppend, "# Repository-specific instructions") {
		t.Error("unexpected '# Repository-specific instructions' section when append is empty")
	}
	if strings.Contains(outNoAppend, "# Kit Skills") {
		t.Error("unexpected '# Kit Skills' section when skillAppend is empty")
	}

	// With append blocks: headings must be present.
	outWithAppend, err := reg.Render("system_base", map[string]interface{}{
		"append":      "Use pnpm, not npm.",
		"skillAppend": "Skill body here.",
	})
	if err != nil {
		t.Fatalf("Render (with append) error: %v", err)
	}
	checkContains(t, outWithAppend, "# Repository-specific instructions", "append heading missing")
	checkContains(t, outWithAppend, "Use pnpm, not npm.", "append content missing")
	checkContains(t, outWithAppend, "# Kit Skills", "skillAppend heading missing")
	checkContains(t, outWithAppend, "Skill body here.", "skillAppend content missing")
}

// TestRender_UserDevelopment verifies the development user prompt renders
// issue identifier, context, and optional parent/mention context correctly.
func TestRender_UserDevelopment(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := map[string]interface{}{
		"issueIdentifier": "REN-1234",
		"context":         "Implement feature X.",
		"parentContext":   "Parent epic context.",
		"mentionContext":  "User mentioned feature Y.",
		"ref":             "develop",
		"repository":      "github.com/example/platform",
	}
	out, err := reg.Render("user_development", ctx)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	checkContains(t, out, "REN-1234", "issueIdentifier missing")
	checkContains(t, out, "Implement feature X.", "context missing")
	checkContains(t, out, "# Parent issue context", "parentContext section missing")
	checkContains(t, out, "Parent epic context.", "parentContext content missing")
	checkContains(t, out, "# Mention context", "mentionContext section missing")
	checkContains(t, out, "User mentioned feature Y.", "mentionContext content missing")
	checkContains(t, out, "develop", "ref missing")
	checkContains(t, out, "github.com/example/platform", "repository missing")
}

// TestRender_UserDevelopment_OptionalSections verifies that parent/mention
// context sections are absent when the corresponding context fields are empty.
func TestRender_UserDevelopment_OptionalSections(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	out, err := reg.Render("user_development", map[string]interface{}{
		"issueIdentifier": "REN-0001",
		"context":         "Do something.",
	})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	if strings.Contains(out, "# Parent issue context") {
		t.Error("unexpected '# Parent issue context' section when parentContext is empty")
	}
	if strings.Contains(out, "# Mention context") {
		t.Error("unexpected '# Mention context' section when mentionContext is empty")
	}
}

// TestRender_UserQA verifies the QA user prompt renders correctly.
func TestRender_UserQA(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := map[string]interface{}{
		"issueIdentifier": "REN-5678",
		"context":         "Validate PR #42.",
	}
	out, err := reg.Render("user_qa", ctx)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	checkContains(t, out, "REN-5678", "issueIdentifier missing")
	checkContains(t, out, "Validate PR #42.", "context missing")
	checkContains(t, out, "WORK_RESULT:passed", "pass marker missing")
	checkContains(t, out, "WORK_RESULT:failed", "fail marker missing")
}

// TestRender_UserResearch verifies the research user prompt renders correctly.
func TestRender_UserResearch(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx := map[string]interface{}{
		"issueIdentifier": "REN-9999",
		"context":         "Explore options for caching.",
	}
	out, err := reg.Render("user_research", ctx)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	checkContains(t, out, "REN-9999", "issueIdentifier missing")
	checkContains(t, out, "Explore options for caching.", "context missing")
	checkContains(t, out, "Do NOT implement code", "research constraint missing")
}

// TestRender_NotFound verifies that Render returns ErrTemplateNotFound for
// an unregistered name.
func TestRender_NotFound(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	_, err = reg.Render("does-not-exist", map[string]interface{}{})
	if err == nil {
		t.Fatal("Render returned nil error; want ErrTemplateNotFound")
	}
	if !errors.Is(err, templates.ErrTemplateNotFound) {
		t.Errorf("error does not wrap ErrTemplateNotFound: %v", err)
	}
}

// TestHelpers_Eq verifies that the eq helper works via a template that uses it.
func TestHelpers_Eq(t *testing.T) {
	t.Parallel()
	// Use a dedicated helpers/ subdirectory so missing_kind.yaml in
	// testdata/ does not cause NewFromFS to fail validation.
	reg, err := templates.NewFromFS(os.DirFS("testdata/helpers"), ".")
	if err != nil {
		t.Fatalf("NewFromFS error: %v", err)
	}

	outMatch, err := reg.Render("helpers-eq-test", map[string]interface{}{
		"packageManager": "pnpm",
	})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	checkContains(t, outMatch, "using pnpm", "eq helper did not match")

	outNoMatch, err := reg.Render("helpers-eq-test", map[string]interface{}{
		"packageManager": "npm",
	})
	if err != nil {
		t.Fatalf("Render error (no-match): %v", err)
	}
	if strings.Contains(outNoMatch, "using pnpm") {
		t.Error("eq helper matched when it should not have")
	}
}

// TestHelpers_Neq verifies that the neq helper works correctly.
func TestHelpers_Neq(t *testing.T) {
	t.Parallel()
	reg, err := templates.NewFromFS(os.DirFS("testdata/helpers"), ".")
	if err != nil {
		t.Fatalf("NewFromFS error: %v", err)
	}

	outDiff, err := reg.Render("helpers-neq-test", map[string]interface{}{
		"packageManager": "npm",
	})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	checkContains(t, outDiff, "not pnpm", "neq helper did not match")

	outSame, err := reg.Render("helpers-neq-test", map[string]interface{}{
		"packageManager": "pnpm",
	})
	if err != nil {
		t.Fatalf("Render error (same): %v", err)
	}
	if strings.Contains(outSame, "not pnpm") {
		t.Error("neq helper matched when values are equal")
	}
}

// TestHelpers_HasFlag verifies that the hasFlag helper works correctly.
func TestHelpers_HasFlag(t *testing.T) {
	t.Parallel()
	reg, err := templates.NewFromFS(os.DirFS("testdata/helpers"), ".")
	if err != nil {
		t.Fatalf("NewFromFS error: %v", err)
	}

	outHas, err := reg.Render("helpers-hasflag-test", map[string]interface{}{
		"flags": "feature-a feature-b debug",
	})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	checkContains(t, outHas, "debug enabled", "hasFlag helper did not match 'debug'")

	outMissing, err := reg.Render("helpers-hasflag-test", map[string]interface{}{
		"flags": "feature-a feature-b",
	})
	if err != nil {
		t.Fatalf("Render error (missing): %v", err)
	}
	if strings.Contains(outMissing, "debug enabled") {
		t.Error("hasFlag incorrectly matched 'debug' when flag is absent")
	}
}

// checkContains is a test helper that fails the test when s does not contain sub.
func checkContains(t *testing.T, s, sub, msg string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: output does not contain %q\nfull output:\n%s", msg, sub, s)
	}
}
