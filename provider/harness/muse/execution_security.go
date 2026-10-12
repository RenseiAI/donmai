package muse

import "github.com/RenseiAI/donmai/agent"

// museExecutionSecurity is this adapter's execution-security rendering
// declaration: index 0 only.
//
//   - toolApproval: the approval bridge carries the deny entries, but it
//     only sees requests that leave the host's own sandbox
//     (escalations), and it matches raw command text. Under a full-access
//     grant nothing escalates, so the entries have nothing to act on
//     (DenyChannelNeedsSandbox). The read-only lane pins denyUnmatched
//     at the serve flags, not through this bridge.
//   - fileWrite/fileRead, network, credentials and isolation: nothing
//     confines them on this slice (confinement wrap is a later slice).
var museExecutionSecurity = agent.ExecutionSecurityRendering{DenyChannelNeedsSandbox: true}
