# donmai

> **Status: alpha.** APIs and command flags are stabilising. See [CHANGELOG.md](./CHANGELOG.md) for the change log and [RELEASING.md](./RELEASING.md) for the release process.

`donmai` is the open-source CLI and terminal dashboard for local agent fleets. It is the single binary for every OSS operator task: running the three-process stack locally, managing agents and sessions, querying issue trackers, and inspecting fleet health.

**Binary**: `donmai`
**Module**: `github.com/RenseiAI/donmai`

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Credentials in standalone mode (no daemon, no platform)](#credentials-in-standalone-mode-no-daemon-no-platform)
- [Three-process model](#three-process-model)
- [Command catalog](#command-catalog)
  - [donmai status](#donmai-status)
  - [donmai agent](#donmai-agent)
  - [donmai session](#donmai-session)
  - [donmai daemon](#donmai-daemon)
  - [donmai governor](#donmai-governor)
  - [donmai worker and donmai fleet](#donmai-worker-and-donmai-fleet)
  - [donmai orchestrator](#donmai-orchestrator)
  - [donmai logs](#donmai-logs)
  - [donmai linear](#donmai-linear)
  - [donmai github](#donmai-github)
  - [donmai code](#donmai-code)
  - [donmai arch](#donmai-arch)
  - [donmai admin](#donmai-admin)
- [Migration from the legacy TypeScript CLI](#migration-from-the-legacy-typescript-cli)
- [Development](#development)
- [Architecture](#architecture)
- [Contribution and license](#contribution-and-license)

---

## Install

### Homebrew (macOS / Linux, recommended)

```bash
brew install RenseiAI/homebrew-tap/donmai
```

### go install (requires Go 1.26.6+)

```bash
go install github.com/RenseiAI/donmai/cmd/donmai@latest
```

### GitHub release download

Pre-built binaries for macOS (arm64, amd64) and Linux (arm64, amd64) are
attached to every release on the
[releases page](https://github.com/RenseiAI/donmai/releases).

Example for macOS arm64 (replace `0.9.4` with the version you want):

```bash
curl -fsSL https://github.com/RenseiAI/donmai/releases/download/v0.9.4/donmai_0.9.4_darwin_arm64.tar.gz \
  | tar -xz -C /usr/local/bin donmai
```

### Build from source

```bash
git clone https://github.com/RenseiAI/donmai
cd donmai
make build        # produces bin/donmai
```

---

## Quick start

```bash
# 1. Authenticate with Linear (set your API key)
export LINEAR_API_KEY=lin_api_...

# 2. Start the local daemon (persists across reboots via launchd / systemd)
donmai host install
donmai host status

# 3. Pick up Linear backlog issues and dispatch agents
donmai orchestrator --project MyProject

# 4. Watch fleet activity
donmai status
donmai agent list

# 5. Tail logs from the log analyzer
donmai logs analyze --input ~/.donmai/logs/agent.log
```

---

## Credentials in standalone mode (no daemon, no platform)

When you run `donmai` standalone (OSS mode, outside any downstream
embedder), agents inherit credentials from the donmai process. There are
two sources, in this order:

  1. Existing environment variables in the donmai process
  2. .env.local at the root of the working directory

The first source that defines a variable wins. .env.local is read once
at donmai startup and never copied into worktrees.

Some variables (the daemon's own auth tokens) are blocked from forwarding
regardless of source; see internal/credentials/blocklist.go.

If you want secrets sourced from 1Password instead of a flat file, see
the optional `op` CLI integration (run `donmai creds setup` for the
walkthrough).

---

## Three-process model

`donmai` manages three cooperating processes on your local machine. Each has a
distinct role; together they form the complete OSS execution pipeline.

```
┌──────────────────────────────────────────────────────────────────┐
│                        your machine                              │
│                                                                  │
│  ┌─────────────────┐    ┌─────────────────┐   ┌──────────────┐  │
│  │   orchestrator  │───▶│    governor     │──▶│   worker(s)  │  │
│  │  (donmai orche- │    │  (donmai govr.) │   │ (donmai wkr) │  │
│  │   ator)         │    │                 │   │              │  │
│  └─────────────────┘    └─────────────────┘   └──────────────┘  │
│           │                      │                    │          │
│     Linear API             Redis queue         coordinator HTTP  │
└──────────────────────────────────────────────────────────────────┘
```

### Orchestrator (`donmai orchestrator`)

Queries the Linear backlog, selects issues that satisfy the configured
project/work-type filters, and dispatches agent tasks into the Redis work queue.
It does not run agents itself — it schedules them. OSS users run the orchestrator
on demand or via a cron job. SaaS users replace it with the platform's webhook-
driven control plane.

### Governor (`donmai governor`)

Long-running scan loop. Watches the Redis queue for pending work, enforces
concurrency limits, and starts workers to consume each item. The governor is
the process that keeps workers running; it is the OSS equivalent of the SaaS
coordinator service.

### Worker (`donmai worker`)

An agent process. Registers with the coordinator over HTTP, polls for work,
executes the assigned session (calling the LLM runtime: Claude, Codex, etc.),
and reports results back. Multiple workers can run in parallel; the governor
controls the ceiling.

### Daemon (`donmai daemon`)

The local daemon (`rensei-daemon` subprocess) is the persistent service that
ties the three processes together. It installs as a system service (launchd on
macOS, systemd on Linux), survives reboots, manages the workarea pool, and
handles auto-updates with drain semantics. For the full daemon operations manual
see [011-local-daemon-fleet.md](https://github.com/RenseiAI/donmai-architecture/blob/main/011-local-daemon-fleet.md).

---

## Command catalog

All commands output JSON when `--json` is passed. Destructive commands require
interactive confirmation unless `--yes` is provided.

### `donmai status`

Print a fleet-wide status snapshot.

```bash
donmai status
donmai status --json
```

### `donmai agent`

Inspect and control individual agent sessions.

```bash
donmai agent list [--all] [--json] [--sandbox <id>]
donmai agent status <session-id>
donmai agent stop <session-id>
donmai agent chat <session-id>          # forward a prompt to a running agent
donmai agent reconnect <session-id>     # re-attach to a detached session
```

### `donmai session`

Low-level session management (lifecycle, streaming output).

```bash
donmai session list [--status <status>] [--limit <n>]
donmai session inspect <session-id>
donmai session stream <session-id>      # tail activity stream
donmai session restore-workarea <session-id> --to <dir>
```

### `donmai host`

Manage this machine. `host` owns the local daemon's lifecycle, this machine's
capacity envelope and workarea pool, the providers and kits installed on it, the
projects it admits work for, and the live dashboard of sessions running on it.
The daemon installs as a launchd agent (macOS) or systemd user unit (Linux) and
manages the workarea pool, auto-updates, and session lifecycle.

`donmai daemon …` still works as a hidden deprecated alias of the lifecycle
subcommands; it prints a notice on stderr and is removed in v0.58.0.

```bash
donmai host install [--user | --system]   # write and load the system service
donmai host uninstall                     # remove the system service
donmai host status                        # running / stopped / draining
donmai host stop
donmai host pause                         # stop accepting new work
donmai host resume
donmai host drain                         # wait for in-flight sessions, then stop
donmai host update                        # force-pull latest release
donmai host doctor                        # health check: config, credentials, disk
donmai host logs [--follow]               # tail daemon log (NDJSON / pretty)
donmai host stats [--pool]                # capacity, sessions, pool state
donmai host setup                         # first-run interactive wizard
donmai host set <key> <value>             # mutate a single config key
donmai host evict --repo <repo> [--older-than <duration>]
donmai host watch [--all]                 # live dashboard of this host's sessions
donmai host provider list                 # providers installed on this machine
donmai host kit list                      # kits installed on this machine
donmai host workarea list                 # this machine's workarea pool
donmai host project list                  # projects this machine admits work for
```

Supported capacity keys:

```bash
donmai host set capacity.maxConcurrentSessions <sessions>
donmai host set capacity.poolMaxDiskGb <gb>
```

Environment: `DONMAI_DAEMON_TOKEN` (optional — `donmai host install` provisions
this automatically when `~/.config/rensei/config.json` contains a platform key).

### `donmai governor`

Start, stop, and query the governor scan loop.

```bash
donmai governor start [--max <n>] [--interval <seconds>]
donmai governor stop
donmai governor status
```

### `donmai worker` and `donmai fleet` (deprecated)

Legacy local process-manager commands for standalone OSS debugging. `donmai host`
is the primary lifecycle surface for normal operation (a persistent local daemon);
`worker`/`fleet` remain available in the `donmai` binary — never in an embedding
binary — for users who need the older foreground worker process or PID-file fleet
flow. Both are marked deprecated and are removed in v0.59.0.

```bash
donmai worker start [--base-url <url>] [--provisioning-token <token>]
donmai fleet start --count <n>
donmai fleet status
donmai fleet stop
```

`fleet scale` has been removed outright (it only ever returned a
not-yet-supported error): stop and restart the fleet with a new `--count`.

### `donmai orchestrator`

Local orchestrator for OSS users. Queries the Linear backlog and dispatches
agent tasks.

```bash
donmai orchestrator --project <name>            # dispatch from a Linear project
donmai orchestrator --single <issue-id>         # process one specific issue
donmai orchestrator --project <name> --dry-run  # preview without dispatching
donmai orchestrator --project <name> --max 5    # cap concurrent dispatches
donmai orchestrator --project <name> --repo github.com/org/repo
donmai orchestrator --project <name> --templates .donmai/templates
```

**Environment**: `LINEAR_API_KEY` required.

### `donmai logs`

Agent log analysis — detect failure patterns and optionally file Linear issues.

```bash
donmai logs analyze --input /path/to/agent.log
cat agent.log | donmai logs analyze
donmai logs analyze --input agent.log --dry-run
donmai logs analyze --input agent.log --json
donmai logs analyze --input agent.log --team Engineering --project Agent
donmai logs analyze --input agent.log --config ~/.config/af/log-signatures.yaml
```

The built-in signature catalog covers: tool misuse, sandbox permission errors,
approval-required blocks, rate-limit hits, and environment failures. Override or
extend via a YAML catalog at `~/.config/af/log-signatures.yaml`.

**Environment**: `LINEAR_API_KEY` required for issue creation (omit with `--dry-run`).

### `donmai linear`

Linear issue-tracker operations (mirrors the legacy `pnpm af-linear` scripts).
All subcommands output JSON.

```bash
donmai linear get-issue <id>
donmai linear create-issue --title "..." --team "..."
donmai linear update-issue <id> [--project "<name|slug|uuid>"] [--status "..."]
donmai linear list-issues [--project "..."] [--status "..."]
donmai linear create-comment <issue-id> --body "..."
donmai linear list-comments <issue-id>
donmai linear add-relation <issue-id> <related-id> --type <related|blocks|duplicate|similar>
donmai linear list-relations <issue-id>
donmai linear remove-relation <relation-id>
donmai linear list-sub-issues <parent-id>
donmai linear list-sub-issue-statuses <parent-id>
donmai linear update-sub-issue <id> [--state "..."] [--comment "..."]
donmai linear check-blocked <issue-id>
donmai linear list-backlog-issues --project "..."
donmai linear list-unblocked-backlog --project "..."
donmai linear create-blocker <source-issue-id> --title "..."
```

`get-issue` always includes `parentId` and `parentIdentifier`. Both are JSON
strings for a child issue and explicit `null` values for a root issue.

**Authentication**: set `LINEAR_API_KEY` (or `LINEAR_ACCESS_TOKEN`).

### `donmai github`

GitHub Issues operations. Mirrors the `donmai linear` surface adapted to GitHub
Issues vocabulary. All subcommands output JSON.

```bash
donmai github get-issue     --repo owner/repo --number 42
donmai github create-issue  --repo owner/repo --title "Bug: ..." [--body "..."] [--labels "bug,enhancement"] [--assignees "alice"]
donmai github update-issue  --repo owner/repo --number 42 [--title "..."] [--state open|closed]
donmai github list-issues   --repo owner/repo [--state open|closed|all] [--labels "..."] [--assignee "alice"] [--limit 50]
donmai github list-comments --repo owner/repo --number 42
donmai github create-comment --repo owner/repo --number 42 --body "..." [--body-file /path]
donmai github add-labels    --repo owner/repo --number 42 --labels "bug,priority:high"
donmai github set-assignees --repo owner/repo --number 42 --assignees "alice,bob"
donmai github close-issue   --repo owner/repo --number 42 [--comment "Resolved in v2.0"]
donmai github reopen-issue  --repo owner/repo --number 42 [--comment "Reopening for follow-up"]
donmai github list-labels   --repo owner/repo
donmai github get-repo      --repo owner/repo
```

**Owner/repo shorthand**: `--repo owner/repo` sets both owner and repo.
`--owner` and `--repo` also read `GITHUB_OWNER` / `GITHUB_REPO` env vars.

**Authentication**: set `GITHUB_TOKEN` (personal access token, fine-grained
token, or GitHub App installation token). When running under a platform login
session, GitHub calls are proxied through the platform's connected GitHub App
installation credential instead.

### `donmai code`

Code intelligence commands, implemented natively in Go — no external binary
required by default:

```bash
donmai code get-repo-map [--max-files <n>] [--file-patterns "*.go,src/**"]
donmai code search-symbols <query> [--kinds function,method] [--file-pattern "*.go"]
donmai code search-code <query> [--language go] [--max-results <n>]
donmai code check-duplicate --content <text> | --content-file <path>
donmai code find-type-usages <TypeName> [--max-results <n>]
donmai code validate-cross-deps [path]
```

- `get-repo-map` ranks files by PageRank over the file import/dependency graph.
- `search-code` runs Okapi BM25 by default, upgrading to hybrid BM25+vector
  search when `VOYAGE_AI_API_KEY` is set, with `COHERE_API_KEY` enabling
  cross-encoder reranking.
- `check-duplicate` detects exact (xxHash64) and near (SimHash) duplicates.
- `find-type-usages` scans for switch/case, mapping-object, and import sites
  for a union type or enum.
- `validate-cross-deps` checks cross-package imports have `package.json`
  dependency declarations (monorepo JS/TS).

**Scoping**: by default the index root is the enclosing git repository root
(discovered by walking up from the current directory), not just the
invocation cwd. Pass `--repo-path <relative-path>` (a persistent flag on the
`code` command group) to scope indexing to a subtree under that root, e.g. a
single package in a monorepo.

**Override**: set `DONMAI_CODE_BIN` to force the deprecated TypeScript
exec-shim path for all subcommands instead (prints a one-time deprecation
notice to stderr; will be removed once `donmai-libraries` is archived).

### `donmai arch`

Architecture reference commands. Browse, show, and synthesize the
`donmai-architecture` corpus.

```bash
donmai arch list
donmai arch show <doc-id>                    # e.g. donmai arch show 001
donmai arch browse                           # interactive TUI browser
donmai arch synthesize --topic <topic>
donmai arch assess --topic <topic>           # gap/consistency assessment
```

### `donmai admin`

Operational admin commands for cleanup, queue inspection, and merge-queue
management. All subcommands output JSON. Destructive operations require
interactive confirmation unless `--yes` is passed.

**Environment**: `REDIS_URL` must be set for `queue` and `merge-queue` subcommands.

---

#### `donmai admin cleanup`

Prune orphaned git worktrees and stale local branches. Mirrors the TypeScript
`af-cleanup` + `af-cleanup-sub-issues` scripts.

```bash
donmai admin cleanup [flags]

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

#### `donmai admin queue`

Inspect and mutate the Redis work queue.

```bash
donmai admin queue list
donmai admin queue peek
donmai admin queue requeue <session-id> [--yes]
donmai admin queue drop <session-id> [--yes]
```

- **list** — returns all work items, sessions, and registered workers as JSON
- **peek** — shows the next item in the queue without removing it
- **requeue** — resets a session from `running`/`claimed` back to `pending` (destructive)
- **drop** — permanently removes a session and its queue/claim entries (destructive)

Example: `donmai admin queue list`:
```json
{
  "items": [
    {
      "sessionId": "sess-abc123",
      "issueIdentifier": "ENG-42",
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

#### `donmai admin merge-queue`

Inspect and mutate the Redis merge queue.

```bash
donmai admin merge-queue list [--repo <repoId>]
donmai admin merge-queue dequeue <pr-number> [--repo <repoId>] [--yes]
donmai admin merge-queue force-merge <pr-number> [--repo <repoId>] [--yes]
```

- **list** — returns all queued, failed, and blocked PRs for the repo
- **dequeue** — permanently removes a PR from the merge queue (destructive)
- **force-merge** — moves a failed/blocked PR back to the head of the queue (destructive)

The `--repo` flag defaults to `"default"`.

Example: `donmai admin merge-queue list --repo my-org/my-repo`:
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
[migration-from-legacy-cli.md](https://github.com/RenseiAI/donmai-libraries/blob/main/docs/migration-from-legacy-cli.md)
(migration guide in flight).

---

## Development

```bash
make build      # Build donmai binary  →  bin/donmai
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
`donmai` binary opts into legacy worker/fleet process-manager commands; embedders
that want the daemon-only lifecycle surface can leave those commands disabled.

See `AGENTS.md` for the full package layout and contributor guide. The
authoritative architecture corpus lives in
[donmai-architecture](https://github.com/RenseiAI/donmai-architecture) —
particularly:
- `001-layered-execution-model.md` — layered execution model and OSS contracts
- `011-local-daemon-fleet.md` — local daemon operations manual
- `013-orchestrator-and-governor.md` — orchestrator, governor, worker, dispatch loop
- `014-tui-operator-surfaces.md` — TUI display primitives and dual-surface discipline

---

## Contribution and license

Contributions welcome. Please open an issue or PR; follow the conventions in
`AGENTS.md`. The project uses the MIT license — see `LICENSE`.

See [CHANGELOG.md](./CHANGELOG.md) and [RELEASING.md](./RELEASING.md)
for the change history and release process.
