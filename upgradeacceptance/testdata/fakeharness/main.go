// Script fake-pi-upgrade-harness: a scripted stand-in for the pi headless
// RPC lane used by the container upgrade acceptance flow.
//
// It speaks the wire shapes provider/harness/pi/handle.go expects — the same
// shapes provider/harness/pi/testdata/fakepi answers for the load harness —
// without running a language model or interpreting an extension file:
//
//   - `--version` prints the pinned pi version and exits, so the provider
//     version probe labels this binary verified.
//   - Otherwise it emits the policy-extension handshake as an
//     extension_ui_request (placeholder donmai-policy-v1, JSON title carrying
//     the per-session token from DONMAI_PI_HANDSHAKE and the sha256 of the
//     -e extension path), answers the handshake reply, then serves the
//     get_state / get_entries / prompt / steer / follow_up / abort commands
//     with canned turns.
//   - It blocks each turn on a trigger file: the turn only completes after
//     the driver touches the path named by FAKE_HARNESS_TRIGGER. Removing
//     the file or writing "abort" to it ends the session instead, so the
//     upgrade flow can hold a live seat across the daemon restart and then
//     release it.
//   - D8 variant: with FAKE_HARNESS_STATE_DIR set, every turn appends one
//     JSONL transcript line under that directory (the session-owned state
//     location), and a resume (argv carrying --session) reports the number
//     of transcript lines it loaded on its first turn.
//
// Exit code is 0 on abort/trigger-release and 2 on protocol misuse, so the
// driver can tell a harness failure from a seat that simply ended.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	uiMarker     = "donmai-policy-v1"
	handshakeKey = "handshake"

	envHandshakeToken = "DONMAI_PI_HANDSHAKE"
	envTrigger        = "FAKE_HARNESS_TRIGGER"
	envStateDir       = "FAKE_HARNESS_STATE_DIR"
	envReply          = "FAKE_HARNESS_REPLY"
)

func main() {
	restoreSessionEnvironment()
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake-pi-upgrade-harness:", err)
		os.Exit(2)
	}
}

// restoreSessionEnvironment restores the session bindings the harness
// deferred to the credential file (the same file the real boundary
// extension restores from at load): the exec environment carries only the
// allowlist, so FAKE_HARNESS_* knobs ride the file's environment section.
// A name already set wins; harness namespaces are never written.
func restoreSessionEnvironment() {
	path := os.Getenv("DONMAI_PI_CREDENTIALS_FILE")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the session credential file the harness named.
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

func run(args []string) error {
	for _, a := range args {
		if a == "--version" {
			fmt.Println("0.80.10")
			return nil
		}
	}
	extPath := extPathOf(args)
	resumed := resumedOf(args)
	token := os.Getenv(envHandshakeToken)
	sha := extensionSHAOf(extPath)
	trigger := os.Getenv(envTrigger)
	stateDir := os.Getenv(envStateDir)
	reply := os.Getenv(envReply)
	if reply == "" {
		reply = "ok"
	}

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

	loaded := 0
	if resumed && stateDir != "" {
		loaded = countTranscriptLines(stateDir)
	}

	sessionID := "fakeharness-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	turns := 0
	for in.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(in.Bytes(), &cmd); err != nil {
			continue
		}
		switch cmd["type"] {
		case "extension_ui_response":
			// Handshake ack/reject. Nothing else to do.
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
			turns++
			text := reply
			if resumed && turns == 1 {
				text = fmt.Sprintf("resumed history=%d %s", loaded, reply)
			}
			if stateDir != "" {
				appendTranscript(stateDir, sessionID, text)
			}
			if decision := gateOnTrigger(trigger); decision == "abort" {
				writeEvent(out, map[string]any{"type": "agent_start"})
				writeEvent(out, map[string]any{
					"type":                  "message_update",
					"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "aborted"},
				})
				writeEvent(out, map[string]any{"type": "message_end"})
				writeEvent(out, map[string]any{"type": "turn_end", "message": map[string]any{
					"role": "assistant",
					"usage": map[string]any{
						"input": 10, "output": 5, "cacheRead": 0, "cacheWrite": 0,
					},
				}})
				writeEvent(out, map[string]any{"type": "agent_settled"})
				return nil
			}
			runTurn(out, text)
		case "abort":
			return nil
		}
	}
	return in.Err()
}

// gateOnTrigger blocks until the trigger file exists (or no trigger is
// configured). It reports "abort" when the file holds the word "abort".
func gateOnTrigger(trigger string) string {
	if trigger == "" {
		return "go"
	}
	for i := 0; i < 6000; i++ {
		raw, err := os.ReadFile(trigger) //nolint:gosec // path comes from the driver's own environment.
		if err == nil {
			if strings.TrimSpace(string(raw)) == "abort" {
				return "abort"
			}
			return "go"
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "abort"
}

func runTurn(out *bufio.Writer, text string) {
	writeEvent(out, map[string]any{"type": "agent_start"})
	writeEvent(out, map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": text},
	})
	writeEvent(out, map[string]any{"type": "message_end"})
	writeEvent(out, map[string]any{
		"type": "turn_end",
		"message": map[string]any{
			"role": "assistant",
			"usage": map[string]any{
				"input": 1200, "output": 80, "cacheRead": 48000, "cacheWrite": 512,
			},
		},
	})
	writeEvent(out, map[string]any{"type": "agent_settled"})
}

func appendTranscript(stateDir, sessionID, text string) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	line, _ := json.Marshal(map[string]any{"sessionId": sessionID, "text": text, "at": time.Now().UTC().Format(time.RFC3339Nano)})
	f, err := os.OpenFile(filepath.Join(stateDir, "transcript.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // append-only transcript under the driver's own state dir.
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

func countTranscriptLines(stateDir string) int {
	raw, err := os.ReadFile(filepath.Join(stateDir, "transcript.jsonl")) //nolint:gosec // read-back of the transcript this harness wrote.
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func extPathOf(args []string) string {
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func resumedOf(args []string) bool {
	for i, a := range args {
		if a == "--session" && i+1 < len(args) {
			return true
		}
	}
	return false
}

func extensionSHAOf(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is the -e argv value the driver constructed for this process.
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
