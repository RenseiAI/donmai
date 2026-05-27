# Unified `af` Binary — Consolidation

Consolidate existing `cmd/af-tui/` and `cmd/af-status/` into a single `cmd/af/` entry point. One binary, one install, all use cases.

## Issues

### AF-001: Scaffold unified `cmd/af/` entry point with Cobra root command
**Priority**: Critical
**Labels**: infra, migration

Create `cmd/af/main.go` with Cobra root command. When invoked with no args (bare `af`), launch the Bubble Tea TUI dashboard. Add `--mock` and `--url` flags at root level. Load dotenv at startup.

**Acceptance**: `af` launches TUI dashboard. `af --help` shows all subcommands. Existing af-tui functionality preserved.

### AF-002: Migrate af-status to `af status` subcommand
**Priority**: Critical
**Labels**: migration

Move `cmd/af-status/` inline status functionality into `af status` subcommand. Preserve all flags: `--json`, `--watch`, `--interval`, `--mock`, `--url`. Remove `cmd/af-status/` directory.

**Acceptance**: `af status`, `af status --json`, `af status --watch` all work identically to current `af-status` binary.

### AF-003: Migrate af-tui to `af dashboard` subcommand
**Priority**: Critical
**Labels**: migration

Move TUI dashboard launch into `af dashboard` subcommand. Bare `af` (no args) should also launch the dashboard as the default action.

**Acceptance**: `af dashboard` and `af` both launch the TUI. `af dashboard --mock` works.

### AF-004: Update .goreleaser.yaml for single `af` binary
**Priority**: Critical
**Labels**: infra

Replace the two-binary build config with a single `af` binary from `cmd/af/`. Update archive naming. Update homebrew-tap cask to install single `af` binary.

**Acceptance**: `goreleaser --snapshot --clean` produces single `af` binary for all platforms. Cask installs correctly.

### AF-005: Update Makefile for unified binary
**Priority**: High
**Labels**: infra

Replace `build-tui`/`build-status` targets with single `build` target for `af`. Update `run-mock`, `run-status-mock` to use `af` and `af status` respectively.

**Acceptance**: `make build` produces `bin/af`. `make run-mock` launches TUI.
