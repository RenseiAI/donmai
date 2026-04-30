.PHONY: build run run-mock run-status run-status-mock test lint fmt vuln coverage clean release-dry-run

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/af ./cmd/af

run: build
	./$(BUILD_DIR)/af dashboard

run-mock: build
	./$(BUILD_DIR)/af dashboard --mock

run-status: build
	./$(BUILD_DIR)/af status

run-status-mock: build
	./$(BUILD_DIR)/af status --mock

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	gofumpt -w .

vuln:
	govulncheck ./...

coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

clean:
	rm -rf $(BUILD_DIR)/ coverage.out

# Local snapshot release (no publish, no signing). Per goreleaser convention,
# `--snapshot` implies `--skip=sign,notarize` — the macOS signing/notarize
# blocks (REN-1412) only fire on tag-pushed CI runs. Use this to validate the
# build matrix and archive layout locally; for signed binaries, push a tag.
release-dry-run:
	goreleaser release --snapshot --clean
