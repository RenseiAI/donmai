package shell

import "github.com/RenseiAI/donmai/agent"

// shellExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only; a bare shell has no policy channel.
var shellExecutionSecurity = agent.ExecutionSecurityRendering{}
