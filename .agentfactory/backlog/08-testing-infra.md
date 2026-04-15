# Testing & Infrastructure

Cross-cutting concerns for test coverage, CI, and quality.

## Issues

### AF-080: Add tests for existing API client and mock
**Priority**: High
**Labels**: testing

Expand test coverage for `internal/api/`. Add tests for Client HTTP methods (using httptest), error handling, timeout behavior. Current coverage: 4 tests (mock only).

### AF-081: Add tests for inline status/watch output
**Priority**: Medium
**Labels**: testing

Test `internal/inline/` package: status formatting, watch loop behavior, TTY vs piped output detection, JSON mode.

### AF-082: Add tests for format package
**Priority**: Medium
**Labels**: testing

Expand `internal/format/format_test.go` with edge cases: negative durations, large token counts, timezone handling for RelativeTime/Timestamp. Current: 3 test functions.

### AF-083: Import tui-components and remove local duplicates
**Priority**: High
**Labels**: infra

Add `github.com/RenseiAI/tui-components` as a dependency. Replace `internal/theme/`, `internal/format/`, and `internal/component/` imports with tui-components equivalents. Delete local copies. Note: `theme/status.go` signature changed from typed enum to string — update all call sites.

### AF-084: Add integration test harness with mock server
**Priority**: Medium
**Labels**: testing

Create `internal/testutil/` with an httptest-based mock server implementing the coordinator API. Enable end-to-end command testing without a real server.

### AF-085: Enforce minimum test coverage in CI
**Priority**: Low
**Labels**: testing, infra

Add coverage threshold check to CI workflow. Fail if coverage drops below 60%. Target: 80%.
