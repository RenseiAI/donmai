# donmai

> **Status: alpha.** APIs and command flags are stabilising. See [CHANGELOG.md](./CHANGELOG.md) for the change log and [RELEASING.md](./RELEASING.md) for the release process.

`donmai` is the open-source agent runtime CLI and terminal dashboard for local
agent fleets. The single binary runs the local daemon, dispatches work, manages
agent sessions, and exposes code intelligence without a hosted control plane.

**Binary**: `donmai`
**Module**: `github.com/RenseiAI/donmai`

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Standalone credentials](#standalone-credentials)
- [Local execution model](#local-execution-model)
- [Command catalog](#command-catalog)
  - [donmai status](#donmai-status)
  - [donmai agent](#donmai-agent)
  - [donmai session](#donmai-session)
  - [donmai host](#donmai-host)
  - [donmai governor](#donmai-governor)
  - [Legacy worker and fleet aliases](#legacy-worker-and-fleet-aliases)
  - [donmai orchestrator](#donmai-orchestrator)
  - [donmai logs](#donmai-logs)
  - [donmai linear](#donmai-linear)
  - [donmai github](#donmai-github)
  - [donmai code](#donmai-code)
  - [donmai arch](#donmai-arch)
  - [donmai admin](#donmai-admin)
- [Development](#development)
- [Architecture](#architecture)
- [Contribution and license](#contribution-and-license)

---

## Install

### Homebrew (macOS, recommended)

```bash
brew install RenseiAI/homebrew-tap/donmai
```

On Linux, use Go installation or a release archive below.

### go install (requires Go 1.26.6+)

```bash
go install github.com/RenseiAI/donmai/cmd/donmai@v0.72.52
```

Use the explicit release pin: the public Go proxy currently resolves `@latest`
to the historical `v1.0.0` module. This source-install command records the pinned
module version in Go build information and reports `dev` for `donmai --version`.

### GitHub release download

Pre-built binaries for macOS (arm64, amd64) and Linux (arm64, amd64) are
attached to every release on the
[releases page](https://github.com/RenseiAI/donmai/releases).

Example for macOS arm64, pinned to v0.72.52. See the releases page for newer
versions:

```bash
mkdir -p "$HOME/.local/bin"
curl -fsSL https://github.com/RenseiAI/donmai/releases/download/v0.72.52/donmai_0.72.52_darwin_arm64.tar.gz \
  | tar -xz -C "$HOME/.local/bin" donmai
"$HOME/.local/bin/donmai" --version
```

### Build from source

```bash
git clone https://github.com/RenseiAI/donmai
cd donmai
make build        # produces bin/donmai
```

---

## Quick start

### GitHub issue to local agent

This path uses a local file queue and a GitHub repository. You need permission
to push branches and open pull requests there, plus an installed, signed-in
Claude Code or Codex CLI. It does not require Linear or a hosted Donmai account.
On macOS, this example uses Claude Code:

```bash
brew install RenseiAI/homebrew-tap/donmai gh
brew install --cask claude-code
claude --version                 # Sonnet 5 profiles need 2.1.197 or newer
claude auth login                # use a Claude.ai subscription login
gh auth login --hostname github.com --git-protocol https
gh auth setup-git --hostname github.com
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
```

For Codex, install its CLI and use a file-backed ChatGPT login instead:

```bash
codex -c 'cli_auth_credentials_store="file"' login
codex -c 'cli_auth_credentials_store="file"' login status
```

The Codex status should say `Logged in using ChatGPT`. Setup offers only
installed profiles whose local login and model-version checks pass. Those
checks do not verify account entitlement or a successful model turn. Keep
`GITHUB_TOKEN` in the shell running Donmai so it can verify the repository,
read issues, and publish authenticated session receipts.

From a clone of the GitHub repository, choose **2. Local file queue** in the
wizard. Select the native profile, enter `OWNER/REPO`, choose an issue label
(for example, `donmai`), and confirm the base branch for new pull requests.
The wizard verifies the repository and branch and displays the local execution
security policy before saving it.

```bash
cd /path/to/your/repository
donmai host setup
donmai host run
```

Leave `host run` open for this foreground run. In another terminal, make the
GitHub token available again, label a small open issue with a clear task and
acceptance check, and watch the local sessions:

```bash
export GITHUB_TOKEN="$(gh auth token --hostname github.com)"
donmai github add-labels --repo OWNER/REPO --number 42 --labels donmai
donmai host watch --all
```

Replace `OWNER/REPO` and `42` with your repository and issue number. If the
label does not exist, create it with `gh label create donmai --repo OWNER/REPO`
before adding it to the issue. `host watch` is a live session view. If a run
opens a pull request and completes, review the pull request and its Donmai
session receipt comment on GitHub before merging. If no work starts or no
pull request appears, check `donmai host logs`
in another terminal. An issue label starts intake; it does not guarantee a
successful agent run or pull request. Authentication failures are failures;
Donmai does not automatically switch to another model or provider. Each issue
is admitted once per local queue: relabeling a failed issue does not retry it.
Keep the failed issue and its receipt for diagnosis. An unresolved recovered
session appears as **held**, with no claim of live work; completed or failed
results and verified publication receipts remain in the local session record.

### Persistent host

For a persistent local host, use the setup wizard, install the service, and
read its status through the loopback daemon API:

```bash
donmai host setup
donmai host install
donmai host status
donmai host doctor
donmai host stats
donmai host logs
```

The standalone `orchestrator` is a separate path that starts an installed
Claude or Codex CLI directly, without the daemon. In a Git checkout, set
`LINEAR_API_KEY`, replace `MyProject` with your Linear project name, then
preview before dispatching:

```bash
donmai orchestrator --project MyProject --dry-run
donmai orchestrator --project MyProject
```

---

## Standalone credentials

When `donmai` runs without an external credential pipeline, the local daemon
can pass credentials to its agent children from two sources, in this order:

  1. Existing environment variables in the donmai process
  2. `.env.local` at the root of the current Git repository

The first source that defines a variable wins. .env.local is read once
at donmai startup and never copied into worktrees.

The daemon's own authentication variables are blocked from forwarding regardless
of source. See [standalone credential handling](./docs/agents/CREDENTIALS-STANDALONE.md)
for the exact precedence and blocklist.

If you want secrets sourced from 1Password instead of a flat file, see
the optional `op` CLI integration (run `donmai creds setup` for the
walkthrough).

---

## Local execution model

`donmai host` manages the persistent daemon on this machine. It owns the local
session pool, workarea cache, provider setup, and host health. The same binary
offers a separate `donmai orchestrator` path that selects Linear backlog work
and starts a Claude or Codex process directly, plus `donmai governor` for a
configured scan loop. The old `worker` and `fleet` process-manager groups remain
compatibility paths, not the normal host setup. See the
[local daemon architecture](https://github.com/RenseiAI/donmai-architecture/blob/main/011-local-daemon-fleet.md)
for the runtime model.

---

## Command catalog

Flags and output formats vary by command. Use `donmai <command> --help` before
automating a mutation.

`status`, `agent`, and `session` query the configured API URL. For the daemon on
this machine, use `host status`, `host stats`, and `host watch`.

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
donmai agent chat <session-id> <message> # forward a prompt to a running agent
donmai agent reconnect <session-id>     # reconnect to an orphaned session
```

### `donmai session`

Low-level session management (lifecycle, streaming output).

```bash
donmai session list [--all] [--json] [--sandbox <id>]
donmai session show <session-id>
donmai session stream <session-id>      # tail activity stream
```

### `donmai host`

Manage this machine. `host` owns the local daemon's lifecycle, this machine's
capacity envelope and workarea pool, the providers and kits installed on it, the
projects it admits work for, and the live dashboard of sessions running on it.
The daemon installs as a launchd agent (macOS) or systemd user unit (Linux) and
manages the workarea pool and session lifecycle.

`donmai daemon …` still resolves in v0.72.47 as a hidden deprecated lifecycle
alias. It prints a notice on stderr when invoked; use `host` for new scripts.

```bash
donmai host install                       # macOS launchd or Linux user service
donmai host uninstall                     # remove the system service
donmai host status                        # running / stopped / draining
donmai host stop
donmai host pause                         # stop accepting new work
donmai host resume
donmai host drain                         # drain work; keep the daemon resumable
donmai host doctor                        # health check: config, credentials, disk
donmai host logs [--follow]               # tail daemon log (NDJSON / pretty)
donmai host stats [--pool]                # capacity, sessions, pool state
donmai host setup                         # first-run interactive wizard
donmai host set <key> <value>             # mutate a single config key
donmai host watch [--all]                 # live dashboard of this host's sessions
donmai host provider list                 # providers installed on this machine
donmai host kit list                      # kits installed on this machine
donmai host workarea list                 # this machine's workarea pool
donmai host project list                  # projects this machine admits work for
```

By default, the daemon has no update source configured and does not support
manual pool eviction. `donmai host update` can acknowledge a request without
downloading or installing a new binary and leave the host `draining`; use
`donmai host resume` to restore
admission if that happens. `donmai host evict` returns HTTP 501 and evicts
nothing in the stock daemon.

On Linux, `host install --system` selects a system-scoped unit and requires
administrator privileges; `--user` selects the user-scoped unit.

Supported capacity keys:

```bash
donmai host set capacity.maxConcurrentSessions <sessions>
donmai host set capacity.poolMaxDiskGb <gb>
```

Use `donmai host setup` to configure the work source, daemon settings, and
resource limits before installing the service.

### `donmai governor`

The governor scans named Linear projects and enqueues eligible issues in Redis;
execution requires a separate worker. Set `LINEAR_API_KEY` and a `REDIS_URL`
that reaches Redis before starting it. Pass a Linear project name with
`--project`, or set `GOVERNOR_PROJECTS` to a comma-separated list of project
names. `status` reports whether the recorded governor process is running.

```bash
donmai governor start --project "<project-name>" [--max-dispatches <n>] [--scan-interval <duration>]
donmai governor stop
donmai governor status
```

### Legacy worker and fleet aliases

`donmai worker` and `donmai fleet` still resolve in v0.72.47 for older local
process-manager scripts. Both are deprecated and omitted from top-level help;
new setups should use `donmai host`. The old `fleet scale` stub is absent.

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
donmai logs analyze --input agent.log --config ~/.config/donmai/log-signatures.yaml
```

The built-in signature catalog covers: tool misuse, sandbox permission errors,
approval-required blocks, rate-limit hits, and environment failures. Override or
extend via a YAML catalog at `~/.config/donmai/log-signatures.yaml`.

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
donmai linear list-backlog-issues --project "..." --statuses Backlog
donmai linear list-unblocked-backlog --project "..." --statuses Backlog
donmai linear create-blocker <source-issue-id> --title "..."
```

The two backlog grooming helpers default to top-level `Icebox` issues. Pass
`--statuses Backlog` to select the project's prioritized Backlog state; their
default `--parents-only` filter excludes sub-issues.

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

Assess a GitHub pull request for architectural drift with native diff analysis.
Pass a PR URL or `--repository` with `--pr`. With `gh` available, the command
reads the PR diff; without it, the default warns and returns metadata-only
output. Use `--require-diff` for checks that need a complete diff: it returns
exit code 2 when that diff is unavailable. A triggered policy returns exit code 1.

```bash
donmai arch assess https://github.com/RenseiAI/donmai/pull/667 --summary
donmai arch assess --repository github.com/RenseiAI/donmai --pr 667 --require-diff
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
`donmai` binary retains hidden, deprecated worker/fleet process-manager
commands for older scripts. Embedders leave those optional commands disabled
by default.

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
