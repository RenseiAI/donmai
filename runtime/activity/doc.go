// Package activity pushes per-session [agent.Event] values to the
// platform's /api/sessions/<id>/activity endpoint asynchronously.
//
// The poster is a fire-and-forget seam: the runner Send()s every event
// it observes, the poster maps it to the platform's activity wire shape
// (thought / action / response / error / context), and a background
// worker drains the queue with bounded retries. Send() never blocks —
// when the queue is full events are dropped with a warn log so the
// runner cannot stall on platform I/O.
//
// Endpoint contract: POST /api/sessions/<id>/activity. Body:
//
//	{
//	  "workerId": "wkr_xxx",
//	  "activity": {
//	    "type": "thought" | "action" | "response" | "error" | "context",
//	    "content": "string",
//	    "toolName": "?optional",
//	    "toolInput": {"...": "?optional object"},
//	    "toolCategory": "?optional",
//	    "toolOutput": "?optional",
//	    "timestamp": "?ISO 8601 — server defaults"
//	  }
//	}
//
// Auth: Bearer runtime token (refreshed via [Config.CredentialProvider]
// — same model as [github.com/RenseiAI/agentfactory-tui/runtime/heartbeat]).
//
// Side effect: on the first successful activity POST the poster also
// fires a single best-effort POST /api/sessions/<id>/status with
// {"status":"running","workerId":"..."} to transition the platform-side
// session row from "claimed" to "running". This is gated by an
// atomic.Bool — terminal status is owned by [result.Poster].
//
// Per-session lifecycle: build one Poster per [Runner.Run] (use the
// queued-work session id), call Start, defer Stop. Stop blocks for a
// short drain window before exiting.
package activity
