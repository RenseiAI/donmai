package templates_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/templates"
)

// testdata returns the absolute path to the testdata directory next to
// this file, regardless of where `go test` is invoked from.
func testdata(name string) string {
	return filepath.Join("testdata", name)
}

// TestLoad_WorkflowTemplate verifies that a well-formed WorkflowTemplate
// YAML round-trips through Load with correct field values.
func TestLoad_WorkflowTemplate(t *testing.T) {
	t.Parallel()
	tmpl, err := templates.Load(testdata("valid_workflow.yaml"))
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if tmpl.Kind != templates.KindWorkflowTemplate {
		t.Errorf("Kind = %q, want %q", tmpl.Kind, templates.KindWorkflowTemplate)
	}
	if tmpl.Metadata.Name != "development" {
		t.Errorf("Metadata.Name = %q, want %q", tmpl.Metadata.Name, "development")
	}
	if tmpl.Metadata.WorkType != "development" {
		t.Errorf("Metadata.WorkType = %q, want %q", tmpl.Metadata.WorkType, "development")
	}
	if tmpl.Prompt == "" {
		t.Error("Prompt is empty; want non-empty template body")
	}
	if tmpl.Tools == nil {
		t.Fatal("Tools is nil; want non-nil tools block")
	}
	if len(tmpl.Tools.Allow) != 2 {
		t.Errorf("len(Tools.Allow) = %d, want 2", len(tmpl.Tools.Allow))
	}
	if len(tmpl.Tools.Disallow) != 1 {
		t.Errorf("len(Tools.Disallow) = %d, want 1", len(tmpl.Tools.Disallow))
	}
	// First allow entry is a shell permission.
	if tmpl.Tools.Allow[0].Shell != "pnpm *" {
		t.Errorf("Tools.Allow[0].Shell = %q, want %q", tmpl.Tools.Allow[0].Shell, "pnpm *")
	}
	// Disallow entry is a plain string ("user-input").
	if tmpl.Tools.Disallow[0].Raw != "user-input" {
		t.Errorf("Tools.Disallow[0].Raw = %q, want %q", tmpl.Tools.Disallow[0].Raw, "user-input")
	}
}

// TestLoad_PartialTemplate verifies that a well-formed PartialTemplate
// YAML round-trips through Load with correct field values.
func TestLoad_PartialTemplate(t *testing.T) {
	t.Parallel()
	tmpl, err := templates.Load(testdata("valid_partial.yaml"))
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if tmpl.Kind != templates.KindPartialTemplate {
		t.Errorf("Kind = %q, want %q", tmpl.Kind, templates.KindPartialTemplate)
	}
	if tmpl.Metadata.Name != "cli-instructions" {
		t.Errorf("Metadata.Name = %q, want %q", tmpl.Metadata.Name, "cli-instructions")
	}
	if tmpl.Content == "" {
		t.Error("Content is empty; want non-empty partial body")
	}
	// Partial templates have no Prompt field.
	if tmpl.Prompt != "" {
		t.Errorf("Prompt = %q, want empty for PartialTemplate", tmpl.Prompt)
	}
}

// TestLoad_MissingKind verifies that a YAML file without a kind field
// returns a descriptive validation error.
func TestLoad_MissingKind(t *testing.T) {
	t.Parallel()
	_, err := templates.Load(testdata("missing_kind.yaml"))
	if err == nil {
		t.Fatal("Load returned nil error; want validation error for missing kind")
	}
}

// TestLoad_FileNotFound verifies that Load wraps os.ErrNotExist when the
// path does not exist, allowing callers to use errors.Is for detection.
func TestLoad_FileNotFound(t *testing.T) {
	t.Parallel()
	_, err := templates.Load(testdata("does_not_exist.yaml"))
	if err == nil {
		t.Fatal("Load returned nil error; want error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error does not wrap os.ErrNotExist: %v", err)
	}
}

// TestLoad_StrategyCompoundKey verifies that a WorkflowTemplate whose
// metadata.name differs from metadata.workType (the strategy compound-key
// pattern) loads cleanly and exposes both fields.
func TestLoad_StrategyCompoundKey(t *testing.T) {
	t.Parallel()
	tmpl, err := templates.Load(testdata("strategy_compound.yaml"))
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	// name != workType — this is the strategy compound-key pattern.
	if tmpl.Metadata.Name == tmpl.Metadata.WorkType {
		t.Errorf("Name == WorkType (%q); expected them to differ for a strategy template",
			tmpl.Metadata.Name)
	}
	if tmpl.Metadata.Name != "refinement-context-enriched" {
		t.Errorf("Name = %q, want %q", tmpl.Metadata.Name, "refinement-context-enriched")
	}
	if tmpl.Metadata.WorkType != "refinement" {
		t.Errorf("WorkType = %q, want %q", tmpl.Metadata.WorkType, "refinement")
	}
}
