package prompt_test

import (
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/prompt"
)

// TestBuilderBuild_StagePromptMode covers the REN-1485 / REN-1487
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
				IssueIdentifier: "REN-1487",
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
				IssueIdentifier: "REN-1234",
				WorkType:        string(prompt.WorkTypeDevelopment),
				PromptContext:   "<issue identifier=\"REN-1234\"><title>Legacy</title></issue>",
			},
			expectMode: "legacy",
			expectInUser: []string{
				// Legacy path produces template-rendered text;
				// just assert the issue identifier surfaces.
				"REN-1234",
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
				IssueIdentifier: "REN-1488",
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
		IssueIdentifier: "REN-1487",
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
