package codex

import (
	"encoding/json"

	"github.com/RenseiAI/donmai/agent"
)

// TranslateInstructions reports how the Codex app-server translator honours
// a Spec (harness interaction contract D2). It is computed from the same
// builders Spawn uses, so the native field it names is the one sent.
func (p *Provider) TranslateInstructions(spec agent.Spec) agent.TranslationReport {
	return translateInstructions(spec)
}

func translateInstructions(spec agent.Spec) agent.TranslationReport {
	report := agent.TranslationReport{Harness: string(agent.ProviderCodex)}
	add := func(l agent.TranslationLine) { report.Lines = append(report.Lines, l) }
	plan := NewSpawnPlan(spec)
	turn := turnStartParams("thread", spec, plan.PromptInput)
	if model := resolveModel(spec); model != "" {
		add(agent.TranslationLine{Instruction: agent.InstructionModel, Requested: model, Outcome: agent.TranslationApplied, Native: "thread/start.model"})
	}
	if spec.Effort != "" {
		line := agent.TranslationLine{Instruction: agent.InstructionEffort, Requested: string(spec.Effort), Outcome: agent.TranslationUnsupported}
		if _, ok := turn["effort"]; ok {
			line.Outcome, line.Native = agent.TranslationApplied, "turn/start.effort"
		}
		add(line)
	}
	if mode := resolveSandboxMode(spec); mode != "" {
		add(agent.TranslationLine{Instruction: agent.InstructionSandbox, Requested: mode, Outcome: agent.TranslationApplied, Native: "thread/start.sandbox"})
	}
	if len(spec.ResponseSchema) > 0 {
		add(agent.TranslationLine{Instruction: agent.InstructionOutputSchema, Requested: "json-schema", Outcome: agent.TranslationApplied, Native: "turn/start.outputSchema"})
	}
	in := spec.Instructions
	if in == nil {
		return report
	}
	if in.Approvals != "" {
		line := agent.TranslationLine{Instruction: agent.InstructionApprovals, Requested: string(in.Approvals)}
		switch policy := codexApproval(in.Approvals); {
		case policy == "":
			// An unknown mode falls back to the base policy; it is not honoured.
			line.Outcome = agent.TranslationUnsupported
			line.Note = "no AskForApproval value; thread/start keeps approvalPolicy=" + resolveApprovalPolicy(spec)
		case policy == "never" && spec.PermissionConfig != nil:
			// With never, Codex issues no approval requests, so the bridge's
			// permission patterns are never consulted.
			line.Outcome, line.Native = agent.TranslationApplied, "thread/start.approvalPolicy=never"
			line.Note = "Codex issues no approval requests, so the approval bridge's permission patterns are not consulted"
		default:
			line.Outcome, line.Native = agent.TranslationApplied, "thread/start.approvalPolicy="+policy
		}
		add(line)
	}
	if in.PlanMode != "" && in.PlanMode != agent.PlanOff {
		add(agent.TranslationLine{
			Instruction: agent.InstructionPlanMode, Requested: string(in.PlanMode), Outcome: agent.TranslationUnsupported,
			Note: "the app-server exposes no plan mode on thread/start or turn/start",
		})
	}
	if tp := in.ToolPolicy; tp != nil && (len(tp.Allow)+len(tp.Deny)+len(tp.AllowFamilies)+len(tp.DenyFamilies)) > 0 {
		add(agent.TranslationLine{
			Instruction: agent.InstructionToolPolicy, Requested: "allow/deny", Outcome: agent.TranslationUnsupported,
			Note: "built-in shell and patch tools cannot be filtered per call",
		})
	}
	return report
}

// codexApproval maps the normalized approval mode onto AskForApproval.
func codexApproval(mode agent.ApprovalMode) string {
	switch mode {
	case agent.ApprovalNever:
		return "never"
	case agent.ApprovalOnRequest:
		return "on-request"
	case agent.ApprovalOnRisk:
		return "untrusted"
	}
	return ""
}

// effectiveSettings echoes what thread/start applied (D3): the response's
// own model, sandbox and approval policy, against what the translator
// requested.
//
// Effort is the exception. thread/start carries no effort parameter; a
// requested effort rides turn/start.effort, and no response or notification
// echoes it. The response's reasoningEffort is the thread's configured
// default, so it is a true echo only when no effort was requested (the turn
// then runs at that default). Otherwise the row reports the value turn/start
// sends, with translator evidence.
func effectiveSettings(spec agent.Spec, response json.RawMessage) (agent.EffectiveSettingsEvent, bool) {
	var resp struct {
		Model           string          `json:"model"`
		ReasoningEffort string          `json:"reasoningEffort"`
		Sandbox         json.RawMessage `json:"sandbox"`
		ApprovalPolicy  json.RawMessage `json:"approvalPolicy"`
	}
	if json.Unmarshal(response, &resp) != nil || resp.Model == "" {
		return agent.EffectiveSettingsEvent{}, false
	}
	var sandbox struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(resp.Sandbox, &sandbox)
	var approval string
	_ = json.Unmarshal(resp.ApprovalPolicy, &approval)
	echo := agent.EvidenceNativeEcho
	effort := agent.NewEffectiveSetting(agent.InstructionEffort, "", resp.ReasoningEffort, echo)
	if sent, _ := turnStartParams("", spec, nil)["effort"].(string); sent != "" {
		effort = agent.NewEffectiveSetting(agent.InstructionEffort, string(spec.Effort), sent, agent.EvidenceTranslator)
	}
	return agent.EffectiveSettingsEvent{Settings: []agent.EffectiveSetting{
		agent.NewEffectiveSetting(agent.InstructionModel, resolveModel(spec), resp.Model, echo),
		effort,
		agent.NewEffectiveSetting(agent.InstructionSandbox, resolveSandboxMode(spec), codexSandboxName(sandbox.Type), echo),
		agent.NewEffectiveSetting(agent.InstructionApprovals, resolveApprovalPolicy(spec), approval, echo),
	}, Raw: response}, true
}

// codexSandboxName maps the response's SandboxPolicy type onto the
// thread/start sandbox mode names the translator sends.
func codexSandboxName(t string) string {
	switch t {
	case "readOnly":
		return "read-only"
	case "workspaceWrite":
		return "workspace-write"
	case "dangerFullAccess":
		return "danger-full-access"
	}
	return t
}
