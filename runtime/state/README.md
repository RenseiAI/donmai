# `runtime/state/`

`.agent/state.json` persistence for crash recovery.

## What it does

```go
store := state.NewStore()

// Read on session start (or recovery).
st, err := store.ReadExpect(worktree, "REN-1234")
switch {
case errors.Is(err, state.ErrNotFound):           // fresh worktree
case errors.Is(err, state.ErrIdentifierMismatch): // refuse cross-issue reuse
case errors.Is(err, state.ErrMalformed):          // overwrite with fresh
}

// Update under per-worktree mutex; safe under contention.
_, err = store.Update(worktree, func(st *state.State) error {
    st.CurrentStep = "spawning"
    st.AttemptCount++
    return nil
})
```

## File layout

```
<worktree>/
└── .agent/
    └── state.json
```

Atomic write: `state.json.tmp-XXXX` is written + fsync'd, then renamed over `state.json`. A crash mid-write cannot leave a half-written file.

## Schema

| Field | Purpose |
|---|---|
| `issueId`, `issueIdentifier` | Linear identifiers; identifier is the cross-issue recovery guard. |
| `sessionId` | Platform session UUID. |
| `providerName`, `providerSessionId` | Which provider ran; native session id from `agent.InitEvent`. |
| `workType`, `currentStep` | Runner-level orchestration phase. |
| `attemptCount` | Increment-on-retry counter. |
| `startedAt`, `lastUpdatedAt`, `lastHeartbeat` | Unix-ms timestamps (matches legacy TS `Date.now()`). |
| `pid`, `workerId` | Provider subprocess pid, owning worker. |

The shape mirrors the legacy TS `WorktreeState` (`../../../agentfactory/packages/core/src/orchestrator/state-types.ts`) closely enough that the two readers can co-exist during the F.0/F.5 migration window.

## Concurrency

`Store.Update` serializes through a per-worktree `sync.Mutex` lazily attached on first access. Different worktrees proceed in parallel.

## Tests

`store_test.go` covers: not-found / roundtrip / Update creation + increment, 50-way concurrent contention (final count exact), malformed recovery, identifier mismatch returns loaded state for forensics, atomic write leaves no `*.tmp-*` leftovers.
