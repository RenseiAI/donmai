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

# verify-generated runs only the matrix parity gate (the byte-identical check
# plus the protocol/auth/blocklist/narrowing/manifest-agreement rules). Run
# with GOWORK=off to mirror the OSS-standalone CI lane.
verify-generated:
	GOWORK=off go test -race ./matrix/...

# Local snapshot release (no publish, no signing). Per goreleaser convention,
# `--snapshot` implies `--skip=sign,notarize` — the macOS signing/notarize
# blocks only fire on tag-pushed CI runs. Use this to validate the
# build matrix and archive layout locally; for signed binaries, push a tag.
release-dry-run:
	goreleaser release --snapshot --clean
