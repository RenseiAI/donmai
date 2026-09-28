package claude

import "github.com/RenseiAI/donmai/agent"

// claudeExecutionSecurity is this adapter's execution-security rendering
// declaration (ADR-2026-09-27-execution-security-levels.md; checklist row 9):
// index 0 only. Every spawn runs with the permission prompts skipped, so the
// allow list is advisory; the headless profile carries the deny entries as
// --disallowedTools (best effort, unattested) and the interactive profile
// carries none. No level above bypass is declared until a no-prompt mode's
// deny rules are proven on the exact pinned version by a negative fixture.
// The CLI applies no filesystem, network or credential containment itself.
var claudeExecutionSecurity = agent.ExecutionSecurityRendering{}
