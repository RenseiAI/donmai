// Package prompt renders work-type-specific agent prompts for the Go
// runner subsystem.
//
// Per F.1.1 §1, this package owns template loading and rendering of the
// (system, user) prompt pair the provider's Spawn method consumes. It is
// pure: input is a [QueuedWork] (the Redis session JSON shape the
// daemon's poll loop already decodes), output is two strings.
//
// # Responsibilities
//
//   - Load default templates baked into the binary via [embed.FS].
//   - Compose a stable, deterministic system prompt encapsulating
//     base instructions, agent identity, project + repository context.
//   - Compose a user prompt derived from the QueuedWork.PromptContext
//     (Linear issue body + identifier + project metadata).
//   - Stay deterministic — golden-file snapshot tests live alongside.
//
// # Boundaries
//
//   - Does NOT shell out, make network calls, or touch the filesystem.
//   - Does NOT know about MCP, providers, worktrees, or platform APIs.
//   - Does NOT load .agentfactory/templates/ overrides — that is
//     deferred to F.5 once a worktree-aware loader is wired (see
//     [Builder.WithOverrides] reservation).
//
// # Migration note
//
// The legacy TS prompt builder lives in
// ../agentfactory/packages/core/src/templates/{registry,renderer}.ts.
// This Go port carries forward only the v0.5.0 work types
// (development, qa, research) per the F.2 scope. The full Handlebars
// template surface is deferred to F.5 — once the v0.5.0 walkthrough is
// green we can grow the template set without breaking the Builder
// contract.
//
// Source: F.1.1 §1 ("prompt/" row), F.0.1 §5.
package prompt
