.PHONY: build run run-mock run-status run-status-mock test lint fmt coverage clean

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-tui ./cmd/af-tui
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-status ./cmd/af-status

run: build
	./$(BUILD_DIR)/af-tui

run-mock: build
	./$(BUILD_DIR)/af-tui --mock

run-status: build
	./$(BUILD_DIR)/af-status

run-status-mock: build
	./$(BUILD_DIR)/af-status --mock

test:
	go test ./...

lint:
	go vet ./...

fmt:
	gofumpt -w .

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

clean:
	rm -rf $(BUILD_DIR)/ coverage.out
