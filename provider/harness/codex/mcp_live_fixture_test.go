package codex

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

const codexFakeMCPStdioEnv = "DONMAI_CODEX_FAKE_MCP_STDIO"

// TestMain lets the package test binary double as (a) a real stdio MCP server
// for the live Codex app-server boundary test and (b) a fake `codex
// app-server` for the per-session environment-overlay boundary test
// (appserver_env_test.go). Both child paths never run tests and write protocol
// messages only to stdout.
func TestMain(m *testing.M) {
	if os.Getenv(codexFakeMCPStdioEnv) == "1" {
		runCodexFakeMCPStdio()
		os.Exit(0)
	}
	if os.Getenv(codexFakeAppServerEnv) == "1" {
		runCodexFakeAppServer()
		os.Exit(0)
	}
	if os.Getenv(codexFakeNamedAppServerEnv) == "1" && argvHasFlag(os.Args, "--listen") {
		runCodexFakeNamedAppServer()
		os.Exit(0)
	}
	if os.Getenv(codexFakePTYClientEnv) == "1" {
		// The documented bug signature this package's stderr-capture change
		// exists to stop hiding: a --remote PTY client that only observes a
		// dropped connection exits 0 and reports nothing wrong.
		os.Exit(0)
	}
	if os.Getenv(codexFakePTYClientCreatesThreadEnv) == "1" && argvHasFlag(os.Args, "--remote") {
		os.Exit(runCodexFakePTYClientCreatesThread())
	}
	// Sandbox the plugin-cache host directory (plugin_cache.go) for every
	// test in this package's normal (non-fixture-role) run: without this,
	// any test that builds a boundary through New or SpawnInteractive
	// without itself overriding Options.pluginCacheDir would resolve
	// resolveCodexPluginCacheDir's default and seed/harvest against this
	// MACHINE's real ~/.donmai state directory — a real host-state side
	// effect no test in this suite should ever have. Tests that want to
	// exercise the reuse mechanism itself pass an explicit hostCacheDir (or
	// Options.pluginCacheDir) directly and are unaffected by this default.
	pluginCacheSandbox, sandboxErr := os.MkdirTemp("", "donmai-codex-plugin-cache-test-")
	if sandboxErr == nil {
		_ = os.Setenv(codexPluginCacheDirEnv, pluginCacheSandbox)
	}
	code := m.Run()
	if pluginCacheSandbox != "" {
		_ = os.RemoveAll(pluginCacheSandbox)
	}
	os.Exit(code)
}

// argvHasFlag reports whether flag appears as its own argument anywhere in
// argv — used to disambiguate the two child roles a single fixture env
// var pair can select between (the bootstrap app-server's own --listen
// invocation vs. the PTY's --remote attach), since both processes inherit
// the same session-scoped env in production and therefore the same fixture
// env vars in these tests.
func argvHasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

func runCodexFakeMCPStdio() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 || string(request.ID) == "null" {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if result, ok := codexFakeMCPResult(request.Method, request.Params); ok {
			response["result"] = result
		} else {
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = encoder.Encode(response)
	}
}

type codexFakeMCPHTTP struct {
	server      *httptest.Server
	initialized atomic.Int32
	headerSeen  atomic.Bool
}

func newCodexFakeMCPHTTP(t *testing.T) *codexFakeMCPHTTP {
	t.Helper()
	fixture := &codexFakeMCPHTTP{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Header.Get("X-Probe") == "nonce" {
			fixture.headerSeen.Store(true)
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&rpc); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		response := map[string]any{"jsonrpc": "2.0", "id": rpc.ID}
		if result, ok := codexFakeMCPResult(rpc.Method, rpc.Params); ok {
			response["result"] = result
			if rpc.Method == "initialize" {
				fixture.initialized.Add(1)
			}
		} else {
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(response)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func codexFakeMCPResult(method string, params json.RawMessage) (any, bool) {
	switch method {
	case "initialize":
		var initialize struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &initialize)
		if initialize.ProtocolVersion == "" {
			initialize.ProtocolVersion = "2025-03-26"
		}
		return map[string]any{
			"protocolVersion": initialize.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "donmai-codex-fixture", "version": "1"},
		}, true
	case "tools/list":
		return map[string]any{"tools": []map[string]any{{
			"name":        "fixture_echo",
			"description": "returns its text argument",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
		}}}, true
	case "tools/call":
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "fixture"}}}, true
	case "ping":
		return map[string]any{}, true
	default:
		return nil, false
	}
}
