# agentfactory-tui

OSS terminal dashboard and CLI for AgentFactory AI agent fleets.

**Module**: `github.com/RenseiAI/agentfactory-tui`

## Boundary

This is an open-source project. It must never contain or reference proprietary platform features, endpoints, or concepts. All functionality here is generic AgentFactory core. Downstream closed-source consumers may extend this — but this repo must remain self-contained and platform-agnostic.

## Dependency Stack

Charm v2 ecosystem + Cobra:
- `charm.land/bubbletea/v2` — TUI framework (Elm architecture)
- `charm.land/lipgloss/v2` — Terminal styling
- `charm.land/bubbles/v2` — Reusable UI components
- `log/slog` — Structured logging (stdlib)
- `github.com/spf13/cobra` — CLI framework (unified `af` binary)
- `github.com/sahilm/fuzzy` — Fuzzy search (command palette)
- `github.com/joho/godotenv` — .env.local loading

No other direct dependencies without compelling justification.

## Architecture

Single unified `af` binary covering all use cases:

- `af` (bare) or `af dashboard` — Full Bubble Tea TUI dashboard
- `af status` — Inline status reporter (TTY-aware, watch mode, JSON output)
- `af agent|governor|worker|fleet|queue|...` — CLI subcommands (Cobra)

### TUI Architecture (Bubble Tea v2)

- **Root model** (`internal/app/app.go`) routes between views via messages
- **Views** (`internal/views/`) are Bubble Tea models implementing `Component` interface
  - `dashboard/` — Fleet overview with stats bar and sortable session table
  - `detail/` — Session detail with timeline, metadata, activity stream
  - `palette/` — Fuzzy-search command palette (Ctrl+K)
- **DataSource** interface (`internal/api/client.go`) abstracts data fetching
  - `Client` — Real HTTP client with Bearer token auth, retry, logging
  - `MockClient` — Deterministic mock data for offline development

### Key Patterns

- **Mock-first development**: Use `--mock` flag for offline dev. Mock implements full DataSource interface.
- **View routing**: Root app model dispatches messages to active view. Views communicate via typed messages.
- **Theme/format from tui-components**: Import `github.com/RenseiAI/tui-components/theme` and `format` packages.
- **Cobra + Bubble Tea**: Bare `af` detects TTY and launches TUI. Subcommands are CLI-only. `PersistentPreRunE` initializes shared DataSource and config.
- **Sentinel errors**: Use `internal/api/errors.go` sentinel errors (ErrNotAuthenticated, ErrNotFound, etc.) for expected failure modes. Wrap with context.

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

- **Project layout**: `cmd/af/` entry point, `internal/{api,app,views,config}/` packages
- **Errors**: `fmt.Errorf("context: %w", err)`. Sentinel errors for expected failures. Never panic. Never `log.Fatal`.
- **Logging**: `log/slog` to stderr. Disabled in TUI mode. `--debug`/`--quiet` flags for CLI.
- **Testing**: stdlib `testing` + table-driven tests. No testify. `teatest` for TUI snapshot tests. `cupaloy` for golden files. `httptest` for API mocks. Coverage: 80% target, 70% minimum.
- **Linting**: `golangci-lint` with govet, staticcheck, gofumpt, errcheck, gosec, gocritic, revive.
- **Naming**: Lowercase single-word packages, PascalCase exports
- **New commands**: Each Cobra subcommand gets its own file in `cmd/af/`. Follow existing patterns.
- **New views**: Create directory in `internal/views/<name>/`, implement Component interface. Use Bubbles v2 components as foundations.
- **API types**: All request/response types in `internal/api/types.go`. Client methods in `client.go`. Sentinel errors in `errors.go`.

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
