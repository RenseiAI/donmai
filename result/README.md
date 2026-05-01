# result

Posts an `agent.Result` back to the Rensei platform when a session
finishes, in the order locked by F.1.1 §6:

1. `POST /api/sessions/<id>/completion` — canonical "session done"
   hook. The platform side resolves the Linear comment text from the
   runner-supplied `summary` and posts it via the platform's
   `getLinearClient` resolver.
2. `POST /api/sessions/<id>/status` — FSM status transition
   (`completed` | `failed` | `stopped`) with cost snapshot, provider
   session id, and worktree path. Triggers the cleanup chain (release
   claim, archive inbox, release issue lock, promote next pending
   work) and the lifecycle-hook chain on the platform side.

Both endpoints are worker-auth (`runtime_jwt` | `registration_token` |
legacy `WORKER_API_KEY`). The Bearer token is sent unmodified.

## Usage

```go
import (
    "context"

    "github.com/RenseiAI/agentfactory-tui/agent"
    "github.com/RenseiAI/agentfactory-tui/result"
)

p, err := result.NewPoster(result.Options{
    PlatformURL: "https://app.rensei.ai",
    AuthToken:   runtimeJWT,
    WorkerID:    "wkr_xxx",
})
if err != nil { /* ... */ }

err = p.Post(ctx, sessionID, agent.Result{
    Status:            "completed",
    ProviderName:      agent.ProviderClaude,
    ProviderSessionID: "claude-sess-uuid",
    WorktreePath:      "/tmp/wt/REN-123-DEV",
    PullRequestURL:    "https://github.com/owner/repo/pull/42",
    Summary:           "Implemented X, opened PR.",
    WorkResult:        "passed",
    Cost: &agent.CostData{
        InputTokens:  1234,
        OutputTokens: 567,
        TotalCostUsd: 0.0123,
    },
})
```

## Retry semantics

Both calls are wrapped by a 3-attempt exponential-backoff helper —
`baseDelay << (attempt-1)` defaults to 1s, 2s, 4s. The shape is the
verbatim port of the legacy `apiRequestWithError` from
`../agentfactory/packages/cli/src/lib/worker-runner.ts`.

| Failure                                     | Behavior                                         |
|---------------------------------------------|--------------------------------------------------|
| Network error / DNS / connect refused / timeout | Retry up to `MaxAttempts`, then `TransientError`. |
| 5xx response                                | Retry up to `MaxAttempts`, then `TransientError`. |
| 4xx response                                | Return `PermanentError` immediately, no retry.    |
| Caller `ctx.Cancel`/`ctx.Deadline`          | Return the context error immediately.             |

A `PermanentError` on `/completion` does NOT prevent the
`/status` post — the runner still wants to release the session lock so
the next worker can pick up. When both calls fail, `errors.Join`
combines them so downstream logs see the full picture.

## Boundaries

- Posts to the platform only. Does NOT post directly to Linear; the
  platform's completion + status handlers own the Linear-side
  reflection (REN-1399 tenant scoping requires it).
- Knows nothing about how the `agent.Result` was produced. The runner
  hands it a fully-populated value; this package only translates that
  to the wire shape.

## References

- F.1.1 §6 (Result Posting) — locked endpoint + retry contract
- F.0.1 §5 — legacy result shape
- Live platform routes verified at
  `platform/src/app/api/sessions/[id]/{completion,status}/route.ts`
  (Phase 2a port of the legacy agentfactory-nextjs handlers).
