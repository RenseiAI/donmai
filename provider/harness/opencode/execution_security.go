package opencode

import "github.com/RenseiAI/donmai/agent"

// opencodeExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9).
//
// The deny entries are rendered into the session permission map, whose deny
// rules opencode keeps under --auto; they are glob matches on the raw
// command and unproven by a negative fixture, so the deny baseline is
// best-effort and no list level is declared. Every other dimension renders
// index 0 only.
var opencodeExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineBestEffort}
