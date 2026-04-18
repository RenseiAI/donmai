# agentfactory-tui

OSS terminal dashboard and CLI for AgentFactory AI agent fleets.

**Module**: `github.com/RenseiAI/agentfactory-tui`

## Boundary

**This is an open-source project.** Every commit, PR description, code comment, and branch name is publicly visible. The following rules are non-negotiable:

1. **No proprietary references.** Never mention closed-source project names, proprietary product names, or internal platform details in code, comments, commit messages, PR titles, or PR bodies. Use generic language like "downstream consumers" or "importing CLIs" instead.
2. **No closed-source issue IDs.** Never reference issue identifiers from closed-source trackers in commits or PRs. Use this project's own issue references only.
3. **No platform-specific concepts.** No `rsk_` token references, no platform API routes, no SaaS feature names. All functionality must be generic AgentFactory core.
4. **Generic hooks, not named consumers.** When adding extension points (e.g. `Config.ProjectFunc`), describe them in terms of what they do, not who uses them. Say "allows importing CLIs to scope by project" not naming specific consumers.
5. **Branch names** must use this project's own issue IDs or descriptive names — never IDs from other projects.

Downstream closed-source consumers import this as a Go library via `afcli.RegisterCommands` — all generic commands built here automatically appear in those binaries too.

## Package Architecture

```
agentfactory-tui/
├── afclient/        # PUBLIC — API client, types, mock, errors
├── afcli/           # PUBLIC — Cobra command factories (RegisterCommands pattern)
├── worker/          # PUBLIC — Worker protocol (register, poll, heartbeat, fleet)
├── cmd/af/          # Binary entry point (thin wrapper over afcli)
└── internal/        # MODULE-PRIVATE — TUI views, app routing, inline output
    ├── app/         #   Root Bubble Tea model, view routing
    ├── views/       #   Dashboard, detail, palette views
    └── inline/      #   TTY-aware inline output helpers
```

### Public Packages (importable by downstream consumers)

- **`afclient/`** — `DataSource` interface, `Client`, `MockClient`, all request/response types, sentinel errors. This is the API contract.
- **`afcli/`** — Command factories registered via `RegisterCommands(root *cobra.Command, cfg Config)`. The `Config.ClientFactory` provides the `DataSource`. All command factories are unexported — only `RegisterCommands`, `RunDashboard`, and `Config` are exported.
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
make build           # Build af binary
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
