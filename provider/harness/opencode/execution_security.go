package opencode

import "github.com/RenseiAI/donmai/agent"

// opencodeExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only. The deny entries ride the session permission map, whose deny
// rules opencode keeps under --auto; they are glob matches on the raw command
// and unproven by a negative fixture, so they are best effort.
var opencodeExecutionSecurity = agent.ExecutionSecurityRendering{}
