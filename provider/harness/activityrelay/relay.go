package activityrelay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Relay event names, in the snake_case vocabulary the CLIs themselves use
// on the wire (codex hook_event_name values; claude matches them
// case-insensitively through the same four moments).
const (
	EventSessionStart     = "session_start"
	EventUserPromptSubmit = "user_prompt_submit"
	EventStop             = "stop"
	EventPermissionRequest = "permission_request"
)

// relayEvents is the allowlist of moments a relay line may carry. Anything
// else is a foreign writer or a newer harness event this version does not
// map, and it is dropped rather than forwarded.
var relayEvents = map[string]struct{}{
	EventSessionStart:      {},
	EventUserPromptSubmit:  {},
	EventStop:              {},
	EventPermissionRequest: {},
}

// eventAliases maps the wire casings the CLIs report (claude's
// SessionStart, codex's sessionStart) to the canonical snake_case moment.
// The bundled script always writes the canonical form; this is for lines
// (or future harnesses) that carry a CLI-supplied variant.
var eventAliases = map[string]string{
	"sessionstart":      EventSessionStart,
	"userpromptsubmit":  EventUserPromptSubmit,
	"stop":              EventStop,
	"permissionrequest": EventPermissionRequest,
}

// ValidEvent reports whether name is one of the four lifecycle moments the
// relay maps to runner events.
func ValidEvent(name string) bool {
	_, ok := relayEvents[CanonicalEvent(name)]
	return ok
}

// CanonicalEvent trims name and folds it to the canonical snake_case
// moment, resolving either wire casing. Unknown names fold to their
// lowercase form and fail ValidEvent downstream.
func CanonicalEvent(name string) string {
	folded := strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := eventAliases[strings.ReplaceAll(folded, "_", "")]; ok {
		return canonical
	}
	return folded
}

const (
	// relayScriptFile is the POSIX script every hook entry invokes.
	relayScriptFile = "activity-relay.sh"
	// relayEventsFile is the JSONL file the script appends to and the
	// harness tails.
	relayEventsFile = "events.jsonl"
	// relayScriptTimeoutSeconds bounds the hook from the CLI's side. The
	// script performs one append and exits; ten seconds is already
	// enormous for it, matching the notice channel's bound. An overrun
	// surfaces as a missing telemetry line, never as a stalled turn —
	// the CLI abandons the hook and continues the session.
	relayScriptTimeoutSeconds = 10
)

// Relay owns one session's hook-telemetry drop: the relay script plus the
// JSONL file the hooks append to. The directory is private to the session
// (a temp dir, never the worktree) so the agent can neither read its own
// telemetry nor commit the hook definition by accident.
type Relay struct {
	dir string
}

// New creates the relay directory and writes the relay script into it.
func New() (*Relay, error) {
	dir, err := os.MkdirTemp("", "donmai-activity-relay-")
	if err != nil {
		return nil, fmt.Errorf("activity relay: create dir: %w", err)
	}
	r := &Relay{dir: dir}
	script := scriptSource(filepath.Join(dir, relayEventsFile))
	if err := os.WriteFile(r.path(relayScriptFile), []byte(script), 0o700); err != nil { //nolint:gosec // the script is executed by the CLI; it is owner-only
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("activity relay: write script: %w", err)
	}
	return r, nil
}

// Dir returns the relay directory.
func (r *Relay) Dir() string { return r.dir }

// ScriptPath returns the relay script path for hook commands to invoke.
func (r *Relay) ScriptPath() string { return r.path(relayScriptFile) }

// EventsPath returns the JSONL file the hooks append to.
func (r *Relay) EventsPath() string { return r.path(relayEventsFile) }

// TimeoutSeconds is the per-hook timeout harnesses declare for telemetry
// entries.
func TimeoutSeconds() int { return relayScriptTimeoutSeconds }

func (r *Relay) path(name string) string { return filepath.Join(r.dir, name) }

// Close removes the relay directory. Idempotent.
func (r *Relay) Close() error {
	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("activity relay: remove dir: %w", err)
	}
	return nil
}

// Command renders the hook command line for one lifecycle moment: the
// relay script invoked through /bin/sh with the canonical event name as
// its only argument. The event rides argv — never stdin parsing — so the
// script cannot misattribute a fire.
func (r *Relay) Command(event string) string {
	return "/bin/sh " + shellQuote(r.ScriptPath()) + " " + CanonicalEvent(event)
}

// scriptSource renders the relay script. It takes the events file by value
// and bakes the absolute path in, so the hook needs no environment: hook
// processes inherit whatever the CLI hands them.
//
// The script appends exactly one line — {"v":1,"harness":<argv2>,"event":
// <argv1>,"ts":<utc>,"payload":<stdin verbatim>} — and prints nothing.
// stdin is spliced verbatim: when it is valid JSON the line stays valid
// JSON; when it is not (a CLI that writes a bare marker), the payload is
// still preserved after a best-effort quote. Either way the Go reader
// below decides what the line means; the shell never parses.
func scriptSource(eventsPath string) string {
	p := shellQuote(eventsPath)
	return `#!/bin/sh
# donmai activity-telemetry relay. Generated per session; do not edit.
# Observe-only: appends one JSON line per hook fire and prints nothing,
# so the CLI treats every fire as an abstention. Exits 0 on every path —
# a telemetry hook must never fail a turn, even when the disk is full.
set -u
event="${1:?}"
out=` + p + `
payload="$(cat 2>/dev/null || true)"
ts="$(date -u +%FT%TZ 2>/dev/null || echo "")"
case "$payload" in
  ""|"{"*|"["*|'"'*|true|false|null|-|[0-9]*)
    ;; 
  *)
    payload="$(printf '%s' "$payload" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n' ' ')"
    payload="\"$payload\""
    ;;
esac
if [ -z "$payload" ]; then payload="null"; fi
printf '{"v":1,"event":"%s","ts":"%s","payload":%s}\n' "$event" "$ts" "$payload" >>"$out" 2>/dev/null || true
exit 0
`
}

// shellQuote wraps s in single quotes for /bin/sh, escaping any embedded
// single quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Line is one parsed relay line.
type Line struct {
	// Event is the canonical lifecycle moment.
	Event string
	// Payload is the hook's stdin verbatim, or nil when the line carried
	// none or it was not JSON.
	Payload json.RawMessage
}

// ParseLine parses one relay file line. ok is false for blank lines,
// torn writes, foreign content, and events outside the allowlist — all
// ordinary states of a concurrently appended file, never errors. err is
// non-nil only when the line is structurally a relay line with an
// unreadable envelope.
func ParseLine(line []byte) (parsed Line, ok bool, err error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return Line{}, false, nil
	}
	var envelope struct {
		V       int             `json:"v"`
		Event   string          `json:"event"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return Line{}, false, nil
	}
	if envelope.V != 1 {
		return Line{}, false, nil
	}
	event := CanonicalEvent(envelope.Event)
	if !ValidEvent(event) {
		return Line{}, false, nil
	}
	out := Line{Event: event}
	if len(bytes.TrimSpace(envelope.Payload)) > 0 {
		out.Payload = append(json.RawMessage(nil), envelope.Payload...)
	}
	return out, true, nil
}

// Tailer tracks the relay file read offset across sweeps. It is not safe
// for concurrent use; a single tailer goroutine owns it.
type Tailer struct {
	path   string
	offset int64
}

// NewTailer returns a tailer for path, typically Relay.EventsPath().
func NewTailer(path string) *Tailer { return &Tailer{path: path} }

// Sweep reads newly appended complete lines and maps each to the runner
// event for its moment. A torn trailing line stays unread until the
// writer completes it; the offset never advances past it. Caps bound one
// sweep so a burst defers to later ticks rather than flooding the
// consumer.
func (t *Tailer) Sweep(maxLines, maxEvents int) []agent.Event {
	f, err := os.Open(t.path) //nolint:gosec // path is the harness's own relay dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() < t.offset {
		t.offset = 0
	}
	if info.Size() == t.offset {
		return nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	var events []agent.Event
	reader := bufio.NewReader(f)
	lines := 0
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) == 0 {
			return events
		}
		if raw[len(raw)-1] != '\n' {
			return events
		}
		if maxLines > 0 && lines >= maxLines {
			return events
		}
		if maxEvents > 0 && len(events) >= maxEvents {
			return events
		}
		lines++
		if parsed, ok, _ := ParseLine(raw); ok {
			events = append(events, agent.HarnessLifecycleEvent(parsed.Event))
		}
		t.offset += int64(len(raw))
		if err != nil {
			return events
		}
	}
}

// RelayPollInterval is the cadence at which harness tailers re-read the
// relay file. One second keeps lifecycle markers near-live without
// measurable IO pressure.
const RelayPollInterval = time.Second

// MaxLinesPerSweep bounds how many new relay lines one sweep consumes,
// and MaxEventsPerSweep bounds how many runner events it emits.
const (
	MaxLinesPerSweep  = 100
	MaxEventsPerSweep = 16
)
