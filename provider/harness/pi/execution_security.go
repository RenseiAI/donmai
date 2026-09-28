package pi

import "github.com/RenseiAI/donmai/agent"

// piExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only. pi ships no permission system; the handshake-verified policy
// extension carries the deny entries on every built-in tool call, but its
// tool patterns match a text prefix of the raw command, which chaining
// defeats, so they are best effort and no list level is declared. Path
// containment on the file tools does not confine the bash tool.
var piExecutionSecurity = agent.ExecutionSecurityRendering{}
