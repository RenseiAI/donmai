# Credentials in standalone mode (no daemon supervisor, no control plane)

Routed here from `AGENTS.md` when you touch credential or env handling.

When `donmai` runs standalone (no external credential pipeline), agents inherit
credentials from the donmai process per a fixed two-tier precedence:

| Precedence | Source                              | Notes                                                              |
| ---------- | ----------------------------------- | ------------------------------------------------------------------ |
| 1          | donmai process env (`os.Environ()`) | Anything the operator `export`'d before launching `donmai`.         |
| 2          | `${gitRoot}/.env.local`             | Parsed once at donmai startup; never copied into spawned worktrees. |
| Fail-open  | Redacted stderr warning             | `[creds] no source for KEY — agent may fail` per missing variable.  |

Sources are merged at `daemon run` time into `SpawnerOptions.BaseEnv` so the
standard child-spawn path picks them up. Process env wins over `.env.local`, and
any caller-supplied `BaseEnv` entry (set by daemon code, not the operator) wins
over both.

`AGENT_ENV_BLOCKLIST` is the single source of truth in
`internal/credentials/blocklist.go`. It captures the daemon's own auth surface —
`DONMAI_DAEMON_JWT`, `WORKER_API_KEY`, `M2M_JWT_SECRET`, … — that must never
bleed into a child agent regardless of source. Downstream embedders hardcode the
same list on their side; the copies stay in sync manually until the module
boundary permits a shared import — treat any blocklist edit as a cross-repo
change and say so in your report.

Operators can pin the mode via `donmai daemon run --standalone-creds=<on|off|auto>`.
Default `auto` selects `on` when `DONMAI_DAEMON_JWT` is unset (donmai is NOT
being driven by an external credential socket) and `off` otherwise.

Security guardrails (all implemented — keep them true):

- `.env.local` is never copied into a worktree — the file stays at `${gitRoot}`;
  only parsed values live in process memory and forward through child env.
- `.env.local` paths resolve from `gitRoot` only; no parent-directory walking.
- World-readable `.env.local` triggers a one-time stderr warning recommending
  `chmod 600`; the file is still parsed.
- Values are never echoed to stdout/stderr/logs — log lines reference variable
  names only.
- Malformed `.env.local` lines are non-fatal: name + line number logged, value
  dropped.
