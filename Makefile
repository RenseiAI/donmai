.PHONY: build run run-mock run-status run-status-mock test lint fmt vuln coverage clean

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/af ./cmd/af
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-tui ./cmd/af-tui
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-status ./cmd/af-status

run: build
	./$(BUILD_DIR)/af

run-mock: build
	./$(BUILD_DIR)/af --mock

run-status: build
	./$(BUILD_DIR)/af-status

run-status-mock: build
	./$(BUILD_DIR)/af-status --mock

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
