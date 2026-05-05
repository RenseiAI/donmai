# agentfactory-tui

> **Status: alpha** — APIs and command flags are stabilising. See [CHANGELOG.md](./CHANGELOG.md) for the change log and [RELEASING.md](./RELEASING.md) for the release process.

`af` is the open-source CLI and terminal dashboard for AgentFactory AI agent fleets. It is the single binary for every OSS operator task: running the three-process stack locally, managing agents and sessions, querying Linear, and inspecting fleet health.

**Binary**: `af`
**Module**: `github.com/RenseiAI/agentfactory-tui`

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Three-process model](#three-process-model)
- [Command catalog](#command-catalog)
  - [af status](#af-status)
  - [af agent](#af-agent)
  - [af session](#af-session)
  - [af daemon](#af-daemon)
  - [af governor](#af-governor)
  - [af worker and af fleet](#af-worker-and-af-fleet)
  - [af orchestrator](#af-orchestrator)
  - [af logs](#af-logs)
  - [af linear](#af-linear)
  - [af code](#af-code)
  - [af arch](#af-arch)
  - [af admin](#af-admin)
- [Migration from the legacy TypeScript CLI](#migration-from-the-legacy-typescript-cli)
- [Development](#development)
- [Architecture](#architecture)
- [Contribution and license](#contribution-and-license)

---

## Install

### Homebrew (macOS / Linux, recommended)

```bash
brew install RenseiAI/tap/af
```

### go install (requires Go 1.22+)

```bash
go install github.com/RenseiAI/agentfactory-tui/cmd/af@latest
```

### GitHub release download

Pre-built binaries for macOS (arm64, amd64) and Linux (arm64, amd64) are
attached to every release on the
[releases page](https://github.com/RenseiAI/agentfactory-tui/releases).

```bash
# Example — macOS arm64
curl -fsSL https://github.com/RenseiAI/agentfactory-tui/releases/latest/download/af_darwin_arm64.tar.gz \
  | tar -xz -C /usr/local/bin af
```

### Build from source

```bash
git clone https://github.com/RenseiAI/agentfactory-tui
cd agentfactory-tui
make build        # produces bin/af
```

---

## Quick start

```bash
# 1. Authenticate with Linear (set your API key)
export LINEAR_API_KEY=lin_api_...

# 2. Start the local daemon (persists across reboots via launchd / systemd)
af daemon install
af daemon status

# 3. Pick up Linear backlog issues and dispatch agents
af orchestrator --project MyProject

# 4. Watch fleet activity
af status
af agent list

# 5. Tail logs from the log analyzer
af logs analyze --input ~/.rensei/logs/agent.log
```

---

## Three-process model

`af` manages three cooperating processes on your local machine. Each has a
distinct role; together they form the complete OSS execution pipeline.

```
┌──────────────────────────────────────────────────────────────────┐
│                        your machine                              │
│                                                                  │
│  ┌─────────────────┐    ┌─────────────────┐   ┌──────────────┐  │
│  │   orchestrator  │───▶│    governor     │──▶│   worker(s)  │  │
│  │  (af orchestr-  │    │  (af governor)  │   │ (af worker)  │  │
│  │   ator)         │    │                 │   │              │  │
│  └─────────────────┘    └─────────────────┘   └──────────────┘  │
│           │                      │                    │          │
│     Linear API             Redis queue         coordinator HTTP  │
└──────────────────────────────────────────────────────────────────┘
```

### Orchestrator (`af orchestrator`)

Queries the Linear backlog, selects issues that satisfy the configured
project/work-type filters, and dispatches agent tasks into the Redis work queue.
It does not run agents itself — it schedules them. OSS users run the orchestrator
on demand or via a cron job. SaaS users replace it with the platform's webhook-
driven control plane.

### Governor (`af governor`)

Long-running scan loop. Watches the Redis queue for pending work, enforces
concurrency limits, and starts workers to consume each item. The governor is
the process that keeps workers running; it is the OSS equivalent of the SaaS
coordinator service.

### Worker (`af worker`)

An agent process. Registers with the coordinator over HTTP, polls for work,
executes the assigned session (calling the LLM runtime: Claude, Codex, etc.),
and reports results back. Multiple workers can run in parallel; the governor
controls the ceiling.

### Daemon (`af daemon`)

The local daemon (`rensei-daemon` subprocess) is the persistent service that
ties the three processes together. It installs as a system service (launchd on
macOS, systemd on Linux), survives reboots, manages the workarea pool, and
handles auto-updates with drain semantics. For the full daemon operations manual
see [011-local-daemon-fleet.md](https://github.com/RenseiAI/rensei-architecture/blob/main/011-local-daemon-fleet.md).

---

## Command catalog

All commands output JSON when `--json` is passed. Destructive commands require
interactive confirmation unless `--yes` is provided.

### `af status`

Print a fleet-wide status snapshot.

```bash
af status
af status --json
```

### `af agent`

Inspect and control individual agent sessions.

```bash
af agent list [--all] [--json] [--sandbox <id>]
af agent status <session-id>
af agent stop <session-id>
af agent chat <session-id>          # forward a prompt to a running agent
af agent reconnect <session-id>     # re-attach to a detached session
```

### `af session`

Low-level session management (lifecycle, streaming output).

```bash
af session list [--status <status>] [--limit <n>]
af session inspect <session-id>
af session stream <session-id>      # tail activity stream
af session restore-workarea <session-id> --to <dir>
```

### `af daemon`

Start and manage the local daemon. The daemon installs as a launchd agent
(macOS) or systemd user unit (Linux) and manages the workarea pool, auto-
updates, and session lifecycle.

```bash
af daemon install [--user | --system]   # write and load the system service
af daemon uninstall                     # remove the system service
af daemon status                        # running / stopped / draining
af daemon start
af daemon stop
af daemon restart
af daemon pause                         # stop accepting new work
af daemon resume
af daemon drain                         # wait for in-flight sessions, then stop
af daemon update                        # force-pull latest release
af daemon doctor                        # health check: config, credentials, disk
af daemon logs [--follow]              # tail daemon log (NDJSON / pretty)
af daemon stats [--pool]               # capacity, sessions, pool state
af daemon setup                        # first-run interactive wizard
af daemon set <key> <value>            # mutate a single config key
af daemon evict --repo <repo> [--older-than <duration>]
```

Environment: `RENSEI_DAEMON_TOKEN` (optional — `af daemon install` provisions
this automatically when `~/.config/rensei/config.json` contains a platform key).

### `af governor`

Start, stop, and query the governor scan loop.

```bash
af governor start [--max <n>] [--interval <seconds>]
af governor stop
af governor status
```

### `af worker` and `af fleet`

Legacy local process-manager commands for standalone OSS debugging. `af daemon`
is the primary host lifecycle surface for normal operation, while these commands
remain available in the `af` binary for users who need the older foreground
worker host or PID-file fleet flow.

```bash
af worker start [--base-url <url>] [--provisioning-token <token>]
af fleet start --count <n>
af fleet status
af fleet stop
af fleet scale --count <n>
```

### `af orchestrator`

Local orchestrator for OSS users. Queries the Linear backlog and dispatches
agent tasks.

```bash
af orchestrator --project <name>            # dispatch from a Linear project
af orchestrator --single <issue-id>         # process one specific issue
af orchestrator --project <name> --dry-run  # preview without dispatching
af orchestrator --project <name> --max 5    # cap concurrent dispatches
af orchestrator --project <name> --repo github.com/org/repo
af orchestrator --project <name> --templates .agentfactory/templates
```

**Environment**: `LINEAR_API_KEY` required.

### `af logs`

Agent log analysis — detect failure patterns and optionally file Linear issues.

```bash
af logs analyze --input /path/to/agent.log
cat agent.log | af logs analyze
af logs analyze --input agent.log --dry-run
af logs analyze --input agent.log --json
af logs analyze --input agent.log --team Engineering --project Agent
af logs analyze --input agent.log --config ~/.config/af/log-signatures.yaml
```

The built-in signature catalog covers: tool misuse, sandbox permission errors,
approval-required blocks, rate-limit hits, and environment failures. Override or
extend via a YAML catalog at `~/.config/af/log-signatures.yaml`.

**Environment**: `LINEAR_API_KEY` required for issue creation (omit with `--dry-run`).

### `af linear`

Linear issue-tracker operations (mirrors the legacy `pnpm af-linear` scripts).
All subcommands output JSON.

```bash
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

Code intelligence commands (repo map, symbol search, BM25 + vector hybrid
search).

```bash
af code search <query>
af code map [--depth <n>]
af code symbols <file>
```

### `af arch`

Architecture reference commands. Browse, show, and synthesize the
`rensei-architecture` corpus.

```bash
af arch list
af arch show <doc-id>                    # e.g. af arch show 001
af arch browse                           # interactive TUI browser
af arch synthesize --topic <topic>
af arch assess --topic <topic>           # gap/consistency assessment
```

### `af admin`

Operational admin commands for cleanup, queue inspection, and merge-queue
management. All subcommands output JSON. Destructive operations require
interactive confirmation unless `--yes` is passed.

**Environment**: `REDIS_URL` must be set for `queue` and `merge-queue` subcommands.

---

#### `af admin cleanup`

Prune orphaned git worktrees and stale local branches. Mirrors the TypeScript
`af-cleanup` + `af-cleanup-sub-issues` scripts.

```bash
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

```bash
af admin queue list
af admin queue peek
af admin queue requeue <session-id> [--yes]
af admin queue drop <session-id> [--yes]
```

- **list** — returns all work items, sessions, and registered workers as JSON
- **peek** — shows the next item in the queue without removing it
- **requeue** — resets a session from `running`/`claimed` back to `pending` (destructive)
- **drop** — permanently removes a session and its queue/claim entries (destructive)

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

```bash
af admin merge-queue list [--repo <repoId>]
af admin merge-queue dequeue <pr-number> [--repo <repoId>] [--yes]
af admin merge-queue force-merge <pr-number> [--repo <repoId>] [--yes]
```

- **list** — returns all queued, failed, and blocked PRs for the repo
- **dequeue** — permanently removes a PR from the merge queue (destructive)
- **force-merge** — moves a failed/blocked PR back to the head of the queue (destructive)

The `--repo` flag defaults to `"default"`.

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

## Migration from the legacy TypeScript CLI

If you are moving from the previous TypeScript-based `pnpm af-*` scripts, see
[migration-from-legacy-cli.md](https://github.com/RenseiAI/agentfactory/blob/main/docs/migration-from-legacy-cli.md)
(REN-1365 in flight).

---

## Development

```bash
make build      # Build af binary  →  bin/af
make test       # go test -race ./...
make lint       # golangci-lint run
make fmt        # gofumpt -w .
make vuln       # govulncheck ./...
make coverage   # Test with coverage report
make run-mock        # Run TUI dashboard with mock data
make run-status-mock # Run status with mock data
```

---

## Architecture

The public library surface (`afclient`, `afcli`, `worker`) is designed to be
imported by downstream consumers. Embedders use `afcli.RegisterCommands` and
extend the generic OSS command set with their own subcommands. The standalone
`af` binary opts into legacy worker/fleet process-manager commands; embedders
that want the daemon-only lifecycle surface can leave those commands disabled.

See `AGENTS.md` for the full package layout and contributor guide. The
authoritative architecture corpus lives in
[rensei-architecture](https://github.com/RenseiAI/rensei-architecture) —
particularly:
- `001-layered-execution-model.md` — OSS / SaaS boundary and the `af` ↔ `rensei` contract
- `011-local-daemon-fleet.md` — local daemon operations manual
- `013-orchestrator-and-governor.md` — orchestrator, governor, worker, dispatch loop
- `014-tui-operator-surfaces.md` — TUI display primitives and dual-surface discipline

---

## Contribution and license

Contributions welcome. Please open an issue or PR; follow the conventions in
`AGENTS.md`. The project uses the MIT license — see `LICENSE`.

See [CHANGELOG.md](./CHANGELOG.md) and [RELEASING.md](./RELEASING.md)
for the change history and release process.
