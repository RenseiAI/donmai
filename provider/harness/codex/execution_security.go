package codex

import "github.com/RenseiAI/donmai/agent"

// codexExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only.
//
//   - toolApproval: the headless approval bridge carries the deny entries,
//     but it only sees requests that leave codex's own sandbox (escalations),
//     and it matches raw command text. Under the full-access grant nothing
//     escalates, so the entries have nothing to act on
//     (DenyChannelNeedsSandbox). The interactive profile carries none.
//   - fileWrite "workarea" is not declared: the workspace-write sandbox is
//     unproven on the pinned version, leaves the shared temporary
//     directories writable, and refuses writes to the worktree's git
//     metadata, so it neither meets nor honestly reports the level.
//   - fileRead, network, credentials and isolation: nothing confines them.
var codexExecutionSecurity = agent.ExecutionSecurityRendering{DenyChannelNeedsSandbox: true}
