// Package claude implements an agent.Provider that shells out to the
// Anthropic Claude Code CLI (`claude`).
//
// This is the v0.5.0 production provider for the agent-runner subsystem.
// It satisfies the agent.Provider contract defined in
// github.com/RenseiAI/agentfactory-tui/agent and is wired into the
// daemon's WorkerCommand path via afcli's `af agent run` subcommand
// (delivered separately in F.2.4).
//
// # Implementation strategy
//
// Per F.1.1 §3.1 and the locked coordinator decisions, this provider:
//
//   - Shells out to the `claude` binary (the Anthropic Claude Code CLI)
//     in `--print --output-format stream-json --verbose` mode. Each
//     line of stdout is a JSON event the provider parses and maps to
//     an agent.Event variant (see jsonl.go).
//   - Does NOT depend on Node.js at runtime — pure subprocess.
//   - Probes for `claude` on PATH at construction (New); fails fast
//     with agent.ErrProviderUnavailable if missing, before any worktree
//     work would have been done.
//   - Configures MCP stdio servers via a per-session JSON tmpfile
//     passed to `--mcp-config` (see mcp.go).
//   - Translates an agent.Spec into the CLI's flag set (see cli_args.go).
//
// # Capability matrix (v0.5.0)
//
// Per coordinator decision #1 in F.1.1 §10 (with F.2.3-cap-flip
// follow-up REN-1455), this provider ships with
// SupportsMessageInjection=true (between-turn) and
// SupportsSessionResume=false.
//
//   - SupportsMessageInjection=true: between-turn injection via
//     `claude --resume <session-id> -p <text>`. Each Inject() call
//     spawns a fresh resume subprocess and forwards its JSONL stream
//     onto the parent Handle's events channel. Same semantic level
//     as the legacy TS Agent SDK. Sequential (one --resume at a
//     time); see ErrInjectInFlight. Option C upgrade (REN-1451)
//     replaces the subprocess shell-out with the Anthropic Go SDK
//     for true mid-turn injection without subprocess overhead.
//
//   - SupportsSessionResume=false: while the CLI exposes `--resume
//     <session-id>`, the v0.5.0 runner does not yet exercise the
//     resume code path on Provider.Resume. Flip to true in v0.5.+
//     once F.5's option C lands (REN-1451).
//
// All other capabilities follow F.1.1 §3.1: tool plugins (true), no
// base instructions (false), no permission config (false), code-intel
// enforcement (false), subagent events (true), reasoning effort
// (true), Claude tool-permission grammar.
//
// # Failure-mode protocol
//
// Per F.1.1 §5:
//
//   - The CLI shell-out does not make HTTP calls directly, so the
//     3-attempt exponential backoff wrapper does not apply at this
//     layer. The runner-level result poster is responsible for retries.
//   - Subprocess failure (non-zero exit, EOF without terminal result,
//     spawn error) is propagated cleanly via a synthetic ErrorEvent
//     followed by close of the events channel — never panics.
//   - ctx.Done() (cancellation) sends SIGTERM to the subprocess, waits
//     up to 5 seconds, then SIGKILL, then closes the events channel.
//
// # Legacy TS reference
//
// The legacy in-process Anthropic SDK provider lives at
// ../agentfactory/packages/core/src/providers/claude-provider.ts. The
// JSONL → Event mapping in jsonl.go ports the verbatim `mapSDKMessage`
// translation logic from that file. The Go port does NOT re-implement
// the in-process SDK integration (that is a Node-runtime concern).
package claude
