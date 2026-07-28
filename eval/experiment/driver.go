// Package experiment provides provider-neutral prompt-experiment orchestration.
// It deliberately owns only experiment identity, balanced trial enumeration, and
// prompt perturbation planning. Concrete harness packages retain provisioning,
// execution, transcript capture, grading, and result-posting responsibilities.
package experiment

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	// SlugMaxLength matches the bounded experiment/arm slug accepted by the eval
	// receipt API.
	SlugMaxLength = 64
)

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// ArmID is a bounded opaque experiment-arm slug.
type ArmID string

// Arm defines one prompt variant. Prompt text remains process-local; durable
// receipts carry only SubjectRef and VariantRef.
type Arm struct {
	ID           ArmID  `json:"id"`
	SubjectRef   string `json:"subjectRef"`
	VariantRef   string `json:"variantRef"`
	SystemPrompt string `json:"-"`
}

// Case is the task input shared across every arm and repeated trial.
type Case struct {
	ID     string
	Prompt string
}

// Definition identifies an experiment and its balanced arm set. An empty ID is
// permitted for an existing concrete harness that only reuses matrix
// orchestration; a non-empty ID activates strict prompt-experiment validation.
type Definition struct {
	ID            string
	Arms          []Arm
	Perturbations []Perturbation
}

// PromptPlan is the exact prompt/perturbation plan handed to a concrete harness.
type PromptPlan struct {
	UserPrompt    string
	SystemPrompt  string `json:"-"`
	ContextReset  *ContextReset
	Perturbations []string
}

// ContextReset describes a deterministic mid-session context reset. Concrete
// executors must either implement this plan exactly or fail closed; silently
// ignoring it would invalidate an arm comparison.
type ContextReset struct {
	AfterTurn          int
	ContinuationPrompt string
}

// Trial is one case × arm × repeated-trial execution request.
type Trial struct {
	ExperimentID string
	CaseID       string
	Arm          Arm
	TrialIndex   int
	Prompt       PromptPlan
}

// Outcome pairs a concrete harness result with its immutable trial identity.
type Outcome[T any] struct {
	Trial  Trial
	Result T
}

// Report is the ordered set of completed balanced trials.
type Report[T any] struct {
	ExperimentID string
	TrialsPerArm int
	Outcomes     []Outcome[T]
}

// Perturbation deterministically transforms the shared task plan. It receives
// no arm identity or variant prompt. Variant-specific text belongs in
// Arm.SystemPrompt instead.
type Perturbation interface {
	Name() string
	Apply(caseID string, trialIndex int, plan PromptPlan) (PromptPlan, error)
}

// SHA256VariantRef returns the immutable identity for the exact process-local
// system-prompt bytes. Prompt experiments validate this binding before any
// trial executes, so a receipt cannot name content different from what ran.
func SHA256VariantRef(systemPrompt string) string {
	sum := sha256.Sum256([]byte(systemPrompt))
	return fmt.Sprintf("sha256:%x", sum)
}

// ContextResetAtTurn creates a deterministic context-reset perturbation.
func ContextResetAtTurn(afterTurn int, continuationPrompt string) Perturbation {
	return contextResetPerturbation{afterTurn: afterTurn, continuationPrompt: continuationPrompt}
}

type contextResetPerturbation struct {
	afterTurn          int
	continuationPrompt string
}

func (p contextResetPerturbation) Name() string { return "context-reset" }

func (p contextResetPerturbation) Apply(_ string, _ int, plan PromptPlan) (PromptPlan, error) {
	if p.afterTurn <= 0 {
		return PromptPlan{}, fmt.Errorf("context-reset after-turn must be positive")
	}
	if strings.TrimSpace(p.continuationPrompt) == "" {
		return PromptPlan{}, fmt.Errorf("context-reset continuation prompt is required")
	}
	if plan.ContextReset != nil {
		return PromptPlan{}, fmt.Errorf("multiple context-reset perturbations are not allowed")
	}
	reset := ContextReset{AfterTurn: p.afterTurn, ContinuationPrompt: p.continuationPrompt}
	plan.ContextReset = &reset
	plan.Perturbations = append(plan.Perturbations, p.Name())
	return plan, nil
}

// Validate checks arm balance inputs and binds immutable prompt-variant identity
// to the exact prompt bytes that the concrete executor will receive.
func (d Definition) Validate() error {
	if len(d.Arms) < 2 {
		return fmt.Errorf("experiment requires at least two arms")
	}
	if d.ID != "" && !validSlug(d.ID) {
		return fmt.Errorf("experiment id %q must be a bounded lowercase slug", d.ID)
	}
	seen := make(map[ArmID]struct{}, len(d.Arms))
	for _, arm := range d.Arms {
		if !validSlug(string(arm.ID)) {
			return fmt.Errorf("arm id %q must be a bounded lowercase slug", arm.ID)
		}
		if _, ok := seen[arm.ID]; ok {
			return fmt.Errorf("duplicate arm id %q", arm.ID)
		}
		seen[arm.ID] = struct{}{}
		if d.ID == "" {
			continue
		}
		subjectRef := strings.TrimSpace(arm.SubjectRef)
		if subjectRef == "" {
			return fmt.Errorf("arm %q subject ref is required", arm.ID)
		}
		if len(subjectRef) > 300 {
			return fmt.Errorf("arm %q subject ref exceeds 300 bytes", arm.ID)
		}
		expected := SHA256VariantRef(arm.SystemPrompt)
		if arm.VariantRef != expected {
			return fmt.Errorf("arm %q variant ref %q does not match system-prompt content (want %q)", arm.ID, arm.VariantRef, expected)
		}
	}
	for i, perturbation := range d.Perturbations {
		if perturbation == nil {
			return fmt.Errorf("perturbation %d is nil", i)
		}
		if strings.TrimSpace(perturbation.Name()) == "" {
			return fmt.Errorf("perturbation %d has an empty name", i)
		}
	}
	return nil
}

// Run executes a balanced case × trial × arm matrix in deterministic order,
// starting at trial index 1. The complete matrix is planned before the first
// callback, so invalid or asymmetric perturbation plans cannot leave partially
// executed evidence.
func Run[T any](
	ctx context.Context,
	definition Definition,
	cases []Case,
	trials int,
	runTrial func(context.Context, Trial) (T, error),
) (Report[T], error) {
	return RunFromTrial(ctx, definition, cases, 1, trials, runTrial)
}

// RunFromTrial executes a balanced matrix beginning at firstTrialIndex. This is
// intended for reviewed continuation of an existing experiment while preserving
// the immutable identities of earlier trials.
func RunFromTrial[T any](
	ctx context.Context,
	definition Definition,
	cases []Case,
	firstTrialIndex int,
	trials int,
	runTrial func(context.Context, Trial) (T, error),
) (Report[T], error) {
	planned, err := planTrials(definition, cases, firstTrialIndex, trials)
	if err != nil {
		return Report[T]{}, err
	}
	if runTrial == nil {
		return Report[T]{}, fmt.Errorf("run trial callback is required")
	}

	report := Report[T]{ExperimentID: definition.ID, TrialsPerArm: trials}
	for _, request := range planned {
		result, err := runTrial(ctx, request)
		if err != nil {
			return Report[T]{}, fmt.Errorf("case %s arm %s trial %d: %w", request.CaseID, request.Arm.ID, request.TrialIndex, err)
		}
		report.Outcomes = append(report.Outcomes, Outcome[T]{Trial: request, Result: result})
	}
	return report, nil
}

func planTrials(definition Definition, cases []Case, firstTrialIndex, trials int) ([]Trial, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	if firstTrialIndex <= 0 {
		return nil, fmt.Errorf("first trial index must be positive")
	}
	if trials <= 0 {
		return nil, fmt.Errorf("trials per arm must be positive")
	}
	if firstTrialIndex > math.MaxInt-(trials-1) {
		return nil, fmt.Errorf("trial index range overflows int")
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("at least one case is required")
	}

	seenCases := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if strings.TrimSpace(c.ID) == "" {
			return nil, fmt.Errorf("case id is required")
		}
		if _, ok := seenCases[c.ID]; ok {
			return nil, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seenCases[c.ID] = struct{}{}
		if strings.TrimSpace(c.Prompt) == "" {
			return nil, fmt.Errorf("case %q prompt is required", c.ID)
		}
	}

	if trials > math.MaxInt/len(cases)/len(definition.Arms) {
		return nil, fmt.Errorf("planned trial count overflows int")
	}
	plannedCount := len(cases) * trials * len(definition.Arms)
	planned := make([]Trial, 0, plannedCount)
	for _, c := range cases {
		for offset := 0; offset < trials; offset++ {
			trialIndex := firstTrialIndex + offset
			shared := PromptPlan{UserPrompt: c.Prompt}
			for _, perturbation := range definition.Perturbations {
				var err error
				shared, err = perturbation.Apply(c.ID, trialIndex, shared)
				if err != nil {
					return nil, fmt.Errorf("case %s trial %d perturbation %s: %w", c.ID, trialIndex, perturbation.Name(), err)
				}
				if shared.SystemPrompt != "" {
					return nil, fmt.Errorf("case %s trial %d perturbation %s modified reserved system prompt", c.ID, trialIndex, perturbation.Name())
				}
			}
			for _, arm := range definition.Arms {
				plan := clonePromptPlan(shared)
				plan.SystemPrompt = arm.SystemPrompt
				planned = append(planned, Trial{
					ExperimentID: definition.ID,
					CaseID:       c.ID,
					Arm:          arm,
					TrialIndex:   trialIndex,
					Prompt:       plan,
				})
			}
		}
	}
	return planned, nil
}

func clonePromptPlan(plan PromptPlan) PromptPlan {
	cloned := plan
	cloned.Perturbations = append([]string(nil), plan.Perturbations...)
	if plan.ContextReset != nil {
		reset := *plan.ContextReset
		cloned.ContextReset = &reset
	}
	return cloned
}

func validSlug(value string) bool {
	return len(value) <= SlugMaxLength && slugPattern.MatchString(value)
}
