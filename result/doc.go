// Package result posts an [agent.Result] back to the Rensei platform
// at the end of a session.
//
// Per F.1.1 §6, the wire contract is:
//
//  1. POST /api/sessions/<id>/completion — the canonical "session done"
//     endpoint. The platform side resolves the Linear comment text from
//     the runner-supplied summary and posts it via the platform's
//     getLinearClient resolver. Worker auth: runtime_jwt | registration
//     token | legacy WORKER_API_KEY.
//
//  2. POST /api/sessions/<id>/status — the FSM status transition
//     ("completed" | "failed" | "stopped") with the cost snapshot,
//     provider session id, and worktree path. Drives Linear status
//     transitions, fleet quota release, governor phase tracking, file
//     reservation release, and the lifecycle hook chain
//     (audit + session-status + LLM-billing + event publisher) on the
//     platform side.
//
// Both calls are wrapped by the legacy `apiRequestWithError` retry
// pattern: 3 attempts with exponential backoff (1s, 2s, 4s) on
// transient failures. 4xx responses are treated as permanent —
// retrying a 401 / 403 / 404 / 422 just papers over a programmer error.
//
// # Boundaries
//
//   - Posts to the platform only. Does NOT post directly to Linear; the
//     platform's completion + status handlers own the Linear-side
//     reflection (REN-1399 tenant scoping requires it).
//   - Knows nothing about how the Result was produced. The runner
//     hands it a fully-populated [agent.Result]; this package only
//     translates that to the wire shape.
//
// # Retry semantics
//
// Transient errors (network failures, 5xx responses, context-deadline
// timeouts) trigger the 3-attempt exponential backoff. Permanent
// errors (4xx) return immediately wrapped in a [PermanentError]. The
// caller (runner) typically logs and moves on either way — a failed
// completion is operationally bad but not worth crashing the daemon
// over.
//
// Source: F.1.1 §6, F.0.1 §5 ("Result"), legacy
// ../agentfactory/packages/cli/src/lib/worker-runner.ts::reportStatus
// + ../agentfactory/packages/core/src/orchestrator/orchestrator.ts
// (completion fetch).
package result
