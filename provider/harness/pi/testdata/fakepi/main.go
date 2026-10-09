// Command fakepi is a minimal stand-in for the `pi --mode rpc` JSONL-over-
// stdio protocol. It is built by two test suites: provider/harness/pi's
// N-instance load-validation harness (../../scale_load_test.go, build tag
// pi_scale_load), and the runner's usage test
// (runner/pi_cache_usage_test.go), which drives a real pi handle through
// the runner to the terminal status post. It never runs a language model
// and never interprets a real extension file; it exists purely to give
// those tests a REAL subprocess to fork/exec, so the load harness measures
// real process overhead and the usage test exercises the real event
// mapping — the things the rest of this package's tests deliberately avoid
// by stubbing stdin/stdout over an io.Pipe (Options.skipProcess) for fast,
// deterministic correctness checks.
//
// It reproduces exactly the wire shapes provider/harness/pi/handle.go
// expects — the SAME shapes that package's own handle_test.go scripts by
// hand (handshakeEvent, getStateResponse, uiRequest): a `--version` probe,
// the extension_ui_request handshake carrying the donmai marker/token/sha,
// get_state, and prompt/steer/follow_up/abort commands answered with
// message_update/message_end/turn_end/agent_settled events. Every turn_end
// carries the same fixture usage (turnUsage*), in pi's own shape: input
// excludes the cache-read and cache-write buckets.
//
// This file lives under testdata/ so the Go toolchain's own `...` pattern
// exclusion keeps it out of every ordinary `go build ./...` / `go vet ./...`
// / `golangci-lint run` invocation (the same reason
// extension_delivery_real_binary_test.go's fixtures live here); the load
// test builds it explicitly by path.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mirrors provider/harness/pi/extension.go's unexported donmaiUIMarker and
// handshakeKind wire constants. Duplicated deliberately: this is a
// standalone test-only binary in its own package main, not a consumer of
// package pi, so it cannot import unexported identifiers — and it should
// not, since widening pi's public API purely to share a test fixture's wire
// literals would weaken the encapsulation those constants exist to keep.
// Wire-shape drift here surfaces immediately as a load-test handshake
// failure, the same way a scripted fixture's drift would.
const (
	uiMarker     = "donmai-policy-v1"
	handshakeKey = "handshake"
)

// The usage every turn reports on its turn_end, in pi's AssistantMessage
// usage shape. Fixed values so a test can assert exactly what one turn
// carries through the mapper, the runner and the status post.
const (
	turnUsageInput      = 1200
	turnUsageOutput     = 80
	turnUsageCacheRead  = 48000
	turnUsageCacheWrite = 512
)

// replyEnvVar overrides the assistant text every turn replies with (default
// "ok"), so a runner test can end a session on the verdict its work type
// needs.
const replyEnvVar = "FAKEPI_REPLY"

// restoreSessionEnvironment does what the real boundary extension does at
// load (extensions/donmai-policy.ts restoreSessionEnvironment): the harness
// execs pi with an allowlisted environment only, and the session's other
// bindings ride the environment section of the credential file named by
// DONMAI_PI_CREDENTIALS_FILE. This stub stands in for pi AND that extension,
// so it restores them the same way — a name already set wins, and the
// harness's own namespaces are never written — before reading its own
// FAKEPI_* knobs from the environment.
func restoreSessionEnvironment() {
	path := os.Getenv("DONMAI_PI_CREDENTIALS_FILE")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the session credential file the harness named
	if err != nil {
		return
	}
	var envelope struct {
		Environment []struct {
			Env   string `json:"env"`
			Value string `json:"value"`
		} `json:"environment"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	for _, entry := range envelope.Environment {
		if entry.Env == "" || strings.ContainsAny(entry.Env, "=\x00") ||
			strings.HasPrefix(entry.Env, "DONMAI_PI_") || strings.HasPrefix(entry.Env, "PI_") {
			continue
		}
		if _, set := os.LookupEnv(entry.Env); set {
			continue
		}
		_ = os.Setenv(entry.Env, entry.Value)
	}
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "--version" {
			// Matches provider/harness/pi/probe.go's PinnedVersion/
			// VerifiedAgainst exactly, so New()'s probe-time version-pin
			// check labels this binary "verified", not merely "proceeds
			// unverified" — the load test measures the same code path a
			// real, in-range pi binary would take.
			fmt.Println("0.80.10")
			return
		}
	}

	restoreSessionEnvironment()
	extPath, sessionDelay := parseArgs(os.Args[1:])
	token := os.Getenv("DONMAI_PI_HANDSHAKE")
	sha := extensionSHAOf(extPath)

	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)

	writeEvent(out, map[string]any{
		"type":        "extension_ui_request",
		"id":          "handshake-1",
		"method":      "input",
		"placeholder": uiMarker,
		"title":       marshalTitle(map[string]any{"donmai": handshakeKey, "token": token, "sha": sha}),
	})

	sessionID := "fakepi-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	for in.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			continue // not a line we understand; a real pi would surface an error, this stub just skips it
		}
		switch cmd["type"] {
		case "extension_ui_response":
			// The handshake ack/reject. Nothing else to do — keep reading.
		case "get_state":
			writeEvent(out, map[string]any{
				"type": "response", "command": "get_state", "success": true,
				"data": map[string]any{"sessionId": sessionID},
			})
		case "get_entries":
			writeEvent(out, map[string]any{
				"type": "response", "command": "get_entries", "success": true,
				"data": map[string]any{"entries": []any{}},
			})
		case "prompt", "follow_up", "steer":
			if sessionDelay > 0 {
				time.Sleep(sessionDelay)
			}
			runTurn(out)
		case "abort":
			return
		}
	}
}

// runTurn emits one minimal, complete turn: agent_start, one text delta, a
// turn_end carrying the fixture usage, and the terminal agent_settled —
// enough for handle.go's mapEvent to produce an AssistantTextEvent, an
// LlmCallEvent and a ResultEvent, which is what the load harness's
// consumeEvents-equivalent and the runner wait on.
func runTurn(out *bufio.Writer) {
	reply := os.Getenv(replyEnvVar)
	if reply == "" {
		reply = "ok"
	}
	writeEvent(out, map[string]any{"type": "agent_start"})
	writeEvent(out, map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": reply},
	})
	writeEvent(out, map[string]any{"type": "message_end"})
	writeEvent(out, map[string]any{"type": "turn_end", "message": map[string]any{
		"role": "assistant",
		"usage": map[string]any{
			"input":      turnUsageInput,
			"output":     turnUsageOutput,
			"cacheRead":  turnUsageCacheRead,
			"cacheWrite": turnUsageCacheWrite,
		},
	}})
	writeEvent(out, map[string]any{"type": "agent_settled"})
}

// parseArgs pulls the first `-e` path (the boundary extension always loads
// first — ADR-2026-08-12 D1) and an optional FAKEPI_TURN_DELAY-derived delay
// out of argv. Every other flag (--no-extensions, --approve, --session-dir,
// --provider, --model, --session) is accepted and ignored: this stub answers
// the same regardless of them, since none of the load harness's measurements
// depend on pi's own routing/model behavior.
func parseArgs(args []string) (extPath string, turnDelay time.Duration) {
	for i, a := range args {
		if a == "-e" && extPath == "" && i+1 < len(args) {
			extPath = args[i+1]
		}
	}
	if raw := os.Getenv("FAKEPI_TURN_DELAY_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			turnDelay = time.Duration(ms) * time.Millisecond
		}
	}
	return extPath, turnDelay
}

// extensionSHAOf hashes the boundary extension's on-disk bytes exactly the
// way the REAL embedded extension hashes its own source (via import.meta.url)
// at handshake time — so the production handshake verifier
// (extension.go verifyHandshakeSHA) sees a correct SHA for whatever bytes
// materializeExtension actually wrote, without this stub needing to know
// what those bytes are.
func extensionSHAOf(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the -e argv value the load-test harness itself constructed and passed to this very process.
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeEvent(w *bufio.Writer, fields map[string]any) {
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	_, _ = w.Write(b)
	_ = w.WriteByte('\n')
	_ = w.Flush()
}

func marshalTitle(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
