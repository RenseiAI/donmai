package agycli

import "github.com/RenseiAI/donmai/agent"

// agyExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only; the CLI always auto-approves and exposes no deny channel.
var agyExecutionSecurity = agent.ExecutionSecurityRendering{}
