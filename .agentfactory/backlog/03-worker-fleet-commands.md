# `af worker` + `af fleet` — Worker Management

Port from legacy `af-worker` (worker.ts) and `af-worker-fleet` (worker-fleet.ts).

**Requires**: `WORKER_API_URL`, `WORKER_API_KEY`. Optional: `LINEAR_API_KEY`, `WORKER_PROJECTS`.

## Issues

### AF-030: Implement `af worker start` subcommand
**Priority**: High
**Labels**: migration

Start a single worker process that polls the coordinator for queued work. Support flags:
- `--capacity` — max concurrent agents (default: 3)
- `--hostname` — worker hostname
- `--api-url` — coordinator URL
- `--api-key` — worker API key
- `--projects` — comma-separated project filter
- `--dry-run` — log what would happen without executing

### AF-031: Implement `af worker status` subcommand
**Priority**: Medium
**Labels**: migration

Show worker status: connected/disconnected, capacity, active agents, projects.

### AF-032: Implement `af fleet start` subcommand
**Priority**: High
**Labels**: migration

Start a fleet of worker processes. Auto-detect CPU cores (default: cores/2). Support flags:
- `-w`/`--workers` — worker count (or `WORKER_FLEET_SIZE`)
- `-c`/`--capacity` — agents per worker (or `WORKER_CAPACITY`)
- `-p`/`--projects` — project filter (or `WORKER_PROJECTS`)
- `--auto-update` — enable auto-update checking
- `--dry-run`

### AF-033: Implement `af fleet stop` subcommand
**Priority**: High
**Labels**: migration

Stop all fleet workers. Graceful shutdown with SIGTERM, force after timeout.

### AF-034: Implement `af fleet status` subcommand
**Priority**: Medium
**Labels**: migration

Show fleet-wide status: worker count, total capacity, active agents, per-worker breakdown.

### AF-035: Implement `af fleet scale` subcommand
**Priority**: Low
**Labels**: migration

Dynamically scale fleet up or down. `af fleet scale 8` adjusts to 8 workers.
