package codex

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestTranslateInstructionsReportsNativeFields(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{
		Model: "gpt-5.6-sol", Effort: agent.EffortHigh, SandboxLevel: agent.SandboxWorkspaceWrite,
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Instructions: &agent.Instructions{
			PlanMode: agent.PlanOnly, Approvals: agent.ApprovalNever,
			ToolPolicy: &agent.ToolPolicy{DenyFamilies: []string{"web"}},
		},
	}
	report := translateInstructions(spec)
	want := map[string]agent.TranslationOutcome{
		agent.InstructionModel: agent.TranslationApplied, agent.InstructionEffort: agent.TranslationApplied,
		agent.InstructionSandbox: agent.TranslationApplied, agent.InstructionOutputSchema: agent.TranslationApplied,
		agent.InstructionApprovals: agent.TranslationApplied, agent.InstructionPlanMode: agent.TranslationUnsupported,
		agent.InstructionToolPolicy: agent.TranslationUnsupported,
	}
	for name, outcome := range want {
		line, ok := report.Line(name)
		if !ok || line.Outcome != outcome {
			t.Errorf("%s = %+v (present %v); want %s", name, line, ok, outcome)
		}
	}
	if line, _ := report.Line(agent.InstructionApprovals); line.Native != "thread/start.approvalPolicy=never" {
		t.Errorf("approvals native = %q; the normalized mode must reach thread/start", line.Native)
	}
	if got := NewSpawnPlan(spec).ThreadStart["approvalPolicy"]; got != "never" {
		t.Errorf("thread/start approvalPolicy = %v; want never", got)
	}
	spec.Instructions.Required = []string{agent.InstructionPlanMode}
	if err := report.CheckRequired(spec.Instructions.Required); !errors.Is(err, agent.ErrInstructionUnsupported) {
		t.Errorf("required plan mode not refused: %v", err)
	}
}

func TestEffectiveSettingsFromThreadStartResponse(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{Model: "gpt-5.6-sol", Effort: agent.EffortMax, SandboxLevel: agent.SandboxReadOnly}
	ev, ok := effectiveSettings(spec, json.RawMessage(`{"model":"gpt-5.6-sol","reasoningEffort":"xhigh","sandbox":{"type":"readOnly"},"approvalPolicy":"untrusted","thread":{"id":"t"}}`))
	if !ok || len(ev.Settings) != 4 {
		t.Fatalf("settings = %+v, %v", ev, ok)
	}
	byName := map[string]agent.EffectiveSetting{}
	for _, s := range ev.Settings {
		byName[s.Setting] = s
		if s.Evidence != agent.EvidenceNativeEcho {
			t.Errorf("%s evidence = %s; want native echo", s.Setting, s.Evidence)
		}
	}
	if e := byName[agent.InstructionEffort]; !e.Mismatch || e.Applied != "xhigh" {
		t.Errorf("effort = %+v; want the clamp reported as a mismatch", e)
	}
	if s := byName[agent.InstructionSandbox]; s.Mismatch || s.Applied != "read-only" {
		t.Errorf("sandbox = %+v; want read-only, no mismatch", s)
	}
	if _, ok := effectiveSettings(spec, json.RawMessage(`{"thread":{"id":"t"}}`)); ok {
		t.Error("a response without a model produced settings")
	}
}

func TestSettingsEchoWaitsForInit(t *testing.T) {
	t.Parallel()
	h := &Handle{events: make(chan agent.Event, 4)}
	h.queueSettings(agent.EffectiveSettingsEvent{Settings: []agent.EffectiveSetting{{Setting: "model"}}})
	if len(h.events) != 0 {
		t.Fatal("echo emitted before the InitEvent")
	}
	h.initDone()
	if ev := <-h.events; ev.Kind() != agent.EventEffectiveSettings {
		t.Fatalf("released %s; want the held echo", ev.Kind())
	}
	h.queueSettings(agent.EffectiveSettingsEvent{})
	if len(h.events) != 1 {
		t.Fatal("echo after init was not emitted immediately")
	}
}
