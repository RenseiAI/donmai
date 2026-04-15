# `af governor` — Workflow Governance

Port from legacy `af-governor` (governor.ts + lib/governor-runner.ts). Automated workflow scan loop with configurable triggers.

**Requires**: `LINEAR_API_KEY`, `REDIS_URL`. Optional: `GOVERNOR_PROJECTS`.

## Issues

### AF-020: Implement `af governor start` subcommand
**Priority**: High
**Labels**: migration

Start the governor process. Support flags:
- `--project` (repeatable) — project names to govern
- `--scan-interval` — polling interval
- `--max-dispatches` — concurrent dispatch limit
- `--once` — single scan then exit
- `--mode` — `event-driven` (default) or `poll-only`
- Feature toggles: `--no-auto-research`, `--no-auto-backlog-creation`, `--no-auto-development`, `--no-auto-qa`, `--no-auto-acceptance`
- `--foreground` / background with PID tracking

### AF-021: Implement `af governor stop` subcommand
**Priority**: High
**Labels**: migration

Stop running governor process. SIGTERM with grace period, then SIGKILL. Clean up PID file.

### AF-022: Implement `af governor status` subcommand
**Priority**: Medium
**Labels**: migration

Show governor status: running/stopped, uptime, projects being governed, last scan time, dispatch count.
