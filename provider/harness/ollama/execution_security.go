package ollama

import "github.com/RenseiAI/donmai/agent"

// ollamaExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only; the loop has no tool surface.
var ollamaExecutionSecurity = agent.ExecutionSecurityRendering{}
