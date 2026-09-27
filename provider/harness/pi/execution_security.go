package pi

import "github.com/RenseiAI/donmai/agent"

// piExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9).
//
// pi ships no permission system; the handshake-verified policy extension is
// the injected boundary every built-in tool call crosses. It carries the deny
// entries at every level, but its tool patterns match a text prefix of the
// raw command, which chaining defeats, so the deny baseline is best-effort
// and no list level is declared. Path containment on the file tools does not
// confine the bash tool, so fileRead and fileWrite render "host" only.
var piExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineBestEffort}
