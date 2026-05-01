// Package heartbeat sends per-session ownership-refresh pings to the
// platform and emits a 3-strike LostOwnership event when the platform
// stops accepting them.
//
// This is distinct from the worker-level heartbeat in
// daemon/heartbeat.go: that one keeps the daemon's worker registration
// alive (POST /api/workers/<id>/heartbeat); this one refreshes a
// specific session's ownership lock (POST /api/sessions/<id>/lock-refresh)
// and is started+stopped per session by the runner.
//
// Endpoint: POST /api/sessions/<id>/lock-refresh — verbatim from the
// legacy TS worker-runner.ts:749. The body is
// {"workerId": "...", "issueId": "..."} and the response shape is
// {"refreshed": bool}. The platform applies a TTL refresh on the
// underlying issue lock; missing one tick is recoverable, missing
// three signals the platform has handed the session to another worker.
//
// Failure-mode protocol (per F.1.1 §5):
//   - 3 consecutive failures (any source: HTTP error, non-2xx, refresh
//     failure) emit a LostOwnership event the runner consumes.
//   - The strike counter resets on any successful refresh.
//   - Each HTTP attempt is wrapped in the standard 3-attempt exponential
//     backoff (1s, 2s, 4s) used by every platform-API call in the
//     runner; that retry happens *inside* one tick, so the 3-strike
//     count is over outer ticks (default 30s apart), not inner retries.
//
// Configuration:
//   - Interval defaults to 30 seconds (matching the legacy 60s ttl
//     refresher cadence; F.1.1 calls for 30s to align with the worker
//     heartbeat).
//   - The first tick fires synchronously inside Start() so the
//     platform mirror updates immediately on session start without a
//     30s lag.
//   - Stop() is idempotent and safe to call from a deferred runner
//     cleanup path.
package heartbeat
