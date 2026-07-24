.PHONY: build run run-mock run-status run-status-mock test lint fmt vuln coverage clean release-dry-run generate verify-generated guard

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/donmai ./cmd/donmai

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

lint:
	golangci-lint run

guard:
	bash scripts/leak-guard.sh --self-test
	bash scripts/leak-guard.sh --all
	bash scripts/check-no-inbound-attach.sh

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

# verify-generated runs only the matrix parity gate (the byte-identical check
# plus the protocol/auth/blocklist/narrowing/manifest-agreement rules) and the
# canonical env-blocklist artifact freshness check (blocklist.json vs the Go
# source of truth). Run with GOWORK=off to mirror the OSS-standalone CI lane.
verify-generated:
	GOWORK=off go test -race ./matrix/... ./internal/credentials/...

# Local snapshot release (no publish, no signing). GoReleaser v2 snapshot mode
# skips publication but not custom sign pipes, so signing must be skipped
# explicitly. The macOS signing/notarize path still fires on tag-pushed CI.
# Use this to validate the build matrix and archive layout locally; for signed
# binaries, push a tag. Snapshot runs are version-only and never publish a cask
# or alter GitHub's Latest release.
release-dry-run:
	GOWORK=off GOTOOLCHAIN=go1.25.12 GORELEASER_PUBLISH_HOMEBREW=false GORELEASER_MAKE_LATEST=false goreleaser release --snapshot --clean --skip=sign
