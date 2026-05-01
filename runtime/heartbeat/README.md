# `runtime/heartbeat/`

Per-session heartbeat to the platform `POST /api/sessions/<id>/lock-refresh` endpoint.

## What it does

```go
p, _ := heartbeat.New(heartbeat.Config{
    SessionID:  "sess-123",
    WorkerID:   "worker-1",
    IssueID:    "issue-uuid",
    BaseURL:    "https://app.rensei.ai",
    AuthToken:  bearerToken,
    HTTPClient: c,
})
_ = p.Start(ctx)
defer func() { _ = p.Stop() }()

// Runner watches this channel:
select {
case <-p.LostOwnership():
    // 3 consecutive ticks failed — abort the session
case <-doneCh:
    // session completed normally
}
```

## Failure-mode protocol (per F.1.1 §5)

- `Interval = 30s` (default), aligned with the worker-level heartbeat.
- Each tick performs **up to 3 inner HTTP attempts** with `1s/2s/4s` exponential backoff (mirrors `apiRequestWithError` in the legacy TS).
- After all inner attempts fail, the strike counter increments by 1.
- After **3 consecutive strikes** (default `StrikesUntilLost`), `LostOwnership()` channel closes; the loop exits.
- A success at any point resets the strike counter to 0.
- The first tick fires synchronously inside `Start()` so the platform mirror updates without the 30s lag.

## Endpoint shape (per F.1.1 §4 + legacy TS)

`POST /api/sessions/<id>/lock-refresh`

Body: `{"workerId":"...","issueId":"..."}` (matches `worker-runner.ts:749`).

Response: `{"refreshed": true}` (or 204 with no body).

A `refreshed: false` response is treated as a strike-eligible failure — it means the platform did not extend the lock and the session has likely been handed off.

## Distinction from worker heartbeat

This package is the **per-session** heartbeat (REN-1399). The **worker-level** heartbeat is in `daemon/heartbeat.go` and uses `POST /api/workers/<id>/heartbeat`. They have separate cadences, separate failure modes, and serve different lifecycles.

## Tests

`pulser_test.go` (httptest-driven) covers: synchronous first tick, 3-strike trip, strike reset on success, `refreshed: false` counts as failure, idempotent `Stop`, ctx-cancel ends loop, `Start` rejects double-call, request body shape + auth header, validation of required fields.
