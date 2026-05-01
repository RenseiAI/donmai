// Package runner orchestrates one agent session end-to-end.
//
// It is the keystone of v0.5.0 — F.2.8 (daemon wire-up) calls
// Runner.Run for every claimed [QueuedWork] and the function does not
// return until the session is fully terminated (a result has been
// posted, the worktree torn down, and the heartbeat stopped).
//
// The package wires the Wave 2/2b building blocks into the
// per-session main loop:
//
//	┌───────────────────────────────────────────────────────────────┐
//	│                      runner.Run(ctx, qw)                      │
//	│                                                               │
//	│  1.  Translate QueuedWork → agent.Spec (spec_translation.go)  │
//	│  2.  Resolve Provider via Registry        (registry.go)       │
//	│  3.  Provision worktree                   (runtime/worktree)  │
//	│  4.  Compose env (blocklist applied)      (runtime/env)       │
//	│  5.  Build MCP stdio config tmpfile       (runtime/mcp)       │
//	│  6.  Render system+user prompt            (prompt)            │
//	│  7.  Spawn provider                       (provider/...)      │
//	│  8.  Start heartbeat pulser               (runtime/heartbeat) │
//	│  9.  Stream events → state.json + post    (runtime/state)     │
//	│ 10.  Wait for terminal event (or cancel)                      │
//	│ 11.  Tail recovery: steering → backstop   (steering/backstop) │
//	│ 12.  Post result                          (result.Poster)     │
//	│ 13.  Teardown                                                 │
//	└───────────────────────────────────────────────────────────────┘
//
// The package exports a small, public surface so rensei-tui can
// embed the runner without depending on private daemon plumbing:
//
//   - [Runner] — long-lived per-daemon orchestrator built once via
//     [New], then [Runner.Run] called per session.
//   - [QueuedWork] — input contract; mirrors the platform Redis
//     payload plus the resolved-profile knob.
//   - [Result] — terminal output; identical to [agent.Result] today
//     but kept distinct for forward compatibility (so future runner
//     wave hooks can extend it without touching the agent package).
//   - [Registry] — provider resolution; configurable so test code can
//     swap stubs in.
//
// Failure modes follow F.1.1 §5 verbatim:
//
//   - Worktree provisioning retries 3 times with 15s delay. Lost
//     ownership during retry short-circuits the run.
//   - Heartbeat 3-strike trips [runtime/heartbeat.ErrLostOwnership];
//     the runner cancels the provider via Handle.Stop and records
//     FailureMode "lost-ownership".
//   - Provider Spawn errors map to FailureMode "spawn-failed".
//   - ErrorEvents on the stream before any terminal ResultEvent map
//     to FailureMode "provider-error".
//   - Steering failure falls through to backstop; backstop failure
//     records its diagnostics on Result.BackstopReport.
//
// Two-stage tail recovery on agent completion (F.0.1 §1):
//
//   - Stage 1 (steering): when the provider supports message
//     injection or session resume and the session ended without a
//     PR, the runner sends a per-provider templated steering prompt
//     asking the agent to commit/push/PR.
//   - Stage 2 (backstop): when steering did not produce a PR (or the
//     provider does not support steering), the runner runs a
//     deterministic git workflow (`git add -A` with the path-exclude
//     list, `git commit`, `git push`, `gh pr create`).
//
// The path-exclude list in [backstop.go] is ported verbatim from the
// legacy TS at agentfactory/packages/core/src/orchestrator/session-backstop.ts:57-95.
//
// # Telemetry
//
// Every Event is mirrored three ways:
//
//  1. Appended to <worktree>/.agent/events.jsonl (audit trail).
//  2. State-store snapshot updated for crash recovery.
//  3. Logger.Debug for operator dashboards.
//
// Posting per-event activity to the platform is left to F.5; today's
// runner only posts the terminal Result (via [result.Poster.Post]).
package runner
