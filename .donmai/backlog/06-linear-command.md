# `af linear` — Linear Issue Tracker CLI

Port from legacy `af-linear` (linear.ts). Comprehensive Linear API wrapper with 20+ operations.

**Requires**: `LINEAR_API_KEY` or `LINEAR_ACCESS_TOKEN`.

## Issues

### AF-060: Implement `af linear get-issue` subcommand
**Priority**: High
**Labels**: migration

Get issue details by identifier. `af linear get-issue SUP-674`. Output: title, status, assignee, labels, description.

### AF-061: Implement `af linear list-issues` subcommand
**Priority**: High
**Labels**: migration

List issues with filters. Support: `--project`, `--status`, `--assignee`, `--label`, `--limit`.

### AF-062: Implement `af linear create-issue` subcommand
**Priority**: High
**Labels**: migration

Create a new issue. Support: `--title`, `--description`, `--project`, `--status`, `--assignee`, `--label`, `--priority`.

### AF-063: Implement `af linear update-issue` subcommand
**Priority**: High
**Labels**: migration

Update issue fields. `af linear update-issue SUP-674 --status "In Progress" --assignee "user"`.

### AF-064: Implement `af linear list-comments` / `create-comment` subcommands
**Priority**: Medium
**Labels**: migration

List and create comments on issues.

### AF-065: Implement `af linear list-backlog-issues` / `list-unblocked-backlog` subcommands
**Priority**: Medium
**Labels**: migration

List backlog issues, optionally filtered to unblocked items only. Used by orchestrator for work dispatch.

### AF-066: Implement `af linear` relation subcommands
**Priority**: Medium
**Labels**: migration

`af linear add-relation`, `list-relations`, `remove-relation` for managing issue dependencies.

### AF-067: Implement `af linear` sub-issue subcommands
**Priority**: Medium
**Labels**: migration

`af linear list-sub-issues`, `list-sub-issue-statuses`, `update-sub-issue`.

### AF-068: Implement `af linear check-blocked` / `check-deployment` subcommands
**Priority**: Low
**Labels**: migration

Check if an issue is blocked or ready for deployment.

### AF-069: Implement `af linear create-blocker` subcommand
**Priority**: Low
**Labels**: migration

Create a blocker relation between issues.
