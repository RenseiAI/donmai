// Package snapshot_test verifies that the raymond-rendered YAML templates
// produce output that is semantically equivalent to the legacy text/template
// .tmpl files in prompt/templates/.
//
// "Equivalent" means the same structural sections appear, the same variable
// values are interpolated, and conditional blocks behave identically. We do
// not assert byte-identical output because the Handlebars syntax differs
// cosmetically from Go text/template syntax (e.g. {{or .X "def"}} vs
// {{#if x}}{{x}}{{else}}def{{/if}}).
package snapshot_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/prompt"
	"github.com/RenseiAI/agentfactory-tui/templates"
)

// fixtureWork mirrors the canonical session fixture used in prompt/builder_test.go
// so both test suites cover the same representative payload.
func fixtureWork() prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:       "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
		IssueIdentifier: "REN2-1",
		ProjectName:     "smoke-alpha",
		OrganizationID:  "org_ejkmv9ojdyifipydw5l1",
		Repository:      "github.com/RenseiAI/rensei-smokes-alpha",
		Ref:             "main",
		PromptContext: "<issue identifier=\"REN2-1\">\n" +
			"<title>Wave 6 smoke test</title>\n" +
			"<description>\nCreate hello-from-wave6.md\n</description>\n" +
			"</issue>",
	}
}

// TestSnapshot_SystemBase verifies that the raymond-rendered system_base
// contains all the structural elements that the legacy system_base.tmpl
// produces.
func TestSnapshot_SystemBase(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	qw := fixtureWork()
	ctx := map[string]interface{}{
		"sessionID":      qw.SessionID,
		"organizationID": qw.OrganizationID,
		"projectName":    qw.ProjectName,
		"repository":     qw.Repository,
		"ref":            qw.Ref,
	}

	raymond, err := reg.Render("system_base", ctx)
	if err != nil {
		t.Fatalf("raymond Render error: %v", err)
	}

	// Render via legacy text/template path (prompt.Builder with zero Registry).
	legacy, _, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	// Structural equivalence checks — both outputs must contain the same
	// mandatory sections.
	sections := []string{
		"You are an autonomous Rensei agent",
		"# Identity",
		"# Operating rules",
		qw.SessionID,
		qw.OrganizationID,
		qw.ProjectName,
		qw.Repository,
		qw.Ref,
	}
	for _, s := range sections {
		if !strings.Contains(raymond, s) {
			t.Errorf("raymond output missing %q", s)
		}
		if !strings.Contains(legacy, s) {
			t.Errorf("legacy output missing %q (test fixture issue)", s)
		}
	}

	// Neither output should contain the conditional sections when inputs
	// carry no append data.
	for _, forbidden := range []string{"# Repository-specific instructions", "# Kit Skills"} {
		if strings.Contains(raymond, forbidden) {
			t.Errorf("raymond output unexpectedly contains %q", forbidden)
		}
		if strings.Contains(legacy, forbidden) {
			t.Errorf("legacy output unexpectedly contains %q (test fixture issue)", forbidden)
		}
	}
}

// TestSnapshot_SystemBase_WithAppend verifies append-block equivalence.
func TestSnapshot_SystemBase_WithAppend(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	qw := fixtureWork()
	appendText := "Always run `make verify` before opening a PR."

	ctx := map[string]interface{}{
		"sessionID":      qw.SessionID,
		"organizationID": qw.OrganizationID,
		"projectName":    qw.ProjectName,
		"repository":     qw.Repository,
		"ref":            qw.Ref,
		"append":         appendText,
	}

	raymondOut, err := reg.Render("system_base", ctx)
	if err != nil {
		t.Fatalf("raymond Render error: %v", err)
	}

	b := &prompt.Builder{SystemAppend: appendText}
	legacyOut, _, err := b.Build(qw)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	for _, expected := range []string{"# Repository-specific instructions", appendText} {
		if !strings.Contains(raymondOut, expected) {
			t.Errorf("raymond output missing %q", expected)
		}
		if !strings.Contains(legacyOut, expected) {
			t.Errorf("legacy output missing %q (fixture issue)", expected)
		}
	}
}

// TestSnapshot_UserDevelopment verifies structural equivalence between
// raymond and legacy for the development user prompt.
func TestSnapshot_UserDevelopment(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	qw := fixtureWork()
	qw.WorkType = string(prompt.WorkTypeDevelopment)
	ctx := map[string]interface{}{
		"issueIdentifier": qw.IssueIdentifier,
		"context":         qw.PromptContext,
		"ref":             qw.Ref,
		"repository":      qw.Repository,
	}

	raymondOut, err := reg.Render("user_development", ctx)
	if err != nil {
		t.Fatalf("raymond Render error: %v", err)
	}

	_, legacyOut, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	mustContain := []string{
		"REN2-1",
		"# What to do",
		"WORK_RESULT:passed",
		"WORK_RESULT:failed",
		"git push",
		"gh pr create",
	}
	for _, s := range mustContain {
		if !strings.Contains(raymondOut, s) {
			t.Errorf("raymond output missing %q", s)
		}
		if !strings.Contains(legacyOut, s) {
			t.Errorf("legacy output missing %q (fixture issue)", s)
		}
	}
}

// TestSnapshot_UserQA verifies structural equivalence for the qa user prompt.
func TestSnapshot_UserQA(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	qw := fixtureWork()
	qw.WorkType = string(prompt.WorkTypeQA)
	ctx := map[string]interface{}{
		"issueIdentifier": qw.IssueIdentifier,
		"context":         qw.PromptContext,
	}

	raymondOut, err := reg.Render("user_qa", ctx)
	if err != nil {
		t.Fatalf("raymond Render error: %v", err)
	}

	_, legacyOut, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	mustContain := []string{
		"REN2-1",
		"acceptance criteria",
		"WORK_RESULT:passed",
		"WORK_RESULT:failed",
	}
	for _, s := range mustContain {
		if !strings.Contains(raymondOut, s) {
			t.Errorf("raymond output missing %q", s)
		}
		if !strings.Contains(legacyOut, s) {
			t.Errorf("legacy output missing %q (fixture issue)", s)
		}
	}
}

// TestSnapshot_UserResearch verifies structural equivalence for the research
// user prompt.
func TestSnapshot_UserResearch(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	qw := fixtureWork()
	qw.WorkType = string(prompt.WorkTypeResearch)
	ctx := map[string]interface{}{
		"issueIdentifier": qw.IssueIdentifier,
		"context":         qw.PromptContext,
	}

	raymondOut, err := reg.Render("user_research", ctx)
	if err != nil {
		t.Fatalf("raymond Render error: %v", err)
	}

	_, legacyOut, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("legacy Build error: %v", err)
	}

	mustContain := []string{
		"REN2-1",
		"Do NOT implement code",
		"acceptance criteria",
	}
	for _, s := range mustContain {
		if !strings.Contains(raymondOut, s) {
			t.Errorf("raymond output missing %q", s)
		}
		if !strings.Contains(legacyOut, s) {
			t.Errorf("legacy output missing %q (fixture issue)", s)
		}
	}
}

// TestSnapshot_ConditionalEquivalence checks that optional context sections
// appear/disappear consistently across both renderers.
func TestSnapshot_ConditionalEquivalence(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	// With parentContext populated — both renderers must include the heading.
	parentText := "Parent epic: REN-EPIC-1"
	qwParent := fixtureWork()
	qwParent.WorkType = string(prompt.WorkTypeDevelopment)
	qwParent.ParentContext = parentText

	ctxParent := map[string]interface{}{
		"issueIdentifier": qwParent.IssueIdentifier,
		"context":         qwParent.PromptContext,
		"parentContext":   parentText,
		"ref":             qwParent.Ref,
		"repository":      qwParent.Repository,
	}

	raymondWithParent, err := reg.Render("user_development", ctxParent)
	if err != nil {
		t.Fatalf("raymond Render (with parent) error: %v", err)
	}
	_, legacyWithParent, err := prompt.NewBuilder().Build(qwParent)
	if err != nil {
		t.Fatalf("legacy Build (with parent) error: %v", err)
	}

	for _, expected := range []string{"# Parent issue context", parentText} {
		if !strings.Contains(raymondWithParent, expected) {
			t.Errorf("raymond missing %q when parentContext is set", expected)
		}
		if !strings.Contains(legacyWithParent, expected) {
			t.Errorf("legacy missing %q when parentContext is set", expected)
		}
	}

	// Without parentContext — neither renderer should include the section.
	qwNoParent := fixtureWork()
	qwNoParent.WorkType = string(prompt.WorkTypeDevelopment)
	ctxNoParent := map[string]interface{}{
		"issueIdentifier": qwNoParent.IssueIdentifier,
		"context":         qwNoParent.PromptContext,
		"ref":             qwNoParent.Ref,
		"repository":      qwNoParent.Repository,
	}

	raymondNoParent, err := reg.Render("user_development", ctxNoParent)
	if err != nil {
		t.Fatalf("raymond Render (no parent) error: %v", err)
	}
	_, legacyNoParent, err := prompt.NewBuilder().Build(qwNoParent)
	if err != nil {
		t.Fatalf("legacy Build (no parent) error: %v", err)
	}

	if strings.Contains(raymondNoParent, "# Parent issue context") {
		t.Error("raymond unexpectedly includes '# Parent issue context' when parentContext is empty")
	}
	if strings.Contains(legacyNoParent, "# Parent issue context") {
		t.Error("legacy unexpectedly includes '# Parent issue context' when parentContext is empty")
	}
}
