package agent

import (
	"fmt"
	"strings"
)

// Normalized instructions (ADR-2026-11-20 harness interaction contract, D2):
// harness-neutral intents an adapter translates to its harness's native
// controls, reporting for each whether it applied, approximated or could not
// honour it.

// PlanMode asks the harness to plan before (or instead of) acting.
type PlanMode string

// Plan modes. Stable wire values; empty means off.
const (
	PlanOff   PlanMode = "off"
	PlanFirst PlanMode = "plan-first"
	PlanOnly  PlanMode = "plan-only"
)

// ApprovalMode is when the harness stops to ask before acting.
type ApprovalMode string

// Approval modes. Stable wire values.
const (
	// ApprovalNever runs autonomously inside the sandbox.
	ApprovalNever ApprovalMode = "never"
	// ApprovalOnRequest asks when the model requests escalation.
	ApprovalOnRequest ApprovalMode = "on-request"
	// ApprovalOnRisk asks before any action outside the trusted set.
	ApprovalOnRisk ApprovalMode = "on-risk"
)

// ToolPolicy allows and denies tools by family (the agent/present families:
// command, read, edit, search, web, agent, mcp) and by exact name. Deny wins
// over allow; an empty allow list allows everything not denied.
type ToolPolicy struct {
	AllowFamilies []string `json:"allowFamilies,omitempty"`
	DenyFamilies  []string `json:"denyFamilies,omitempty"`
	Allow         []string `json:"allow,omitempty"`
	Deny          []string `json:"deny,omitempty"`
}

// Instructions is the normalized instruction block on Spec.
type Instructions struct {
	PlanMode   PlanMode     `json:"planMode,omitempty"`
	Approvals  ApprovalMode `json:"approvals,omitempty"`
	ToolPolicy *ToolPolicy  `json:"toolPolicy,omitempty"`
	// Required names the instructions (InstructionPlanMode, …) that must
	// translate to applied or approximated; an unsupported required
	// instruction refuses the spawn.
	Required []string `json:"required,omitempty"`
}

// Instruction names used in translation reports and Instructions.Required.
const (
	InstructionPlanMode     = "planMode"
	InstructionApprovals    = "approvals"
	InstructionToolPolicy   = "toolPolicy"
	InstructionOutputSchema = "outputSchema"
	InstructionEffort       = "effort"
	InstructionModel        = "model"
	InstructionSandbox      = "sandbox"
)

// TranslationOutcome is how a harness honours one instruction.
type TranslationOutcome string

// Translation outcomes. Stable wire values.
const (
	TranslationApplied      TranslationOutcome = "applied"
	TranslationApproximated TranslationOutcome = "approximated"
	TranslationUnsupported  TranslationOutcome = "unsupported"
)

// TranslationLine is one instruction's translation: the requested value, the
// outcome, the native control that carries it (empty when unsupported) and a
// one-line note for approximations.
type TranslationLine struct {
	Instruction string             `json:"instruction"`
	Requested   string             `json:"requested"`
	Outcome     TranslationOutcome `json:"outcome"`
	Native      string             `json:"native,omitempty"`
	Note        string             `json:"note,omitempty"`
}

// TranslationReport is a translator's deterministic account of a Spec. It is
// computed without starting the harness, so a control plane can show it
// before dispatch.
type TranslationReport struct {
	Harness string            `json:"harness"`
	Lines   []TranslationLine `json:"lines"`
}

// InstructionTranslator is implemented by adapters that report how they
// translate a Spec's instructions and knobs.
type InstructionTranslator interface {
	TranslateInstructions(spec Spec) TranslationReport
}

// Line returns the report line for an instruction.
func (r TranslationReport) Line(instruction string) (TranslationLine, bool) {
	for _, l := range r.Lines {
		if l.Instruction == instruction {
			return l, true
		}
	}
	return TranslationLine{}, false
}

// CheckRequired refuses a report in which a required instruction is
// unsupported (or missing from the report, which is the same thing). The
// error wraps ErrInstructionUnsupported and names every offending line.
func (r TranslationReport) CheckRequired(required []string) error {
	var bad []string
	for _, name := range required {
		l, ok := r.Line(name)
		if !ok || l.Outcome == TranslationUnsupported {
			detail := name
			if ok && l.Requested != "" {
				detail += "=" + l.Requested
			}
			bad = append(bad, detail)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s cannot honour %s", ErrInstructionUnsupported, r.Harness, strings.Join(bad, ", "))
}

// EventEffectiveSettings is the D3 echo of what a harness applied.
const EventEffectiveSettings EventKind = "effective_settings"

// SettingEvidence says where an applied value came from.
type SettingEvidence string

// Setting evidence. Stable wire values.
const (
	// EvidenceNativeEcho: the harness reported the value itself.
	EvidenceNativeEcho SettingEvidence = "native-echo"
	// EvidenceTranslator: the translator's report, with no harness echo.
	EvidenceTranslator SettingEvidence = "translator"
)

// EffectiveSetting is one applied value with its provenance.
type EffectiveSetting struct {
	Setting   string          `json:"setting"`
	Requested string          `json:"requested,omitempty"`
	Applied   string          `json:"applied,omitempty"`
	Evidence  SettingEvidence `json:"evidence"`
	Mismatch  bool            `json:"mismatch,omitempty"`
}

// EffectiveSettingsEvent reports what the harness actually applied: once
// after start with every setting, then a delta with only changed settings
// (a model reroute, an effort clamp). Consumers show the latest per setting.
type EffectiveSettingsEvent struct {
	Settings []EffectiveSetting `json:"settings"`
	Raw      any                `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (EffectiveSettingsEvent) Kind() EventKind { return EventEffectiveSettings }
func (EffectiveSettingsEvent) isAgentEvent()   {}

// NewEffectiveSetting builds a setting line, flagging a mismatch when both
// values are known and differ.
func NewEffectiveSetting(setting, requested, applied string, evidence SettingEvidence) EffectiveSetting {
	return EffectiveSetting{
		Setting: setting, Requested: requested, Applied: applied, Evidence: evidence,
		Mismatch: requested != "" && applied != "" && requested != applied,
	}
}
