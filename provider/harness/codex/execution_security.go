package codex

import "github.com/RenseiAI/donmai/agent"

// codexExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9).
//
//   - toolApproval: bypass only. The approval bridge sees just the requests
//     codex chooses to raise, and it matches raw command text, so the deny
//     entries it carries are best-effort; no list level is declared.
//   - fileWrite: "workarea" renders natively on the headless app-server lane
//     as the workspace-write sandbox (writable roots = the workarea) with the
//     approval policy "never", so the model cannot escalate out of the
//     sandbox and nobody auto-approves an escalation (see
//     resolveApprovalPolicy). The interactive TUI lane honours an operator
//     approvals seed and a human can approve escalations there, so it does
//     not declare the level.
//   - fileRead: the workspace-write sandbox does not confine reads, so only
//     "host" renders. network, credentials and isolation render index 0 only.
var codexExecutionSecurity = agent.ExecutionSecurityRendering{
	DenyBaseline: agent.DenyBaselineBestEffort,
	Levels: []agent.RenderedExecutionSecurityLevel{{
		Dimension: agent.ExecutionSecurityFileWrite, Level: agent.FileWriteWorkarea,
		Modes:  []agent.PromptSessionMode{agent.PromptModeAutonomous},
		Layers: []agent.EnforcingLayer{agent.LayerHarnessNative},
	}},
}
