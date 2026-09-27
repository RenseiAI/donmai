package shell

import "github.com/RenseiAI/donmai/agent"

// shellExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// a bare shell has no policy channel, so the deny baseline is unavailable and
// every dimension renders index 0 only.
var shellExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineUnavailable}
