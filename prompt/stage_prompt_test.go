package prompt_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/prompt"
)

// TestBuilderBuild_StagePromptMode covers the
// Phase 2 dispatch path: when QueuedWork.StagePrompt is non-empty the
// builder uses it verbatim (with a stage-context preamble) instead of
// rendering the embedded user template. Cardinal rule 1 (legacy
// prompt path stays working) is asserted by the legacy_fallback case.
func TestBuilderBuild_StagePromptMode(t *testing.T) {
	t.Parallel()
	tcs := []struct {
		name         string
		work         prompt.QueuedWork
		expectMode   string // "stage" | "legacy"
		expectInUser []string
		excludeUser  []string
	}{
		{
			name: "stage_prompt_only",
			work: prompt.QueuedWork{
				SessionID:       "sess-1",
				IssueIdentifier: "ENG-1487",
				StagePrompt:     "Run the development stage on the issue. Decompose if needed.",
				StageID:         "development",
				StageBudget: &prompt.StageBudget{
					MaxDurationSeconds: 14400,
					MaxSubAgents:       5,
					MaxTokens:          200_000,
				},
				StageSourceEventID: "evt-abc-123",
			},
			expectMode: "stage",
			expectInUser: []string{
				"Run the development stage on the issue. Decompose if needed.",
				"<stage>development</stage>",
				"maxDurationSeconds=\"14400\"",
				"maxSubAgents=\"5\"",
				"maxTokens=\"200000\"",
				"<stageSourceEventId>evt-abc-123</stageSourceEventId>",
			},
		},
		{
			name: "legacy_fallback_no_stage_prompt",
			work: prompt.QueuedWork{
				SessionID:       "sess-2",
				IssueIdentifier: "ENG-1234",
				WorkType:        string(prompt.WorkTypeDevelopment),
				PromptContext:   "<issue identifier=\"ENG-1234\"><title>Legacy</title></issue>",
			},
			expectMode: "legacy",
			expectInUser: []string{
				// Legacy path produces template-rendered text;
				// just assert the issue identifier surfaces.
				"ENG-1234",
			},
			excludeUser: []string{
				"<stage>",
				"<stageBudget",
			},
		},
		{
			name: "stage_prompt_no_budget",
			work: prompt.QueuedWork{
				SessionID:       "sess-3",
				IssueIdentifier: "ENG-1488",
				StagePrompt:     "Research the prior art and produce a memo.",
				StageID:         "research",
			},
			expectMode: "stage",
			expectInUser: []string{
				"Research the prior art and produce a memo.",
				"<stage>research</stage>",
			},
			excludeUser: []string{
				"<stageBudget",
			},
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := prompt.NewBuilder()
			system, user, err := b.Build(tc.work)
			if err != nil {
				t.Fatalf("Build returned err: %v", err)
			}
			if system == "" {
				t.Fatalf("expected non-empty system prompt")
			}
			for _, want := range tc.expectInUser {
				if !strings.Contains(user, want) {
					t.Errorf("user prompt missing expected substring %q\nuser=%q", want, user)
				}
			}
			for _, exclude := range tc.excludeUser {
				if strings.Contains(user, exclude) {
					t.Errorf("user prompt should not contain %q\nuser=%q", exclude, user)
				}
			}
		})
	}
}

// TestBuilderBuild_StagePromptEmptyWorkRejected asserts that a
// QueuedWork with NEITHER StagePrompt NOR legacy issue-context fields
// still fails with ErrEmptyWork — the stage-prompt addition does not
// loosen the empty-work guard.
func TestBuilderBuild_StagePromptEmptyWorkRejected(t *testing.T) {
	t.Parallel()
	b := prompt.NewBuilder()
	_, _, err := b.Build(prompt.QueuedWork{SessionID: "sess-empty"})
	if err == nil {
		t.Fatalf("expected ErrEmptyWork for empty work")
	}
}

// TestBuilderBuild_StagePromptOverridesIssueContext asserts the
// short-circuit: when StagePrompt is set and PromptContext is ALSO
// set, the user prompt comes from StagePrompt — the platform-rendered
// stage prompt wins because the dispatcher already incorporated the
// issue context into it.
func TestBuilderBuild_StagePromptOverridesIssueContext(t *testing.T) {
	t.Parallel()
	b := prompt.NewBuilder()
	work := prompt.QueuedWork{
		SessionID:       "sess-hybrid",
		IssueIdentifier: "ENG-1487",
		PromptContext:   "<issue><description>This should NOT appear</description></issue>",
		StagePrompt:     "Stage prompt body that wins.",
		StageID:         "qa",
		WorkType:        string(prompt.WorkTypeDevelopment),
	}
	_, user, err := b.Build(work)
	if err != nil {
		t.Fatalf("Build err: %v", err)
	}
	if !strings.Contains(user, "Stage prompt body that wins.") {
		t.Fatalf("expected stage prompt body, got: %q", user)
	}
	if strings.Contains(user, "This should NOT appear") {
		t.Fatalf("legacy PromptContext should be suppressed when StagePrompt is set: %q", user)
	}
}

// TestBuilderBuild_SystemPromptOverride covers the legacy upstream wire field.
// Its content is role intent: Build appends it after the immutable operating
// protocol rather than allowing it to replace that higher authority.
func TestBuilderBuild_SystemPromptOverride(t *testing.T) {
	t.Parallel()

	const overrideText = "You are the upstream-supplied override agent. Custom identity active."

	t.Run("legacy_override_appends_role_intent", func(t *testing.T) {
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:            "sess-override-1",
			IssueIdentifier:      "ENG-9001",
			StagePrompt:          "Implement the feature described in the issue.",
			StageID:              "development",
			SystemPromptOverride: overrideText,
		}
		system, user, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		// The override remains byte-identical after the immutable preamble.
		if !strings.HasSuffix(system, "\n\n"+overrideText) {
			t.Errorf("expected system to end with override %q, got %q", overrideText, system)
		}
		// User prompt must still contain the stage prompt body.
		if !strings.Contains(user, "Implement the feature described in the issue.") {
			t.Errorf("user prompt missing stage body: %q", user)
		}
		// The harness operating protocol must remain ahead of role intent.
		baseIndex := strings.Index(system, "agent operating without an interactive user")
		roleIndex := strings.Index(system, overrideText)
		if baseIndex < 0 || roleIndex <= baseIndex {
			t.Errorf("operating protocol must precede role intent: %q", system)
		}
	})

	t.Run("override_with_legacy_user_path", func(t *testing.T) {
		// Override also applies when StagePrompt is absent (legacy user-prompt path).
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:            "sess-override-2",
			IssueIdentifier:      "ENG-9002",
			WorkType:             string(prompt.WorkTypeDevelopment),
			PromptContext:        "<issue><title>Legacy dispatch</title></issue>",
			SystemPromptOverride: overrideText,
		}
		system, user, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		if !strings.HasSuffix(system, "\n\n"+overrideText) {
			t.Errorf("expected system to end with override %q, got %q", overrideText, system)
		}
		// User prompt comes from the legacy template and must contain the identifier.
		if !strings.Contains(user, "ENG-9002") {
			t.Errorf("user prompt missing issue identifier: %q", user)
		}
	})

	t.Run("empty_override_falls_back_to_system_base_tmpl", func(t *testing.T) {
		// When SystemPromptOverride is empty the runner uses system_base.tmpl.
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:            "sess-override-3",
			IssueIdentifier:      "ENG-9003",
			StagePrompt:          "Do the thing.",
			StageID:              "qa",
			SystemPromptOverride: "", // explicitly empty
		}
		system, _, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		// Baseline system_base.tmpl must be present.
		if !strings.Contains(system, "autonomous") {
			t.Errorf("expected system_base.tmpl fallback, got: %q", system)
		}
		// Override text must not be present.
		if strings.Contains(system, overrideText) {
			t.Errorf("override should not appear when field is empty: %q", system)
		}
	})

	t.Run("whitespace_only_override_falls_back_to_system_base_tmpl", func(t *testing.T) {
		// Whitespace-only SystemPromptOverride is treated as absent.
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:            "sess-override-4",
			IssueIdentifier:      "ENG-9004",
			StagePrompt:          "Do the thing.",
			StageID:              "qa",
			SystemPromptOverride: "   \t\n  ",
		}
		system, _, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		if !strings.Contains(system, "autonomous") {
			t.Errorf("expected system_base.tmpl fallback for whitespace-only override, got: %q", system)
		}
	})
}

// TestBuilderBuild_MemoryBlock covers the Wave 3 dispatch-time agent-memory
// fold. When QueuedWork.MemoryBlock is non-empty Build APPENDS it under an
// "# Agent Memory" heading after the resolved system prompt — additive on
// every path (base template, override). Empty/whitespace is a no-op.
func TestBuilderBuild_MemoryBlock(t *testing.T) {
	t.Parallel()

	const memText = "recall: this repo pins gofumpt; never run plain gofmt"

	t.Run("appends_after_system_base_tmpl", func(t *testing.T) {
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:       "sess-mem-1",
			IssueIdentifier: "ENG-9101",
			WorkType:        string(prompt.WorkTypeDevelopment),
			PromptContext:   "<issue><title>Mem dispatch</title></issue>",
			MemoryBlock:     memText,
		}
		system, _, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		// Base template content must still be present (additive — not replaced).
		if !strings.Contains(system, "autonomous") {
			t.Errorf("system_base.tmpl content missing; memory fold must be additive: %q", system)
		}
		if !strings.Contains(system, "# Agent Memory") {
			t.Errorf("missing '# Agent Memory' heading: %q", system)
		}
		if !strings.Contains(system, memText) {
			t.Errorf("memory block text missing: %q", system)
		}
	})

	t.Run("appends_after_override", func(t *testing.T) {
		t.Parallel()
		b := prompt.NewBuilder()
		const overrideText = "You are the upstream override agent."
		work := prompt.QueuedWork{
			SessionID:            "sess-mem-2",
			IssueIdentifier:      "ENG-9102",
			StagePrompt:          "Do the thing.",
			StageID:              "development",
			SystemPromptOverride: overrideText,
			MemoryBlock:          memText,
		}
		system, _, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		if !strings.Contains(system, overrideText) {
			t.Errorf("override text missing; memory fold must be additive on override path: %q", system)
		}
		if !strings.Contains(system, memText) {
			t.Errorf("memory block text missing on override path: %q", system)
		}
		// Order: override first, memory appended after.
		if strings.Index(system, overrideText) > strings.Index(system, memText) {
			t.Errorf("memory block should be appended AFTER the override: %q", system)
		}
	})

	t.Run("empty_memory_is_noop", func(t *testing.T) {
		t.Parallel()
		b := prompt.NewBuilder()
		work := prompt.QueuedWork{
			SessionID:       "sess-mem-3",
			IssueIdentifier: "ENG-9103",
			StagePrompt:     "Do the thing.",
			StageID:         "qa",
			MemoryBlock:     "   \t\n ",
		}
		system, _, err := b.Build(work)
		if err != nil {
			t.Fatalf("Build returned err: %v", err)
		}
		if strings.Contains(system, "# Agent Memory") {
			t.Errorf("whitespace-only memory must be a no-op; got heading: %q", system)
		}
	})
}
