# `af queue` — Work Queue Management

Port from legacy `af-queue-admin` (queue-admin.ts + lib/queue-admin-runner.ts).

**Requires**: `REDIS_URL`.

## Issues

### AF-040: Implement `af queue list` subcommand
**Priority**: High
**Labels**: migration

List items in the work queue. Show: position, issue ID, work type, priority, queued time.

### AF-041: Implement `af queue sessions` subcommand
**Priority**: High
**Labels**: migration

List active sessions tracked by the queue. Show: session ID, status, worker, duration.

### AF-042: Implement `af queue workers` subcommand
**Priority**: Medium
**Labels**: migration

List registered workers and their claims. Show: worker ID, hostname, capacity, active claims.

### AF-043: Implement `af queue clear` subcommand
**Priority**: Medium
**Labels**: migration

Clear queue items. Support `--claims` (clear stale claims), `--queue` (clear pending queue), `--all` (everything).

### AF-044: Implement `af queue reset` subcommand
**Priority**: Low
**Labels**: migration

Full queue reset — clears all queue state, sessions, and claims. Requires `--confirm` flag.

### AF-045: Implement `af queue remove` subcommand
**Priority**: Medium
**Labels**: migration

Remove a specific item from the queue by ID. `af queue remove <id>`.
