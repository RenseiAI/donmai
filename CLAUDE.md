# agentfactory-tui

OSS terminal dashboard and CLI for AgentFactory AI agent fleets.

**Module**: `github.com/RenseiAI/agentfactory-tui`

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
  - `Client` — Real HTTP client with Bearer token auth
  - `MockClient` — Deterministic mock data for offline development

### Key Patterns
- **Mock-first development**: Use `--mock` flag for offline dev. Mock implements full DataSource interface.
- **View routing**: Root app model dispatches messages to active view. Views communicate via typed messages.
- **Theme/format from tui-components**: Import `github.com/RenseiAI/tui-components/theme` and `format` packages.

## Commands

```bash
make build          # Build af binary
make test           # go test ./...
make lint           # go vet ./...
make fmt            # gofumpt -w .
make coverage       # Test with coverage report
make run-mock       # Run TUI dashboard with mock data
make run-status-mock # Run status with mock data
```

## Conventions

- **Project layout**: `cmd/af/` entry point, `internal/{api,app,views,config}/` packages
- **Errors**: `fmt.Errorf("context: %w", err)`. Return errors, never `log.Fatal` in library code.
- **Testing**: Table-driven tests. Interfaces for external deps. Mock implementations. 80% target.
- **Linting**: `go vet`, `staticcheck`, `gofumpt`
- **Naming**: Lowercase single-word packages, PascalCase exports
- **New commands**: Each Cobra subcommand gets its own file in `cmd/af/`. Follow existing patterns.
- **New views**: Create directory in `internal/views/<name>/`, implement Component interface.
- **API types**: All request/response types in `internal/api/types.go`. Client methods in `client.go`.

## API Endpoints

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
- `GET /api/cli/whoami` — Auth check
