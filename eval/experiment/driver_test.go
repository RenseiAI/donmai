package experiment

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testArm(id ArmID, subject, prompt string) Arm {
	return Arm{ID: id, SubjectRef: subject, VariantRef: SHA256VariantRef(prompt), SystemPrompt: prompt}
}

func TestRunBalancedMatrixInjectsImmutableVariants(t *testing.T) {
	definition := Definition{
		ID: "injection-clause-v1",
		Arms: []Arm{
			testArm("incumbent", "agent/development", "incumbent prompt"),
			testArm("candidate", "agent/development", "candidate prompt"),
		},
	}
	cases := []Case{{ID: "case-a", Prompt: "complete the benign task"}, {ID: "case-b", Prompt: "inspect the repository"}}

	var got []string
	report, err := Run(context.Background(), definition, cases, 2, func(_ context.Context, trial Trial) (string, error) {
		got = append(got, fmt.Sprintf("%s/%d/%s/%s/%s", trial.CaseID, trial.TrialIndex, trial.Arm.ID, trial.Prompt.UserPrompt, trial.Prompt.SystemPrompt))
		return string(trial.Arm.ID), nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"case-a/1/incumbent/complete the benign task/incumbent prompt",
		"case-a/1/candidate/complete the benign task/candidate prompt",
		"case-a/2/incumbent/complete the benign task/incumbent prompt",
		"case-a/2/candidate/complete the benign task/candidate prompt",
		"case-b/1/incumbent/inspect the repository/incumbent prompt",
		"case-b/1/candidate/inspect the repository/candidate prompt",
		"case-b/2/incumbent/inspect the repository/incumbent prompt",
		"case-b/2/candidate/inspect the repository/candidate prompt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trial order/plans = %#v, want %#v", got, want)
	}
	if report.ExperimentID != definition.ID || report.TrialsPerArm != 2 || len(report.Outcomes) != len(want) {
		t.Fatalf("report = %+v", report)
	}
}

func TestRunAppliesSameDeterministicContextResetToEveryArm(t *testing.T) {
	definition := Definition{
		ID: "long-horizon-v1",
		Arms: []Arm{
			testArm("incumbent", "agent/base", "incumbent prompt"),
			testArm("candidate", "agent/base", "candidate prompt"),
		},
		Perturbations: []Perturbation{ContextResetAtTurn(4, "resume from durable state")},
	}

	var resets []*ContextReset
	_, err := Run(context.Background(), definition, []Case{{ID: "case-a", Prompt: "work"}}, 1, func(_ context.Context, trial Trial) (struct{}, error) {
		if trial.Prompt.ContextReset == nil {
			t.Fatal("context reset missing")
		}
		resets = append(resets, trial.Prompt.ContextReset)
		if !reflect.DeepEqual(trial.Prompt.Perturbations, []string{"context-reset"}) {
			t.Fatalf("perturbations = %v", trial.Prompt.Perturbations)
		}
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(resets) != 2 || *resets[0] != *resets[1] {
		t.Fatalf("reset plan differed across arms: %+v", resets)
	}
	if resets[0] == resets[1] {
		t.Fatal("arms share a mutable context-reset pointer")
	}
}

func TestDefinitionRejectsUnboundOrDuplicateVariantIdentity(t *testing.T) {
	tests := []struct {
		name string
		def  Definition
		want string
	}{
		{
			name: "mutable variant",
			def: Definition{ID: "prompt-v1", Arms: []Arm{
				{ID: "incumbent", SubjectRef: "agent/base", VariantRef: "main"},
				testArm("candidate", "agent/base", "candidate"),
			}},
			want: "does not match",
		},
		{
			name: "hash names different content",
			def: Definition{ID: "prompt-v1", Arms: []Arm{
				{ID: "incumbent", SubjectRef: "agent/base", VariantRef: SHA256VariantRef("other"), SystemPrompt: "incumbent"},
				testArm("candidate", "agent/base", "candidate"),
			}},
			want: "does not match",
		},
		{
			name: "duplicate arm",
			def: Definition{ID: "prompt-v1", Arms: []Arm{
				testArm("candidate", "agent/base", "first"),
				testArm("candidate", "agent/base", "second"),
			}},
			want: "duplicate arm",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.def.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

type failOnCasePerturbation struct{ caseID string }

func (p failOnCasePerturbation) Name() string { return "fail-on-case" }

func (p failOnCasePerturbation) Apply(caseID string, _ int, plan PromptPlan) (PromptPlan, error) {
	if caseID == p.caseID {
		return PromptPlan{}, fmt.Errorf("planned failure")
	}
	return plan, nil
}

func TestRunPlansCompleteMatrixBeforeExecuting(t *testing.T) {
	definition := Definition{
		ID: "preplan-v1",
		Arms: []Arm{
			testArm("incumbent", "agent/base", "incumbent"),
			testArm("candidate", "agent/base", "candidate"),
		},
		Perturbations: []Perturbation{failOnCasePerturbation{caseID: "case-b"}},
	}
	calls := 0
	_, err := Run(context.Background(), definition, []Case{{ID: "case-a", Prompt: "work"}, {ID: "case-b", Prompt: "work"}}, 1,
		func(context.Context, Trial) (struct{}, error) {
			calls++
			return struct{}{}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "planned failure") {
		t.Fatalf("error = %v, want planning failure", err)
	}
	if calls != 0 {
		t.Fatalf("callback ran %d times before complete matrix validation", calls)
	}
}

func TestLegacyConcreteConsumerMayOmitExperimentRefs(t *testing.T) {
	definition := Definition{Arms: []Arm{{ID: "without"}, {ID: "with"}}}
	if err := definition.Validate(); err != nil {
		t.Fatalf("legacy concrete definition should validate: %v", err)
	}
}
