.PHONY: build run run-mock run-status run-status-mock test lint fmt vuln coverage clean

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
