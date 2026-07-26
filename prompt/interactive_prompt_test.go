package prompt_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/templates"
)

// fixtureInteractive mirrors the QueuedWork shape an upstream dispatcher
// emits for a Mode="interactive" PTY session: a synthetic, non-tracker-backed
// IssueIdentifier (a session-id prefix), workType "interactive", and no
// rendered issue context. The synthetic identifier is what let the batch
// template render at all — hasIssueContext() passes on IssueIdentifier
// alone.
func fixtureInteractive() prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:       "0b5e88d9-32d0-4aca-9f8c-caf82f2b399c",
		IssueIdentifier: "0b5e88d9-32d",
		ProjectName:     "smoke-alpha",
		OrganizationID:  "org_ejkmv9ojdyifipydw5l1",
		Repository:      "github.com/RenseiAI/rensei-smokes-alpha",
		Ref:             "main",
		WorkType:        "interactive",
		Mode:            prompt.InteractiveRunMode,
	}
}

// batchScaffoldingMarkers are strings that only appear in the batch
// work-type user templates (user_development.tmpl et al). An interactive
// session's first message must never contain any of them.
var batchScaffoldingMarkers = []string{
	"Start work on",
	"# What to do",
	"WORK_RESULT:",
	"gh pr create",
}

// TestBuilderBuild_InteractiveNeverRendersBatchTemplate is the regression
// test for the interactive first-prompt bug: a Mode="interactive" QueuedWork
// with no seed prompt used to fall through userTemplateName's unknown-
// work-type default and render user_development.tmpl against the synthetic
// issue identifier ("Start work on 0b5e88d9-32d… open a PR…"), which the
// in-session agent correctly self-reported as blocked. Interactive sessions
// must receive NO templated user prompt at all: the harness starts the
// PTY REPL idle (claude/codex interactiveArgs treat an empty Spec.Prompt as
// "no seeded message") and any seed text is delivered verbatim through the
// runner's PTY first-input write, not through the renderer.
func TestBuilderBuild_InteractiveNeverRendersBatchTemplate(t *testing.T) {
	t.Parallel()

	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New() error: %v", err)
	}

	builders := []struct {
		name string
		b    *prompt.Builder
	}{
		{"legacy text/template path", prompt.NewBuilder()},
		{"raymond registry path", &prompt.Builder{Registry: reg}},
	}
	for _, tc := range builders {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			system, user, err := tc.b.Build(fixtureInteractive())
			if err != nil {
				t.Fatalf("Build error: %v", err)
			}
			if user != "" {
				t.Errorf("interactive user prompt must be empty (harness starts idle); got %d bytes:\n%s",
					len(user), user)
			}
			for _, marker := range batchScaffoldingMarkers {
				if strings.Contains(user, marker) {
					t.Errorf("interactive user prompt contains batch scaffolding marker %q", marker)
				}
			}
			// The system prompt (identity + operating rules) still renders —
			// only the batch user scaffolding is suppressed.
			if system == "" {
				t.Error("interactive system prompt must still render")
			}
		})
	}
}

// TestBuilderBuild_InteractiveSeedStaysOutOfPrompts pins the InitialPrompt
// contract on the renderer side: the seed is opaque first-INPUT data
// delivered verbatim (plus one newline) into the live PTY by the runner's
// writeInitialPromptInput hop — the prompt renderer must not fold it into
// either prompt, and the user prompt stays empty so the seed is the
// session's only first message (no wrapping, no template).
func TestBuilderBuild_InteractiveSeedStaysOutOfPrompts(t *testing.T) {
	t.Parallel()
	const seed = "please take a look at the flaky auth test on this branch"
	qw := fixtureInteractive()
	qw.InitialPrompt = seed

	system, user, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if user != "" {
		t.Errorf("user prompt must stay empty (seed rides the PTY write, not the renderer); got:\n%s", user)
	}
	if strings.Contains(system, seed) {
		t.Error("system prompt must not embed the interactive seed prompt")
	}
}

// TestBuilderBuild_InteractiveNoIssueContextStartsIdle verifies an
// interactive dispatch that carries no issue context at all (no
// PromptContext, no Body, no IssueIdentifier — nothing synthetic) builds
// without ErrEmptyWork: an idle interactive session awaiting terminal input
// is valid work, unlike a batch dispatch with nothing to do.
func TestBuilderBuild_InteractiveNoIssueContextStartsIdle(t *testing.T) {
	t.Parallel()
	qw := prompt.QueuedWork{
		SessionID: "5f1c9a2e-77aa-4a2f-8f74-0d3c2b1a9e88",
		WorkType:  "interactive",
		Mode:      prompt.InteractiveRunMode,
	}
	system, user, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("Build must not require issue context for interactive mode: %v", err)
	}
	if user != "" {
		t.Errorf("user prompt must be empty; got:\n%s", user)
	}
	if system == "" {
		t.Error("system prompt must still render")
	}
}

// TestBuilderBuild_InteractiveWinsOverStagePrompt pins precedence: even if a
// misconfigured dispatcher stamps both Mode="interactive" and a StagePrompt,
// the interactive gate wins — the stage preamble is wrapping, and an
// interactive first message is never wrapped.
func TestBuilderBuild_InteractiveWinsOverStagePrompt(t *testing.T) {
	t.Parallel()
	qw := fixtureInteractive()
	qw.StagePrompt = "stage-rendered directive"
	qw.StageID = "development"

	_, user, err := prompt.NewBuilder().Build(qw)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if user != "" {
		t.Errorf("interactive mode must suppress the stage-prompt user body too; got:\n%s", user)
	}
}

// TestBuilderBuild_ModeGateIsExact proves the interactive gate fires only on
// the exact Mode literal: batch ("" mode) and interview dispatches keep
// rendering the work-type template byte-identically. This is the
// batch-unchanged guarantee (alongside the golden-snapshot tests).
func TestBuilderBuild_ModeGateIsExact(t *testing.T) {
	t.Parallel()

	baseline := fixtureSession()
	baseline.WorkType = string(prompt.WorkTypeDevelopment)
	wantSystem, wantUser, err := prompt.NewBuilder().Build(baseline)
	if err != nil {
		t.Fatalf("baseline Build error: %v", err)
	}
	if wantUser == "" {
		t.Fatal("baseline batch user prompt unexpectedly empty")
	}

	for _, mode := range []string{"", "interview", "INTERACTIVE", "interactive-v2"} {
		qw := fixtureSession()
		qw.WorkType = string(prompt.WorkTypeDevelopment)
		qw.Mode = mode
		system, user, err := prompt.NewBuilder().Build(qw)
		if err != nil {
			t.Fatalf("Build(mode=%q) error: %v", mode, err)
		}
		if system != wantSystem {
			t.Errorf("mode=%q system prompt diverged from baseline", mode)
		}
		if user != wantUser {
			t.Errorf("mode=%q user prompt diverged from baseline", mode)
		}
	}
}
