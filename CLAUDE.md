# agentfactory-tui

OSS terminal dashboard and CLI for AgentFactory AI agent fleets.

**Module**: `github.com/RenseiAI/agentfactory-tui`

## Boundary

This is an open-source project. It must never contain or reference proprietary platform features, endpoints, or concepts. All functionality here is generic AgentFactory core. The closed-source `rensei-tui` imports this as a Go library dependency via `afcli.RegisterCommands` — so all generic commands built here automatically appear in the `rensei` binary too.

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

### Public Packages (importable by rensei-tui and other consumers)

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
