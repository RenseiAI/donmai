# `af merge-queue` — Merge Queue Management

Port from legacy `af-merge-queue` (merge-queue.ts).

**Requires**: `REDIS_URL`.

## Issues

### AF-050: Implement `af merge-queue status` subcommand
**Priority**: Medium
**Labels**: migration

Show merge queue status: paused/active, items in queue, current processing item.

### AF-051: Implement `af merge-queue list` subcommand
**Priority**: Medium
**Labels**: migration

List items in the merge queue with their position, PR link, status, and retry count. Support `--repo` flag.

### AF-052: Implement `af merge-queue retry` subcommand
**Priority**: Medium
**Labels**: migration

Retry a failed merge queue item. `af merge-queue retry <id> --repo <repoId>`.

### AF-053: Implement `af merge-queue skip` subcommand
**Priority**: Medium
**Labels**: migration

Skip a stuck item in the merge queue. `af merge-queue skip <id> --repo <repoId>`.

### AF-054: Implement `af merge-queue pause/resume` subcommands
**Priority**: Medium
**Labels**: migration

Pause or resume merge queue processing. `af merge-queue pause --repo <repoId>`.

### AF-055: Implement `af merge-queue priority` subcommand
**Priority**: Low
**Labels**: migration

Adjust priority of an item in the merge queue. `af merge-queue priority <id> <priority> --repo <repoId>`.
