// Package span turns normalized agent events into the additive per-call span
// contract and forwards completed spans without blocking the agent runtime.
//
// The package deliberately carries metadata only. Prompt, completion, tool
// input, and tool output bodies never enter the span transport; optional prompt
// and context correlation is digest-only through agent.DonmaiSpanExtensions.
package span
