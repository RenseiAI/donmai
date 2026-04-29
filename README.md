# agentfactory-tui

Unified CLI and terminal dashboard for AgentFactory AI agent fleets.

**Binary**: `af`  
**Module**: `github.com/RenseiAI/agentfactory-tui`

## Installation

```bash
make build        # produces bin/af
```

## Commands

### `af status`

Show fleet status.

### `af agent`

Inspect and control agent sessions.

```
af agent list [--all] [--json] [--sandbox <id>]
af agent status <session-id>
af agent stop <session-id>
af agent chat <session-id>
af agent reconnect <session-id>
```

### `af session`

Low-level session management.

### `af fleet`

Manage the entire agent fleet.

### `af governor`

Start, stop, and query the governor scan loop.

### `af worker`

Start and manage fleet workers.

### `af daemon`

Start and manage the local daemon.

### `af linear`

Linear issue-tracker operations (mirrors `pnpm af-linear`). All subcommands output JSON.

```
af linear get-issue <id>
af linear create-issue --title "..." --team "..."
af linear update-issue <id> [--state "..."]
af linear list-issues [--project "..."] [--status "..."]
af linear create-comment <issue-id> --body "..."
af linear list-comments <issue-id>
af linear add-relation <issue-id> <related-id> --type <related|blocks|duplicate>
af linear list-relations <issue-id>
af linear remove-relation <relation-id>
af linear list-sub-issues <parent-id>
af linear list-sub-issue-statuses <parent-id>
af linear update-sub-issue <id> [--state "..."] [--comment "..."]
af linear check-blocked <issue-id>
af linear list-backlog-issues --project "..."
af linear list-unblocked-backlog --project "..."
af linear create-blocker <source-issue-id> --title "..."
```

**Authentication**: set `LINEAR_API_KEY` (or `LINEAR_ACCESS_TOKEN`).

### `af code`

Code intelligence commands (repo map, symbol search, etc.).

### `af arch`

Architecture reference commands.

### `af admin`

Operational admin commands — cleanup, queue inspection, and merge-queue management.

All subcommands output JSON to stdout. Destructive operations require interactive
confirmation unless `--yes` is provided.

**Environment**: `REDIS_URL` must be set for `queue` and `merge-queue` subcommands.

---

#### `af admin cleanup`

Prune orphaned git worktrees and stale local branches. Mirrors the TypeScript
`af-cleanup` + `af-cleanup-sub-issues` scripts.

```
af admin cleanup [flags]

Flags:
  --dry-run          Show what would be cleaned without removing
  --force            Force removal (includes branches with gone remotes)
  --path <dir>       Custom worktrees directory (default: ../<repoName>.wt)
  --skip-worktrees   Skip worktree cleanup
  --skip-branches    Skip branch cleanup
  --yes              Skip confirmation prompt
```

Example output:
```json
{
  "dryRun": false,
  "worktrees": {
    "scanned": 12,
    "orphaned": 3,
    "cleaned": 3,
    "skipped": 0,
    "errors": []
  },
  "branches": {
    "scanned": 5,
    "deleted": 5,
    "errors": []
  }
}
```

---

#### `af admin queue`

Inspect and mutate the Redis work queue.

```
af admin queue list
af admin queue peek
af admin queue requeue <session-id> [--yes]
af admin queue drop <session-id> [--yes]
```

- **list** — returns all work items, sessions, and registered workers as JSON
- **peek** — shows the next item in the queue without removing it
- **requeue** — resets a session from `running`/`claimed` back to `pending` (destructive: requires confirmation)
- **drop** — permanently removes a session and its queue/claim entries (destructive: requires confirmation)

Example: `af admin queue list`:
```json
{
  "items": [
    {
      "sessionId": "sess-abc123",
      "issueIdentifier": "REN-42",
      "workType": "development",
      "priority": 2,
      "queuedAt": 1714000000000
    }
  ],
  "sessions": [...],
  "workers": [...]
}
```

---

#### `af admin merge-queue`

Inspect and mutate the Redis merge queue.

```
af admin merge-queue list [--repo <repoId>]
af admin merge-queue dequeue <pr-number> [--repo <repoId>] [--yes]
af admin merge-queue force-merge <pr-number> [--repo <repoId>] [--yes]
```

- **list** — returns all queued, failed, and blocked PRs for the repo
- **dequeue** — permanently removes a PR from the merge queue (destructive: requires confirmation)
- **force-merge** — moves a failed/blocked PR back to the head of the queue (destructive: requires confirmation)

The `--repo` flag defaults to `"default"` (same convention as the TypeScript runner).

Example: `af admin merge-queue list --repo my-org/my-repo`:
```json
{
  "repoId": "my-org/my-repo",
  "depth": 2,
  "entries": [
    {
      "repoId": "my-org/my-repo",
      "prNumber": 42,
      "sourceBranch": "feature/foo",
      "priority": 1,
      "enqueuedAt": 1714000000000,
      "status": "queued"
    },
    {
      "repoId": "my-org/my-repo",
      "prNumber": 7,
      "sourceBranch": "feature/bar",
      "status": "failed",
      "failureReason": "merge conflict"
    }
  ]
}
```

---

## Development

```bash
make build      # Build af binary
make test       # go test -race ./...
make lint       # golangci-lint run
make fmt        # gofumpt -w .
make coverage   # Test with coverage report
```

## Architecture

See `AGENTS.md` for full architecture documentation and package layout.
