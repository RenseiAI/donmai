package ollama

import "github.com/RenseiAI/donmai/agent"

// ollamaExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// the loop has no tool surface and no policy channel, so the deny baseline
// is unavailable and every dimension renders index 0 only.
var ollamaExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineUnavailable}
