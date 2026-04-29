package governor

import (
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	allEnabled := Config{
		AutoResearch:        true,
		AutoBacklogCreation: true,
		AutoDevelopment:     true,
		AutoQA:              true,
		AutoAcceptance:      true,
	}

	issueWithProject := func(state string) linear.Issue {
		iss := linear.Issue{Identifier: "TEST-1"}
		iss.State.Name = state
		iss.Project.Name = "SomeProject"
		return iss
	}
	issueWithoutProject := func(state string) linear.Issue {
		iss := linear.Issue{Identifier: "TEST-2"}
		iss.State.Name = state
		return iss
	}

	tests := []struct {
		name           string
		issue          linear.Issue
		cfg            Config
		wantDecision   DispatchDecision
		wantReasonHint string // substring expected in reason
	}{
		// ── Triage → research ──────────────────────────────────────────────
		{
			name:           "triage/enabled dispatch",
			issue:          issueWithProject("Triage"),
			cfg:            allEnabled,
			wantDecision:   DecisionDispatch,
			wantReasonHint: "research",
		},
		{
			name:           "triage/disabled skip",
			issue:          issueWithProject("Triage"),
			cfg:            Config{AutoResearch: false},
			wantDecision:   DecisionSkip,
			wantReasonHint: "auto-research disabled",
		},

		// ── Backlog with project → development ────────────────────────────
		{
			name:           "backlog+project/enabled dispatch",
			issue:          issueWithProject("Backlog"),
			cfg:            allEnabled,
			wantDecision:   DecisionDispatch,
			wantReasonHint: "development",
		},
		{
			name:           "backlog+project/disabled skip",
			issue:          issueWithProject("Backlog"),
			cfg:            Config{AutoDevelopment: false},
			wantDecision:   DecisionSkip,
			wantReasonHint: "auto-development disabled",
		},

		// ── Backlog without project → backlog-creation ────────────────────
		{
			name:           "backlog-no-project/enabled dispatch",
			issue:          issueWithoutProject("Backlog"),
			cfg:            allEnabled,
			wantDecision:   DecisionDispatch,
			wantReasonHint: "backlog-creation",
		},
		{
			name:           "backlog-no-project/disabled skip",
			issue:          issueWithoutProject("Backlog"),
			cfg:            Config{AutoBacklogCreation: false},
			wantDecision:   DecisionSkip,
			wantReasonHint: "auto-backlog-creation disabled",
		},

		// ── Started → qa ──────────────────────────────────────────────────
		{
			name:           "started/enabled dispatch",
			issue:          issueWithProject("Started"),
			cfg:            allEnabled,
			wantDecision:   DecisionDispatch,
			wantReasonHint: "qa",
		},
		{
			name:           "started/disabled skip",
			issue:          issueWithProject("Started"),
			cfg:            Config{AutoQA: false},
			wantDecision:   DecisionSkip,
			wantReasonHint: "auto-qa disabled",
		},

		// ── In Review → acceptance ────────────────────────────────────────
		{
			name:           "in-review/enabled dispatch",
			issue:          issueWithProject("In Review"),
			cfg:            allEnabled,
			wantDecision:   DecisionDispatch,
			wantReasonHint: "acceptance",
		},
		{
			name:           "in-review/disabled skip",
			issue:          issueWithProject("In Review"),
			cfg:            Config{AutoAcceptance: false},
			wantDecision:   DecisionSkip,
			wantReasonHint: "auto-acceptance disabled",
		},

		// ── Unknown state ─────────────────────────────────────────────────
		{
			name:           "unknown state skip",
			issue:          issueWithProject("Done"),
			cfg:            allEnabled,
			wantDecision:   DecisionSkip,
			wantReasonHint: "no mapped phase",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := Decide(tc.issue, tc.cfg)
			if got != tc.wantDecision {
				t.Errorf("Decide(%q) decision = %v, want %v (reason %q)",
					tc.issue.State.Name, got, tc.wantDecision, reason)
			}
			if !strings.Contains(reason, tc.wantReasonHint) {
				t.Errorf("Decide(%q) reason = %q, want substring %q",
					tc.issue.State.Name, reason, tc.wantReasonHint)
			}
		})
	}
}
