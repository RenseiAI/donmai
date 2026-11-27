package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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
	thread := realWireThread("t")
	resp := realWireThreadStartResponse(thread)
	resp["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": false}
	resp["approvalPolicy"] = "untrusted"
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	spec := agent.Spec{Model: "gpt-5.6-sol", Effort: agent.EffortHigh, SandboxLevel: agent.SandboxReadOnly}
	ev, ok := effectiveSettings(spec, raw)
	if !ok || len(ev.Settings) != 4 {
		t.Fatalf("settings = %+v, %v", ev, ok)
	}
	byName := map[string]agent.EffectiveSetting{}
	for _, s := range ev.Settings {
		byName[s.Setting] = s
	}
	for _, name := range []string{agent.InstructionModel, agent.InstructionSandbox, agent.InstructionApprovals} {
		if s := byName[name]; s.Evidence != agent.EvidenceNativeEcho {
			t.Errorf("%s evidence = %s; want native echo", name, s.Evidence)
		}
	}
	// thread/start carries no effort: the response's reasoningEffort is the
	// configured default ("medium"), not an echo of the requested "high",
	// which rides turn/start.effort. Report the translator's value.
	if e := byName[agent.InstructionEffort]; e.Applied != "high" || e.Evidence != agent.EvidenceTranslator || e.Mismatch {
		t.Errorf("effort = %+v; want high with translator evidence, no mismatch", e)
	}
	if s := byName[agent.InstructionSandbox]; s.Mismatch || s.Applied != "read-only" {
		t.Errorf("sandbox = %+v; want read-only, no mismatch", s)
	}
	// With no effort requested, turn/start sends none and the turn runs at
	// the thread default, so the response value is a true echo.
	spec.Effort = ""
	ev, _ = effectiveSettings(spec, raw)
	for _, s := range ev.Settings {
		if s.Setting == agent.InstructionEffort && (s.Applied != "medium" || s.Evidence != agent.EvidenceNativeEcho) {
			t.Errorf("default effort = %+v; want the thread default as a native echo", s)
		}
	}
	if _, ok := effectiveSettings(spec, json.RawMessage(`{"thread":{"id":"t"}}`)); ok {
		t.Error("a response without a model produced settings")
	}
}

func TestEmitInitOnceThenDenyNotice(t *testing.T) {
	t.Parallel()
	h := &Handle{events: make(chan agent.Event, 8), denyNotice: &agent.SystemEvent{Subtype: "deny"}}
	h.emitInit(agent.InitEvent{SessionID: "t"})
	h.emitInit(agent.InitEvent{SessionID: "t"})
	close(h.events)
	var kinds []agent.EventKind
	for ev := range h.events {
		kinds = append(kinds, ev.Kind())
	}
	if len(kinds) != 2 || kinds[0] != agent.EventInit || kinds[1] != agent.EventSystem {
		t.Fatalf("kinds = %v; want one init then the deny notice", kinds)
	}
}

func TestTranslateInstructionsApprovalModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode    agent.ApprovalMode
		perms   bool
		outcome agent.TranslationOutcome
		native  string
		note    bool
	}{
		{mode: agent.ApprovalOnRisk, outcome: agent.TranslationApplied, native: "thread/start.approvalPolicy=untrusted"},
		{mode: agent.ApprovalOnRequest, outcome: agent.TranslationApplied, native: "thread/start.approvalPolicy=on-request"},
		{mode: agent.ApprovalNever, outcome: agent.TranslationApplied, native: "thread/start.approvalPolicy=never"},
		{mode: agent.ApprovalNever, perms: true, outcome: agent.TranslationApplied, native: "thread/start.approvalPolicy=never", note: true},
		{mode: "always", outcome: agent.TranslationUnsupported, note: true},
	} {
		spec := agent.Spec{Autonomous: true, Instructions: &agent.Instructions{Approvals: tc.mode}}
		if tc.perms {
			spec.PermissionConfig = &agent.PermissionConfig{DisallowPatterns: []string{"Bash(env)"}}
		}
		report := translateInstructions(spec)
		line, ok := report.Line(agent.InstructionApprovals)
		if !ok || line.Outcome != tc.outcome || line.Native != tc.native || (line.Note != "") != tc.note {
			t.Errorf("%s (perms %v) = %+v; want %s %q note=%v", tc.mode, tc.perms, line, tc.outcome, tc.native, tc.note)
		}
		if want := codexApproval(tc.mode); want != "" && NewSpawnPlan(spec).ThreadStart["approvalPolicy"] != want {
			t.Errorf("%s: thread/start approvalPolicy = %v; want %s", tc.mode, NewSpawnPlan(spec).ThreadStart["approvalPolicy"], want)
		}
		err := report.CheckRequired([]string{agent.InstructionApprovals})
		if refused := errors.Is(err, agent.ErrInstructionUnsupported); refused != (tc.outcome == agent.TranslationUnsupported) {
			t.Errorf("%s: required approvals check = %v", tc.mode, err)
		}
	}
	if _, ok := translateInstructions(agent.Spec{Instructions: &agent.Instructions{PlanMode: agent.PlanOff}}).Line(agent.InstructionPlanMode); ok {
		t.Error("plan mode off produced a line; it asks for nothing")
	}
}

// TestHandle_RealWireEmitsInitThenSettings drives a fresh headless spawn over
// the codex-cli 0.171+ wire: thread/started has no top-level threadId, so it
// never reaches the handle's subscription. The InitEvent, the deny notice and
// the effective-settings echo must still be delivered, in that order, built
// from the thread/start response.
func TestHandle_RealWireEmitsInitThenSettings(t *testing.T) {
	p, fs := newTestProvider(t)
	fs.mu.Lock()
	fs.realThreadWire = true
	fs.mu.Unlock()

	spec := denySpec(agent.SandboxWorkspaceWrite, "Read(~/.ssh/**)")
	spec.Cwd, spec.Model, spec.Effort = "/tmp/wt", "gpt-5.6-sol", agent.EffortHigh
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got := drainEvents(t, h.Events(), 5*time.Second)
	kinds := kindsOf(got)
	if len(got) < 3 || kinds[0] != agent.EventInit || kinds[1] != agent.EventSystem || kinds[2] != agent.EventEffectiveSettings {
		t.Fatalf("kinds = %v; want init, deny notice, effective_settings first", kinds)
	}
	if init := got[0].(agent.InitEvent); init.SessionID != "thread-A" {
		t.Errorf("init session = %q; want thread-A", init.SessionID)
	}
	inits := 0
	for _, k := range kinds {
		if k == agent.EventInit {
			inits++
		}
	}
	if inits != 1 {
		t.Errorf("init emitted %d times; want once", inits)
	}
	byName := map[string]agent.EffectiveSetting{}
	for _, s := range got[2].(agent.EffectiveSettingsEvent).Settings {
		byName[s.Setting] = s
	}
	if m := byName[agent.InstructionModel]; m.Applied != "gpt-5.6-sol" || m.Evidence != agent.EvidenceNativeEcho || m.Mismatch {
		t.Errorf("model = %+v; want the native echo", m)
	}
	// The response's reasoningEffort is the thread default ("medium"); the
	// turn runs at the requested "high" sent on turn/start.effort.
	if e := byName[agent.InstructionEffort]; e.Applied != "high" || e.Evidence != agent.EvidenceTranslator || e.Mismatch {
		t.Errorf("effort = %+v; want high from the translator, no mismatch", e)
	}
	if s := byName[agent.InstructionSandbox]; s.Applied != "workspace-write" || s.Mismatch {
		t.Errorf("sandbox = %+v", s)
	}
	if a := byName[agent.InstructionApprovals]; a.Applied != "on-request" || a.Mismatch {
		t.Errorf("approvals = %+v", a)
	}
}
