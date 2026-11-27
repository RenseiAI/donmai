package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTranslationReportCheckRequired(t *testing.T) {
	t.Parallel()
	report := TranslationReport{Harness: "h", Lines: []TranslationLine{
		{Instruction: InstructionPlanMode, Requested: "plan-only", Outcome: TranslationUnsupported},
		{Instruction: InstructionApprovals, Requested: "never", Outcome: TranslationApplied, Native: "approvalPolicy"},
		{Instruction: InstructionEffort, Requested: "max", Outcome: TranslationApproximated, Note: "clamped to xhigh"},
	}}
	if err := report.CheckRequired([]string{InstructionApprovals, InstructionEffort}); err != nil {
		t.Fatalf("applied and approximated required lines refused: %v", err)
	}
	err := report.CheckRequired([]string{InstructionPlanMode, InstructionToolPolicy})
	if !errors.Is(err, ErrInstructionUnsupported) {
		t.Fatalf("err = %v; want ErrInstructionUnsupported", err)
	}
	for _, want := range []string{"planMode=plan-only", "toolPolicy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if err := report.CheckRequired(nil); err != nil {
		t.Fatalf("no required instructions refused: %v", err)
	}
}

func TestEffectiveSettingsRoundTripAndMismatch(t *testing.T) {
	t.Parallel()
	ev := EffectiveSettingsEvent{Settings: []EffectiveSetting{
		NewEffectiveSetting(InstructionModel, "m-1", "m-1", EvidenceNativeEcho),
		NewEffectiveSetting(InstructionEffort, "max", "xhigh", EvidenceNativeEcho),
		NewEffectiveSetting(InstructionSandbox, "workspace-write", "", EvidenceTranslator),
	}}
	if ev.Settings[0].Mismatch || !ev.Settings[1].Mismatch || ev.Settings[2].Mismatch {
		t.Fatalf("mismatch flags = %v %v %v; want false true false", ev.Settings[0].Mismatch, ev.Settings[1].Mismatch, ev.Settings[2].Mismatch)
	}
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ev) || !isKnownEvent(EventEffectiveSettings) {
		t.Fatalf("round trip = %#v; want %#v", got, ev)
	}
}

func TestSpecInstructionsWire(t *testing.T) {
	t.Parallel()
	spec := Spec{Instructions: &Instructions{
		PlanMode: PlanOnly, Approvals: ApprovalNever,
		ToolPolicy: &ToolPolicy{DenyFamilies: []string{"web"}}, Required: []string{InstructionPlanMode},
	}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"instructions":{"planMode":"plan-only","approvals":"never","toolPolicy":{"denyFamilies":["web"]},"required":["planMode"]}`) {
		t.Fatalf("instructions wire = %s", raw)
	}
	if empty, _ := json.Marshal(Spec{}); strings.Contains(string(empty), "instructions") {
		t.Fatalf("absent instructions serialized: %s", empty)
	}
}
