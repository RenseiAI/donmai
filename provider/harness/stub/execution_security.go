package stub

import "github.com/RenseiAI/donmai/agent"

// stubExecutionSecurity is the test double's execution-security rendering
// declaration: it enforces nothing, so the deny baseline is unavailable and
// every dimension renders index 0 only.
var stubExecutionSecurity = agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineUnavailable}
