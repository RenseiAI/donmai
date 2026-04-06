.PHONY: build build-tui build-status run run-mock run-status run-status-mock test clean

BUILD_DIR := bin
LDFLAGS := -ldflags="-s -w"

build: build-tui build-status

build-tui:
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-tui ./cmd/af-tui

build-status:
	go build $(LDFLAGS) -o $(BUILD_DIR)/af-status ./cmd/af-status

run: build-tui
	./$(BUILD_DIR)/af-tui

run-mock: build-tui
	./$(BUILD_DIR)/af-tui --mock

run-status: build-status
	./$(BUILD_DIR)/af-status

run-status-mock: build-status
	./$(BUILD_DIR)/af-status --mock

test:
	go test ./...

clean:
	rm -rf $(BUILD_DIR)/
