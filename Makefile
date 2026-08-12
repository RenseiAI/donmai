.PHONY: build run run-mock run-status run-status-mock test test-tagged lint fmt vuln coverage clean release-dry-run generate verify-generated guard guard-report hooks hooks-test

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/donmai ./cmd/donmai

# hooks wires this repo's git hooks (currently: .githooks/pre-commit, the
# main-commit guard) via core.hooksPath. Also runs automatically from
# scripts/create-worktree.sh and the SessionStart refresh-worktree.sh hook —
# this target is for a manual/explicit install (e.g. a fresh clone that
# hasn't run either of those yet).
hooks:
	bash scripts/install-git-hooks.sh

# hooks-test exercises scripts/create-worktree.sh + .githooks/pre-commit
# against a throwaway sandbox repo (never this one). Not part of `make test`
# (Go) — this is a bash/git integration suite for the worktree tooling.
hooks-test:
	bash scripts/create-worktree.test.sh

run: build
	./$(BUILD_DIR)/donmai dashboard

run-mock: build
	./$(BUILD_DIR)/donmai dashboard --mock

run-status: build
	./$(BUILD_DIR)/donmai status

run-status-mock: build
	./$(BUILD_DIR)/donmai status --mock

test:
	go test -race ./...

# test-tagged type-checks every build-tag-gated test file in the repo.
#
# A `_test.go` behind `//go:build sometag` is invisible to `go test ./...` and
# `go build ./...` — not run, not compiled, not even syntax-checked. Without
# this target the suite stays green while those files rot.
#
# Compilation is the honest bar: these suites need live harnesses/services CI
# cannot provide, but bit-rot (a renamed symbol, a changed signature) is what
# actually kills them, and that surfaces the moment they are type-checked.
#
# The tag list is spelled out literally, never behind a $(VAR): the guard in
# `internal/testregistration` reads the text the toolchain will really receive,
# so an indirection is exactly where drift would hide. That guard runs under
# `make test` and fails the moment a tag appears on disk without appearing
# here — so this list cannot silently fall behind.
#
# GOWORK=off mirrors the OSS-standalone lane and keeps this runnable from a
# linked worktree the org go.work does not enumerate.
test-tagged:
	GOWORK=off go vet -tags "f28_integration,runner_integration,runtime_integration,integration,codex_integration,stophookspike,pi_scale_load" ./...

lint:
	golangci-lint run

# `guard` stays fast and scoped to what you're about to commit: self-test
# (proves the rules still fire) + --staged (this change only). --all currently
# reports a large pre-existing residue this repo hasn't been curated against
# yet (see .guard-allowlist's header) — `make guard-report` runs it
# non-blocking so that residue stays visible without making ordinary commits
# red for content nobody touched.
guard:
	bash scripts/guard-b-lint-selftest.sh
	bash scripts/guard-b-lint.sh --staged
	bash scripts/check-no-inbound-attach.sh

guard-report:
	bash scripts/guard-b-lint.sh --all || true

fmt:
	gofumpt -w .

vuln:
	govulncheck ./...

coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

clean:
	rm -rf $(BUILD_DIR)/ coverage.out

# generate regenerates the two-axis capability matrix artifacts (matrix/*.json
# + matrix/registry_gen.go) from the live provider/endpoint Manifest()
# declarations. Output is deterministic (no timestamps); the parity test gates
# the committed files against a fresh regenerate.
#
# Run with GOWORK=off: the generator imports only in-module packages, so it
# resolves identically with and without the org go.work workspace, and this
# avoids the "directory prefix not in go.work" error when run from a worktree
# the org-level go.work does not enumerate.
generate:
	GOWORK=off go generate ./matrix/...
	GOWORK=off go generate ./internal/credentials/...
	GOWORK=off go generate ./executioncell

# verify-generated runs the matrix parity gate, canonical env-blocklist
# freshness check, and execution-cell schema/fixture digest + conformance gate.
# Run with GOWORK=off to mirror the OSS-standalone CI lane.
verify-generated:
	GOWORK=off go test -race ./matrix/... ./internal/credentials/... ./executioncell/...

# Local snapshot release (no publish, no signing). GoReleaser v2 snapshot mode
# skips publication but not custom sign pipes, so signing must be skipped
# explicitly. The macOS signing/notarize path still fires on tag-pushed CI.
# Use this to validate the build matrix and archive layout locally; for signed
# binaries, push a tag. Snapshot runs are version-only and never publish a cask
# or alter GitHub's Latest release.
release-dry-run:
	GOWORK=off GOTOOLCHAIN=go1.25.12 GORELEASER_PUBLISH_HOMEBREW=false GORELEASER_MAKE_LATEST=false goreleaser release --snapshot --clean --skip=sign
