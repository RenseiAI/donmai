# Utility Commands

Port remaining legacy CLI utilities.

## Issues

### AF-070: Implement `af setup` subcommand
**Priority**: Medium
**Labels**: migration

Configure development tools for agent workflows. Primary tool: mergiraf (AST-aware merge driver). Support: `--dry-run`, `--worktree-only`, `--skip-check`. Configures `.gitattributes` and git merge driver.

### AF-071: Implement `af cleanup` subcommand
**Priority**: Medium
**Labels**: migration

Clean up orphaned git worktrees and stale branches. Support: `--dry-run`, `--force`, `--path`, `--skip-worktrees`, `--skip-branches`. Identifies orphaned worktrees by branch deletion. With `--force`, removes branches with gone remotes.

### AF-072: Implement `af code` subcommand group
**Priority**: Low
**Labels**: migration

Code intelligence and search tools. Subcommands: `search-symbols`, `get-repo-map`, `search-code`, `check-duplicate`, `find-type-usages`, `validate-cross-deps`. Optional AI features via `VOYAGE_AI_API_KEY` and `COHERE_API_KEY`.

### AF-073: Implement `af logs` subcommand group
**Priority**: Medium
**Labels**: migration

Analyze agent session logs. Port from `af-analyze-logs`. Subcommands:
- `af logs analyze` — scan logs for errors, create Linear issues
- `af logs follow` — live tail with `--interval` (default: 5s)
Support: `--session`, `--dry-run`, `--cleanup`, `--verbose`. Uses `AGENT_LOGS_DIR` (default: `.agent-logs`).

### AF-074: Implement `af migrate-worktrees` subcommand
**Priority**: Low
**Labels**: migration

Migrate worktrees from legacy `.worktrees/` to `../{repoName}.wt/`. Support: `--dry-run`, `--force`. Uses `git worktree move` with fallback to manual move + repair.
