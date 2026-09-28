package amp

import "github.com/RenseiAI/donmai/agent"

// ampExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only; amp exposes no tool policy channel this adapter drives.
var ampExecutionSecurity = agent.ExecutionSecurityRendering{}
