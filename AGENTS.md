# donmai — the OSS agent-fleet CLI and daemon (OSS-public)

Go. Module `github.com/RenseiAI/donmai`. The single `donmai` binary operates a
local agent fleet: daemon lifecycle, runner, governor scan loop, issue-tracker
ops, kits, code intelligence, TUI dashboard. It is a client/execution layer with
essentially no server side; downstream embedders compose it as a library via
`afcli.RegisterCommands`. Everything here must build, run, and operate WITHOUT
any commercial control plane.

## Operating context

- Governing corpus: `../donmai-architecture/` (public,
  `github.com/RenseiAI/donmai-architecture`). Read order:
  `001-layered-execution-model.md` first → the layer doc(s) for your area
  (`002`–`008`, `011`, `013`–`016`) → open `ADR-*.md` → `BOUNDARY.md` before
  authoring an ADR or moving doc content. The corpus wins over code: align the
  code or open an ADR. Missing? `gh repo clone RenseiAI/donmai-architecture
  ../donmai-architecture` (from a worktree: `../../donmai-architecture`).
- Smokes live in `../donmai-smokes` (platform-free by contract); public docs at
  donmai.dev come from `../donmai-site/content/docs/`; official kits in
  `../donmai-kits`; shared TUI widgets in `../tui-components`.
- Legacy TypeScript reference: `../donmai-libraries/` (deprecating). Issue
  descriptions citing `packages/cli/src/*.ts` resolve there. **Read-only** —
  never modify it from a donmai session. Thin-shell scaffolds that exec an
  external binary are stopgaps, not the destination; when an issue's acceptance
  criteria contradict a scaffold, follow the issue.
- Code work happens in a sibling worktree: `scripts/create-worktree.sh <name>` →
  `../donmai.wt/<name>`. A `SessionStart` hook runs
  `scripts/refresh-worktree.sh` on linked worktrees.
- State home is `~/.donmai/`; env vars are `DONMAI_*` (the pre-debrand
  fallbacks were removed in v0.14.0). Local daemon control API:
  `127.0.0.1:7734` (`/api/daemon/*`, localhost-only) — full reference in
  `daemon/README.md`. `donmai agent run` reads `DONMAI_SESSION_ID`, fetches its
  QueuedWork from that API, and invokes `runner.Runner`.

## Before you start — read in this order

| The moment you... | Read |
|---|---|
| change a CLI command surface, daemon endpoint, provider contract, kit surface, or wire type | the corpus read order above |
| add a new command | §Package architecture below + copy the pattern in `afcli/agent.go` |
| touch credential or env handling | `docs/agents/CREDENTIALS-STANDALONE.md` |
| edit README, docs, help text, or anything shipped | §Boundary below + run `make guard` |
| are about to write "done"/"fixed" or open a PR | Gates below + `../donmai-architecture/agents/PROTOCOL.md` §V |
| are about to claim a behaviour is covered / "I added a test" | `../donmai-architecture/agents/PROTOCOL.md` §V V16–V21 (revert → RED → restore → GREEN) |
| hit a failing test or a `-race` flake | `../donmai-architecture/agents/PROTOCOL.md` §D |
| cut or dry-run a release | `PROTOCOL.md` §R + `.goreleaser.yaml` |

When a row matches, read that doc before your next edit and follow it literally.

## Gates — "done" means these passed

```bash
make test        # go test -race ./...   (the race flag is mandatory)
make test-tagged # type-check the build-tag-gated test files ./... cannot see
make lint        # golangci-lint run
make guard       # guard-b: closed-source reference linter, self-test + --staged
make build       # type/compile gate — lint and test alone do not prove linkage
```

Also available and CI-relevant: `make fmt` (gofumpt), `make vuln`
(govulncheck), `make verify-generated` (`GOWORK=off go test -race ./matrix/...`
after `make generate`), `make release-dry-run`. CI's parallel `-race` run
exposes flakes a local serial run hides — treat them as real bugs, not noise.
Quote each gate's result line in your completion report.

**Adding a test does not make a behaviour covered — the red does.** Before you
write "covered", "pinned", or "I added a test", revert the production change,
run the test, confirm it goes RED, restore, confirm GREEN, and quote both. A
green suite proves what ran, passed — not that your change ran. The rule and its
seven failure shapes are `../donmai-architecture/agents/PROTOCOL.md` §V V16–V21.
Two shapes are mechanized here:

- **Unregistered suite.** A `_test.go` behind `//go:build sometag` is excluded
  from `go build ./...` and `go test ./...` outright — not run, not compiled,
  not even syntax-checked. Six such files sat unbuilt in this repo. Every tag on
  disk must appear in `make test-tagged`'s literal tag list (never behind a
  `$(VAR)` — the guard reads the text the toolchain really receives);
  `internal/testregistration` fails `make test` if one is missing.
- **Lifecycle ordering.** `tparallel` (in `.golangci.yml`) catches a parent test
  whose `defer` teardown fires while a `t.Parallel()` subtest is still paused —
  the subtest then runs against torn-down state and can never legitimately pass.
  Use `t.Cleanup` in the subtest, not a parent `defer`.

The other five shapes need behavioural knowledge no cheap check has; they stay
on the V16 red. Note also that `go test` reports a SKIPPED test exactly like a
passing one (`ok`) — CI pipes through `scripts/test-summary.py` for the skip
count, and a suite that quietly starts skipping protects nothing.

## Package architecture

- Public, consumed downstream — changing these is a breaking release:
  `afclient/` (DataSource, Client, MockClient, types, sentinel errors — the API
  contract), `afcli/` (Cobra factories; only `RegisterCommands(root, cfg)` and
  `Config` are exported), `worker/` (register, poll, heartbeat, fleet).
- `internal/` is module-private (TUI app, views, inline output).
- New commands: unexported factory in `afcli/<name>.go`, wired in
  `afcli/commands.go` `RegisterCommands` — follow `afcli/agent.go`.
- Dependency stack is fixed: Charm v2 (`charm.land/{bubbletea,lipgloss,bubbles}/v2`),
  `github.com/RenseiAI/tui-components`, `spf13/cobra`, `log/slog`,
  `sahilm/fuzzy`, `joho/godotenv`, `santhosh-tekuri/jsonschema/v6`. No other
  direct dependencies without stated compelling justification.

## Iron rules

- Errors: `fmt.Errorf("context: %w", err)`; sentinel errors in
  `afclient/errors.go`. Never `panic`, never `log.Fatal` (this is a library).
- Tests: stdlib `testing`, table-driven, no testify;
  `afclient.NewMockClient()` for data, `httptest` for APIs. Coverage 80%
  target / 70% minimum.
- Logging: `log/slog` to stderr, disabled in TUI mode; `--debug`/`--quiet`.
- API types go in `afclient/types.go`, client methods in `afclient/client.go`.
- The wire surface embedders consume is coupled: breaking `afclient`/`afcli`/
  `worker` requires a coordinated lock-step release downstream — flag it in
  your report rather than shipping silently.
- New/changed behavior gains smoke coverage in `../donmai-smokes`; state where
  or report `SMOKE-GAP: <what is uncovered>`.

## Boundary — this repo is public

- Nothing closed-source leaks in: no commercial-platform brand words, no
  internal tracker IDs, no private repo links, no internal hostnames or
  workspace paths, no closed env-var names. `scripts/guard-b-lint.sh` is
  vendored from `donmai-architecture` (provenance header names the pinned
  commit; `scripts/check-guard-b-vendor-drift.sh` fails CI if this copy
  drifts from it). `make guard` runs its self-test + `--staged`; CI
  (`.github/workflows/guard-b.yml`) additionally scans this PR's commits,
  the squash-merge message GitHub will compose, and whatever just landed on
  main — all BLOCKING. The tracked-tree `--all` scan runs separately and
  NON-BLOCKING (`make guard-report`, or the `guard-b-tree-residue` CI job)
  for every rule EXCEPT `DEV_ABS_PATH`: this repo carries a pre-existing
  residue `--all` surfaces that predates guard-b (disclosed in
  `.guard-allowlist`'s header) and was never curated against it — allowlist:
  `.guard-allowlist`. `DEV_ABS_PATH` carries no such residue, so it is
  additionally gated BLOCKING on every PR and push, scoped to the changed
  files, by `scripts/guard-b-diff-gate.sh` (repo-owned, not vendored — see
  its own header).
- Platform-needing features do not ship half-working clients here: OSS defines
  interfaces AND ships a working implementation of each; commercial extensions
  live downstream. When only a downstream implementation would exist, split the
  work per `../donmai-architecture/BOUNDARY.md`.
- Never push to `../tui-components` or other repos from a donmai session —
  describe the upstream need instead.

## Gotchas

- `go test ./...` on daemon install/uninstall tests can bootout your own
  developer launchd daemon — expect it and reinstall (`donmai host install`)
  after test runs that touch `daemon/`.
- `GOWORK`: the org `go.work` ties sibling Go repos together; matrix generation
  and smokes intentionally run with `GOWORK=off` — don't "fix" that.
- `golangci-lint` result cache is keyed by module path, not worktree — it serves stale
  absolute paths from a DIFFERENT sibling worktree until `golangci-lint cache clean`.
  Every parallel-worktree Go session risks phantom lint results; new worktrees get a
  per-worktree `GOLANGCI_LINT_CACHE` from `scripts/create-worktree.sh` and should run with
  `GOWORK=off` so the sibling resolves the right module set.
- After a brew upgrade of the daemon-shipping binary, restart the service
  (`brew services restart donmai`, service `dev.donmai.daemon`) — a resident
  old daemon exec's a dead versioned Caskroom path.
- A checked-in stale binary artifact (`af`, `bin/`) may exist in the tree — do
  not ship or reference it; builds come from `make build`.

## Hard stops

- NEVER modify `../donmai-libraries` from work in this repo -> instead: note
  the needed change in your report.
- NEVER commit content that fails `make guard` -> instead: rewrite
  brand-neutrally or add a justified allowlist entry in the same change.
- NEVER weaken a failing check (skip, deleted test, loosened assert,
  lint-disable) -> instead: quote the failure and propose the change.
- NEVER tag a release from a branch name (`$GITHUB_REF_NAME` on
  workflow_dispatch) -> instead: `gh release create v<X> --target <sha>`.
- NEVER run `git worktree remove/prune`, `git reset --hard`, `git clean -fd`,
  or checkout to another branch as a sub-agent -> instead: the orchestrator
  owns worktree lifecycle.
