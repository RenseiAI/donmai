# donmai

Unified CLI and terminal dashboard for Donmai AI agent fleets.

**Module**: `github.com/RenseiAI/donmai`

## Purpose

`donmai` is the single binary for operating Donmai. The goal is for every AgentFactory capability — running fleets, managing agents, scanning Linear for work, dispatching via Redis, inspecting status — to be available through this one binary. The only operations outside scope are ones that are inherently server-side (e.g., the coordinator's own HTTP service).

This project is taking over the CLI surface from the older TypeScript AgentFactory project. Functionality is being ported into Go in this repo: governor scan loop, Linear client, Redis queue, fleet workers, etc. Use Linear, Redis, GitHub, and other well-known services directly — there is no boundary preventing it.

Some commands are temporarily scaffolded as thin shells that exec an external binary on PATH (e.g., `findGovernorBinary` in `afcli/governor_start.go`). **Treat those as stopgaps, not the destination.** When a sub-issue's acceptance criteria contradict a thin-shell scaffold, follow the issue — the in-process implementation is the goal.

The library surface (`afclient`, `afcli`, `worker`) is exposed so other Go binaries can compose these commands via `afcli.RegisterCommands`.

## Legacy AgentFactory reference

The legacy TypeScript AgentFactory project lives in a sibling directory: `../donmai-libraries/`. Issue descriptions in this project reference paths like `packages/cli/src/governor.ts` — those resolve relative to that legacy repo, e.g. `../donmai-libraries/packages/cli/src/governor.ts`.

Key packages there:

* `packages/cli/` — TypeScript CLI commands being ported into `afcli/`
* `packages/core/` — runner / decision-engine logic
* `packages/linear/` — Linear GraphQL client (port target for `internal/linear/`)
* `packages/server/` — coordinator HTTP service (stays server-side; not in scope for this binary)
* `packages/mcp-server/`, `packages/dashboard/`, `packages/nextjs/` — out of scope for `af`

Treat the legacy repo as **read-only reference**. Don't modify it from work in this project.

## Architecture

The canonical architecture corpus is [`donmai-architecture`](https://github.com/RenseiAI/donmai-architecture) (public). Read it first; it is the source of truth for everything in the execution layer — the `donmai` binary, daemon, runner, the eight Provider Families, kits, the workflow engine. Locally it sits alongside this repo at `../donmai-architecture/`.

Read in this order:

1. `donmai-architecture/001-layered-execution-model.md` — canonical synthesis. Always first.
2. The reference doc(s) for whichever layer you are working on — `donmai-architecture/002`–`008`, `011`, `013`–`016`.
3. Any open ADRs that touch your work (`donmai-architecture/ADR-*.md`).
4. `donmai-architecture/BOUNDARY.md` — boundary-tagging convention. Read before authoring a new ADR or moving doc content; it determines what kind of doc you're writing and where new ADRs live.

If this project's docs conflict with `donmai-architecture/`, the corpus wins. Either update this project's docs to align, or open an ADR to amend the corpus.

## Package Architecture

```
donmai/
├── afclient/        # PUBLIC — API client, types, mock, errors
├── afcli/           # PUBLIC — Cobra command factories (RegisterCommands pattern)
├── worker/          # PUBLIC — Worker protocol (register, poll, heartbeat, fleet)
├── cmd/donmai/      # Binary entry point (thin wrapper over afcli)
└── internal/        # MODULE-PRIVATE — TUI views, app routing, inline output
    ├── app/         #   Root Bubble Tea model, view routing
    ├── views/       #   Dashboard, detail, palette views
    └── inline/      #   TTY-aware inline output helpers
```

### Public Packages (importable by downstream consumers)

- **`afclient/`** — `DataSource` interface, `Client`, `MockClient`, all request/response types, sentinel errors. This is the API contract.
- **`afcli/`** — Command factories registered via `RegisterCommands(root *cobra.Command, cfg Config)`. The `Config.ClientFactory` provides the `DataSource`. All command factories are unexported — only `RegisterCommands` and `Config` are exported. The dashboard is exposed as the `dashboard` subcommand when `Config.EnableDashboard` is true.
- **`worker/`** — Worker protocol client: registration (rsp_live_ tokens), polling, heartbeat, fleet process management.

### Adding New Commands

New commands go in `afcli/` as unexported factory functions, then wire into `RegisterCommands`:

```go
// afcli/mycommand.go
func newMyCmd(ds func() afclient.DataSource) *cobra.Command {
    return &cobra.Command{
        Use: "mycommand",
        RunE: func(cmd *cobra.Command, args []string) error {
            client := ds()
            // ... use client ...
        },
    }
}

// afcli/commands.go — add to RegisterCommands:
root.AddCommand(newMyCmd(ds))
```

Follow existing patterns in `afcli/agent.go` and `afcli/status.go`.

## Dependency Stack

Charm v2 ecosystem + Cobra:
- `charm.land/bubbletea/v2` — TUI framework (Elm architecture)
- `charm.land/lipgloss/v2` — Terminal styling
- `charm.land/bubbles/v2` — Reusable UI components
- `github.com/RenseiAI/tui-components` — Shared theme, format, widgets
- `log/slog` — Structured logging (stdlib)
- `github.com/spf13/cobra` — CLI framework
- `github.com/sahilm/fuzzy` — Fuzzy search (command palette)
- `github.com/joho/godotenv` — .env.local loading

No other direct dependencies without compelling justification.

## Commands

```bash
make build           # Build donmai binary
make test            # go test -race ./...
make lint            # golangci-lint run
make fmt             # gofumpt -w .
make vuln            # govulncheck ./...
make coverage        # Test with coverage report
make run-mock        # Run TUI dashboard with mock data
make run-status-mock # Run status with mock data
```

## Conventions

- **Errors**: `fmt.Errorf("context: %w", err)`. Sentinel errors in `afclient/errors.go` for expected failures. Never panic. Never `log.Fatal`.
- **Logging**: `log/slog` to stderr. Disabled in TUI mode. `--debug`/`--quiet` flags for CLI.
- **Testing**: stdlib `testing` + table-driven tests. No testify. `afclient.NewMockClient()` for data. `httptest` for API mocks. Coverage: 80% target, 70% minimum.
- **Linting**: `golangci-lint` with govet, staticcheck, gofumpt, errcheck, gosec, gocritic, revive.
- **Naming**: Lowercase single-word packages, PascalCase exports.
- **API types**: All request/response types in `afclient/types.go`. Client methods in `afclient/client.go`. Sentinel errors in `afclient/errors.go`.

## Hooks

- `.claude/settings.json` registers a `SessionStart` hook running `scripts/refresh-worktree.sh` to auto-rebase and refresh deps; active only on linked worktrees.

## API Endpoints

The AgentFactory coordinator exposes these endpoints:

**Public (read-only):**

- `GET /api/public/stats` — Fleet statistics
- `GET /api/public/sessions` — Session list
- `GET /api/public/sessions/:id` — Session detail
- `GET /api/public/sessions/:id/activities` — Activity stream

**Authenticated (Bearer token):**

- `POST /api/mcp/submit-task` — Queue new task
- `POST /api/mcp/stop-agent` — Stop running agent
- `POST /api/mcp/forward-prompt` — Send prompt to agent
- `GET /api/mcp/cost-report` — Cost analytics
- `GET /api/mcp/list-fleet` — Fleet snapshot

**CLI auth:**

- `GET /api/cli/whoami` — Verify API key, return org/project context

## Local daemon control API (127.0.0.1:7734)

The locally-installed `rensei-daemon` exposes an HTTP control API consumed
by the `donmai daemon …` CLI surface and by per-session worker children. See
`daemon/README.md` for the full endpoint reference. Notable post-F.2.8
endpoints:

- `GET /api/daemon/sessions` — list active session handles
- `GET /api/daemon/sessions/:id` — per-session detail (issued to spawned
  `donmai agent run` workers; localhost-only)

## `donmai agent run` (F.2.8)

The daemon spawns `donmai agent run` for every claimed session. The subcommand
reads its session id from `RENSEI_SESSION_ID` (set by the spawner), fetches
the full QueuedWork payload from the daemon's local control API, builds a
runner.Registry with stub + claude + codex (best-effort), and invokes
`runner.Runner`. Operators rarely invoke this manually; see
`daemon/README.md`'s operator runbook for debugging tips.

## Credentials in standalone mode (no daemon, no platform)

When `af` runs OUTSIDE of rensei-tui (no daemon credential pipeline, no
platform session), agents inherit credentials from the donmai process per a
fixed two-tier precedence:

| Precedence | Source                                | Notes                                                                 |
| ---------- | ------------------------------------- | --------------------------------------------------------------------- |
| 1          | AF-TUI process env (`os.Environ()`)   | Anything the operator `export`'d before launching `af`.               |
| 2          | `${gitRoot}/.env.local`               | Parsed once at donmai startup; never copied into spawned worktrees.       |
| Fail-open  | Redacted stderr warning               | `[creds] no source for KEY — agent may fail` per missing variable.    |

Sources are merged at `daemon run` time into `SpawnerOptions.BaseEnv` so
the standard child-spawn path picks them up. The merge order respects
the precedence above: process env wins over `.env.local`, and any
caller-supplied `BaseEnv` entry (set by daemon code, not the operator)
wins over both.

`AGENT_ENV_BLOCKLIST` is the single source of truth in
`internal/credentials/blocklist.go`. It captures the daemon's own auth
surface — `RENSEI_DAEMON_JWT`, `WORKER_API_KEY`, `M2M_JWT_SECRET`, …
— that must never bleed into a child agent regardless of source. The
rensei-tui daemon hardcodes the same list in
`daemon/credentials/socket.go`; the two stay in sync manually until
the OSS boundary permits a shared import.

Operators can pin the mode via `donmai daemon run --standalone-creds=<on|off|auto>`.
The default is `auto`, which selects `on` when `RENSEI_DAEMON_JWT` is
unset (i.e. AF-TUI is NOT being driven by rensei-tui's credential
socket) and `off` otherwise.

Security guardrails:

- AF-TUI never copies `.env.local` into the worktree — the file stays
  at `${gitRoot}`; only the parsed values live in the AF-TUI process
  memory and are forwarded through child env.
- `.env.local` paths are resolved from `gitRoot` only; AF-TUI does NOT
  walk parent directories looking for one.
- World-readable `.env.local` triggers a one-time stderr warning
  recommending `chmod 600`; the file is still parsed.
- Values are never echoed to stdout/stderr/logs — log lines reference
  variable names only.
- Malformed `.env.local` lines are non-fatal: the variable name is
  logged with line number; the value side is dropped.
