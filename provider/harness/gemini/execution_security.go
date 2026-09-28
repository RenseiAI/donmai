package gemini

import "github.com/RenseiAI/donmai/agent"

// geminiExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only. The in-box loop checks every tool call against the deny
// entries before dispatch, but Bash patterns match a text prefix of the raw
// command, so they are best effort and no list level is declared. Tools
// execute directly on the host.
var geminiExecutionSecurity = agent.ExecutionSecurityRendering{}
