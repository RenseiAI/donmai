# `af agent` — Session Management

Port from legacy `af-agent` (agent.ts + lib/agent-runner.ts). Manages running agent sessions via Redis.

**Requires**: `REDIS_URL` env var. Optional: `LINEAR_API_KEY` for reconnect.

## Issues

### AF-010: Define agent session API types
**Priority**: High
**Labels**: migration

Create types for agent session list, detail, stop, chat, and reconnect operations in `internal/api/types.go`. Model after legacy runner's data structures.

### AF-011: Implement `af agent list` subcommand
**Priority**: High
**Labels**: migration

List active agent sessions. Support `--all` flag to include completed/failed. Display: session ID, issue identifier, status, duration, work type.

### AF-012: Implement `af agent stop` subcommand
**Priority**: High
**Labels**: migration

Stop a running agent session by identifier. `af agent stop SUP-674`. Send graceful shutdown signal.

### AF-013: Implement `af agent status` subcommand
**Priority**: Medium
**Labels**: migration

Show detailed status for a specific agent session. Display: status, duration, tokens used, cost, current activity.

### AF-014: Implement `af agent chat` subcommand
**Priority**: Medium
**Labels**: migration

Send a message to a running agent. `af agent chat SUP-674 "your message"`. Uses forward-prompt API.

### AF-015: Implement `af agent reconnect` subcommand
**Priority**: Low
**Labels**: migration

Reconnect to an orphaned agent session. Requires `LINEAR_API_KEY`.
