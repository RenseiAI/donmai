package amp

import "github.com/RenseiAI/donmai/agent"

// ampExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// amp exposes no deny or allow channel this adapter drives, so the deny
// baseline is unavailable and every dimension renders index 0 only.
var ampExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineUnavailable}
