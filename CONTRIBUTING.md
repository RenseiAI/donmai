# Contributing to Donmai

Donmai is a Go module and single-binary CLI/TUI for operating Donmai agent
fleets.

Module: `github.com/RenseiAI/donmai`

This repository is taking over command surfaces from the older TypeScript
AgentFactory project. When an issue references a legacy path such as
`packages/cli/src/governor.ts`, use the sibling `../donmai-libraries/` checkout
as a read-only reference and port the behavior into this Go repo.

## Source of Truth

Read `AGENTS.md` before making architectural or command-surface changes. It
captures the package boundaries, command conventions, dependency policy, and
testing expectations for this repo.

The canonical architecture corpus is `../donmai-architecture/` locally and
https://github.com/RenseiAI/donmai-architecture upstream. Read it before
changing execution-layer behavior:

1. `001-layered-execution-model.md`
2. The reference doc for the layer you are touching, especially:
   - `003-workarea-provider.md`
   - `004-sandbox-capability-matrix.md`
   - `005-kit-manifest-spec.md`
   - `011-local-daemon-fleet.md`
   - `013-orchestrator-and-governor.md`
   - `014-tui-operator-surfaces.md`
   - `015-plugin-spec.md`
   - `016-workflow-engine.md`
3. Any ADR that touches the behavior being changed.

If local docs conflict with the architecture corpus, the architecture corpus
wins. Align the local docs or open an ADR to amend the corpus.

## Development Setup

Prerequisites:

- Go 1.25+; `go.mod` currently targets Go 1.25.10
- `make`
- `git`
- `gofumpt`
- `golangci-lint`
- `govulncheck`
- `goreleaser` for release dry-runs only

```bash
git clone https://github.com/RenseiAI/donmai.git
cd donmai

make build           # Build bin/donmai
make test            # go test -race ./...
make lint            # golangci-lint run
make fmt             # gofumpt -w .
make vuln            # govulncheck ./...
make coverage        # Race tests with coverage report
make run-mock        # Run TUI dashboard with mock data
make run-status-mock # Run status with mock data
```

Redis, Linear, GitHub, Claude, Codex, and other provider credentials are only
needed for the command or integration path that talks to those systems. Unit
tests should use mocks, `httptest`, or local fakes such as `miniredis`.

## Repository Layout

```text
cmd/donmai/     Binary entry point
afclient/       Public API client, request/response types, sentinel errors
afcli/          Public Cobra command factories and RegisterCommands
worker/         Public worker registration, polling, heartbeat, fleet client
daemon/         Local daemon runtime and localhost control API
agent/          Normalized agent provider protocol, events, handles
provider/       Agent runtime implementations: stub, claude, codex, etc.
runner/         Per-session orchestration loop
runtime/        Worktree, env, MCP, state, heartbeat helpers
prompt/         Session prompt construction
result/         Completion/result posting
internal/       Module-private TUI, integrations, queues, process helpers
templates/      Prompt and workflow templates
docs/           Repo-local documentation
```

Public packages are importable by downstream consumers. Keep compatibility in
mind when changing `afclient`, `afcli`, `worker`, `runner`, `runtime`, `agent`,
`prompt`, `result`, or `daemon`. Code under `internal/` is module-private.

## Working on Commands

New CLI commands live in `afcli/` as unexported factory functions and are wired
through `RegisterCommands` in `afcli/commands.go`.

```go
func newMyCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "mycommand",
		Short:        "Do one thing",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client := ds()
			_ = client
			return nil
		},
	}
}
```

Follow the existing patterns in `afcli/agent.go`, `afcli/status.go`, and nearby
command tests.

Command rules:

- Use `Config.ClientFactory` and `afclient.DataSource` for API-backed commands.
- Keep command factories unexported unless there is a real downstream API need.
- Support `--json` for machine-readable output where the command exposes data.
- Require confirmation for destructive operations unless `--yes` is supplied.
- Wrap errors with context: `fmt.Errorf("context: %w", err)`.
- Do not use `panic` or `log.Fatal`.
- Treat thin shell-exec scaffolds as temporary. If acceptance criteria call for
  in-process behavior, implement the Go path.

## API Types and Clients

- Add public request and response types in `afclient/types.go` or a focused
  `*_types.go` file when the surface is large.
- Add client methods in `afclient/client.go` or the matching focused client
  file.
- Use sentinel errors from `afclient/errors.go` for expected failure modes.
- Keep JSON field names aligned with the coordinator API.
- Prefer structured parsing and typed structs over ad hoc string handling.

## Runner, Daemon, and Provider Work

For runner and daemon changes, read the package README first:

- `runner/README.md` for per-session orchestration, tail recovery, and failure
  classification.
- `runtime/README.md` for worktree, env, MCP, state, and heartbeat helpers.
- `daemon/README.md` for local control API endpoints and operator debugging.

New agent runtimes should implement `agent.Provider` under `provider/<name>/`
and register through the existing registry path. The runner should continue to
consume normalized `agent.Spec` and event streams; provider-native event mapping
belongs in the provider package.

## Testing

Use stdlib `testing` with table-driven tests. Do not add `testify`.

Guidelines:

- For CLI tests, execute Cobra commands with mock clients and captured output.
- For API calls, use `httptest`.
- For Redis-backed paths, use `miniredis` when practical.
- For filesystem and git workflows, keep tests hermetic and skip clearly when
  required external binaries are unavailable.
- Use build tags for expensive or environment-dependent integration tests.
- Add focused tests for new command flags, output modes, and error handling.

Run the narrow package test while developing, then run the broader gate before
opening a PR:

```bash
go test -race ./afcli/...
go test -race ./runner/...

make fmt
make test
make lint
```

Run `make vuln` when dependency changes or release work is involved.

## Terminal UI Work

The TUI uses the Charm v2 stack:

- `charm.land/bubbletea/v2`
- `charm.land/lipgloss/v2`
- `charm.land/bubbles/v2`
- `github.com/RenseiAI/tui-components`

Keep terminal UI code TTY-aware and avoid logging to stdout in TUI mode.
Use shared theme/components where possible. For CLI output, prefer plain tables
for humans and JSON for automation.

## Dependencies

Do not add direct dependencies without a compelling reason. Prefer the current
stack, the standard library, and existing local helpers. If a dependency is
necessary, explain why in the PR and keep its scope narrow.

## Pull Requests

- Keep PRs focused on one feature, fix, or docs update.
- Include a clear description of what changed and why.
- Add or update tests for behavior changes.
- Update relevant package READMEs when public behavior changes.
- Update `CHANGELOG.md` for user-visible changes.
- Ensure `make fmt`, `make test`, and `make lint` pass before requesting review.

For release work, follow `RELEASING.md`.

## Reporting Issues

Use https://github.com/RenseiAI/donmai/issues for bugs and feature requests.
Include:

- Steps to reproduce
- Expected vs. actual behavior
- `donmai --version` output, if relevant
- OS and Go version for development issues
- Any relevant command output, logs, or environment notes

Never include secrets, API keys, bearer tokens, or private repository contents
in an issue.

## License

By contributing, you agree that your contributions will be licensed under the
MIT License. See `LICENSE`.
