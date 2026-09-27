package claude

import "github.com/RenseiAI/donmai/agent"

// claudeExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9).
//
// Every spawn — headless and interactive — runs with the permission prompts
// skipped, so only toolApproval "bypass" renders here: the allow list is
// advisory in that mode, and the deny entries reach the CLI as
// --disallowedTools, best-effort and unattested. No level above bypass is
// declared until a no-prompt mode's deny rules are proven on the exact
// pinned version by a negative fixture. The CLI applies no filesystem,
// network or credential containment of its own, so every other dimension
// renders index 0 only.
var claudeExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineBestEffort}
