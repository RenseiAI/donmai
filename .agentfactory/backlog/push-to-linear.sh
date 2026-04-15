#!/bin/bash
# Push agentfactory-tui backlog issues to Linear icebox
# Usage: export $(grep -v '^#' /path/to/.env.local | grep -v '^$' | xargs) && bash .agentfactory/backlog/push-to-linear.sh

set -uo pipefail

TEAM="Rensei"
PROJECT="agentfactory-tui"
STATE="Icebox"
COUNT=0
ERRORS=0

create_issue() {
  local title="$1"
  local description="$2"
  local labels="${3:-}"

  local args=(--title "$title" --team "$TEAM" --project "$PROJECT" --state "$STATE")
  if [ -n "$description" ]; then
    args+=(--description "$description")
  fi
  if [ -n "$labels" ]; then
    args+=(--labels "$labels")
  fi

  result=$(af-linear create-issue "${args[@]}" 2>&1) || {
    echo "  ERROR: $result"
    ERRORS=$((ERRORS + 1))
    return 1
  }

  id=$(echo "$result" | grep -o '"identifier":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "  Created $id: $title"
  COUNT=$((COUNT + 1))
  sleep 0.5
}

echo "=== Unified Binary (00) ==="
create_issue "AF-001: Scaffold unified cmd/af/ entry point with Cobra root command" "Create cmd/af/main.go with Cobra root command. Bare af launches Bubble Tea TUI dashboard. Add --mock and --url flags at root level. Load dotenv at startup. Preserve existing af-tui functionality." "infra,migration"
create_issue "AF-002: Migrate af-status to af status subcommand" "Move cmd/af-status/ inline status functionality into af status subcommand. Preserve all flags: --json, --watch, --interval, --mock, --url. Remove cmd/af-status/ directory." "migration"
create_issue "AF-003: Migrate af-tui to af dashboard subcommand" "Move TUI dashboard launch into af dashboard subcommand. Bare af (no args) should also launch the dashboard as the default action." "migration"
create_issue "AF-004: Update .goreleaser.yaml for single af binary" "Replace two-binary build config with single af binary from cmd/af/. Update archive naming. Update homebrew-tap cask to install single af binary." "infra"
create_issue "AF-005: Update Makefile for unified binary" "Replace build-tui/build-status targets with single build target for af. Update run-mock, run-status-mock to use af and af status respectively." "infra"

echo ""
echo "=== Agent Command (01) ==="
create_issue "AF-010: Define agent session API types" "Create types for agent session list, detail, stop, chat, and reconnect operations in internal/api/types.go." "migration"
create_issue "AF-011: Implement af agent list subcommand" "List active agent sessions. Support --all flag to include completed/failed. Display: session ID, issue identifier, status, duration, work type. Requires REDIS_URL." "migration"
create_issue "AF-012: Implement af agent stop subcommand" "Stop a running agent session by identifier. af agent stop SUP-674. Send graceful shutdown signal. Requires REDIS_URL." "migration"
create_issue "AF-013: Implement af agent status subcommand" "Show detailed status for a specific agent session. Display: status, duration, tokens used, cost, current activity." "migration"
create_issue "AF-014: Implement af agent chat subcommand" "Send a message to a running agent. af agent chat SUP-674 'message'. Uses forward-prompt API." "migration"
create_issue "AF-015: Implement af agent reconnect subcommand" "Reconnect to an orphaned agent session. Requires LINEAR_API_KEY." "migration"

echo ""
echo "=== Governor Command (02) ==="
create_issue "AF-020: Implement af governor start subcommand" "Start the governor process. Support flags: --project (repeatable), --scan-interval, --max-dispatches, --once, --mode (event-driven/poll-only), feature toggles (--no-auto-research, etc.), --foreground/background with PID tracking. Requires LINEAR_API_KEY, REDIS_URL." "migration"
create_issue "AF-021: Implement af governor stop subcommand" "Stop running governor process. SIGTERM with grace period, then SIGKILL. Clean up PID file." "migration"
create_issue "AF-022: Implement af governor status subcommand" "Show governor status: running/stopped, uptime, projects being governed, last scan time, dispatch count." "migration"

echo ""
echo "=== Worker/Fleet Commands (03) ==="
create_issue "AF-030: Implement af worker start subcommand" "Start a single worker process that polls coordinator for queued work. Support flags: --capacity, --hostname, --api-url, --api-key, --projects, --dry-run. Requires WORKER_API_URL, WORKER_API_KEY." "migration"
create_issue "AF-031: Implement af worker status subcommand" "Show worker status: connected/disconnected, capacity, active agents, projects." "migration"
create_issue "AF-032: Implement af fleet start subcommand" "Start a fleet of worker processes. Auto-detect CPU cores (default: cores/2). Support flags: -w/--workers, -c/--capacity, -p/--projects, --auto-update, --dry-run." "migration"
create_issue "AF-033: Implement af fleet stop subcommand" "Stop all fleet workers. Graceful shutdown with SIGTERM, force after timeout." "migration"
create_issue "AF-034: Implement af fleet status subcommand" "Show fleet-wide status: worker count, total capacity, active agents, per-worker breakdown." "migration"
create_issue "AF-035: Implement af fleet scale subcommand" "Dynamically scale fleet up or down. af fleet scale 8 adjusts to 8 workers." "migration"

echo ""
echo "=== Queue Commands (04) ==="
create_issue "AF-040: Implement af queue list subcommand" "List items in the work queue. Show: position, issue ID, work type, priority, queued time. Requires REDIS_URL." "migration"
create_issue "AF-041: Implement af queue sessions subcommand" "List active sessions tracked by the queue. Show: session ID, status, worker, duration." "migration"
create_issue "AF-042: Implement af queue workers subcommand" "List registered workers and their claims. Show: worker ID, hostname, capacity, active claims." "migration"
create_issue "AF-043: Implement af queue clear subcommand" "Clear queue items. Support --claims, --queue, --all flags. Requires REDIS_URL." "migration"
create_issue "AF-044: Implement af queue reset subcommand" "Full queue reset — clears all state, sessions, claims. Requires --confirm flag." "migration"
create_issue "AF-045: Implement af queue remove subcommand" "Remove a specific item from the queue by ID. af queue remove <id>." "migration"

echo ""
echo "=== Merge Queue Commands (05) ==="
create_issue "AF-050: Implement af merge-queue status subcommand" "Show merge queue status: paused/active, items in queue, current processing item. Requires REDIS_URL." "migration"
create_issue "AF-051: Implement af merge-queue list subcommand" "List items in the merge queue with position, PR link, status, retry count. Support --repo flag." "migration"
create_issue "AF-052: Implement af merge-queue retry subcommand" "Retry a failed merge queue item. af merge-queue retry <id> --repo <repoId>." "migration"
create_issue "AF-053: Implement af merge-queue skip subcommand" "Skip a stuck item in the merge queue. af merge-queue skip <id> --repo <repoId>." "migration"
create_issue "AF-054: Implement af merge-queue pause/resume subcommands" "Pause or resume merge queue processing. af merge-queue pause --repo <repoId>." "migration"
create_issue "AF-055: Implement af merge-queue priority subcommand" "Adjust priority of a merge queue item. af merge-queue priority <id> <priority> --repo <repoId>." "migration"

echo ""
echo "=== Linear Commands (06) ==="
create_issue "AF-060: Implement af linear get-issue subcommand" "Get issue details by identifier. af linear get-issue SUP-674. Output: title, status, assignee, labels, description. Requires LINEAR_API_KEY." "migration"
create_issue "AF-061: Implement af linear list-issues subcommand" "List issues with filters. Support: --project, --status, --assignee, --label, --limit." "migration"
create_issue "AF-062: Implement af linear create-issue subcommand" "Create a new issue. Support: --title, --description, --project, --status, --assignee, --label, --priority." "migration"
create_issue "AF-063: Implement af linear update-issue subcommand" "Update issue fields. af linear update-issue SUP-674 --status 'In Progress' --assignee 'user'." "migration"
create_issue "AF-064: Implement af linear comment subcommands" "list-comments and create-comment on issues." "migration"
create_issue "AF-065: Implement af linear backlog subcommands" "list-backlog-issues and list-unblocked-backlog. Used by orchestrator for work dispatch." "migration"
create_issue "AF-066: Implement af linear relation subcommands" "add-relation, list-relations, remove-relation for managing issue dependencies." "migration"
create_issue "AF-067: Implement af linear sub-issue subcommands" "list-sub-issues, list-sub-issue-statuses, update-sub-issue." "migration"
create_issue "AF-068: Implement af linear check-blocked/check-deployment" "Check if an issue is blocked or ready for deployment." "migration"
create_issue "AF-069: Implement af linear create-blocker subcommand" "Create a blocker relation between issues." "migration"

echo ""
echo "=== Utility Commands (07) ==="
create_issue "AF-070: Implement af setup subcommand" "Configure development tools (mergiraf AST-aware merge driver). Support: --dry-run, --worktree-only, --skip-check. Configures .gitattributes and git merge driver." "migration"
create_issue "AF-071: Implement af cleanup subcommand" "Clean up orphaned git worktrees and stale branches. Support: --dry-run, --force, --path, --skip-worktrees, --skip-branches." "migration"
create_issue "AF-072: Implement af code subcommand group" "Code intelligence: search-symbols, get-repo-map, search-code, check-duplicate, find-type-usages, validate-cross-deps. Optional AI via VOYAGE_AI_API_KEY and COHERE_API_KEY." "migration"
create_issue "AF-073: Implement af logs subcommand group" "Analyze agent session logs. af logs analyze (scan for errors, create Linear issues), af logs follow (live tail). Support: --session, --dry-run, --cleanup, --verbose." "migration"
create_issue "AF-074: Implement af migrate-worktrees subcommand" "Migrate worktrees from legacy .worktrees/ to ../{repoName}.wt/. Support: --dry-run, --force. Uses git worktree move with fallback." "migration"

echo ""
echo "=== Testing & Infrastructure (08) ==="
create_issue "AF-080: Add tests for existing API client and mock" "Expand test coverage for internal/api/. Add tests for Client HTTP methods (using httptest), error handling, timeout behavior. Current: 4 tests (mock only)." "testing"
create_issue "AF-081: Add tests for inline status/watch output" "Test internal/inline/ package: status formatting, watch loop behavior, TTY vs piped output detection, JSON mode." "testing"
create_issue "AF-082: Add tests for format package edge cases" "Expand internal/format/format_test.go with edge cases: negative durations, large token counts, timezone handling. Current: 3 test functions." "testing"
create_issue "AF-083: Import tui-components and remove local duplicates" "Add github.com/RenseiAI/tui-components as dependency. Replace internal/theme/, internal/format/, internal/component/ imports with tui-components equivalents. Delete local copies. Update status.go call sites (typed enum to string)." "infra"
create_issue "AF-084: Add integration test harness with mock server" "Create internal/testutil/ with httptest-based mock server implementing the coordinator API. Enable end-to-end command testing." "testing"
create_issue "AF-085: Enforce minimum test coverage in CI" "Add coverage threshold check to CI workflow. Fail if below 60%. Target: 80%." "testing"

echo ""
echo "=== Done ==="
echo "Created: $COUNT issues"
echo "Errors: $ERRORS"
