package pi

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Interactive transcript tailing.
//
// An interactive pi session writes its own session transcript as JSONL under
// the per-session state dir (`--session-dir`, i.e. layout.root — pi appends
// one `<timestamp>_<id>.jsonl` file per session directly under it, older
// builds under a `sessions/` subdirectory). The headless RPC lane maps live
// wire events to agent events; the interactive PTY lane has no wire — so this
// file tails those transcript files and maps each new entry to the SAME agent
// event types the headless mapper emits (assistant text, tool calls, tool
// results, per-turn usage), which the interactive runner forwards through the
// existing activity sink.
//
// Posture: best-effort and rate-limited, never blocking the PTY. The tailer
// runs on its own goroutine, polls on a 1s ticker, caps files / lines /
// events per sweep, and emits via a non-blocking send — when the consumer is
// slow, transcript events are dropped, never queued behind the terminal.
const (
	// interactiveTranscriptPollInterval is the cadence at which the tailer
	// re-scans the state dir. One second keeps activities near-live without
	// measurable IO pressure (a stat + occasional bounded read per file).
	interactiveTranscriptPollInterval = time.Second

	// interactiveTranscriptMaxFilesPerSweep bounds how many transcript files
	// one sweep opens. A session normally owns exactly one; the cap only
	// matters when stale files accumulate.
	interactiveTranscriptMaxFilesPerSweep = 8

	// interactiveTranscriptMaxLinesPerSweep bounds how many new JSONL lines
	// one sweep parses. A burst beyond it is consumed on later ticks.
	interactiveTranscriptMaxLinesPerSweep = 200

	// interactiveTranscriptMaxEventsPerSweep bounds how many agent events one
	// sweep emits — the rate limit that keeps a chatty session from
	// flooding the activity buffer. Excess mapped events are dropped with
	// the offsets still advanced, so the tail never replays them.
	interactiveTranscriptMaxEventsPerSweep = 50

	// interactiveTranscriptMaxReadBytes bounds one file read per sweep so a
	// huge single-tick append cannot spike memory; the remainder is picked
	// up on the next tick.
	interactiveTranscriptMaxReadBytes = 4 << 20
)

// interactiveTranscriptTailer tracks per-file read offsets and already-seen
// transcript entry IDs across sweeps. It is not safe for concurrent use;
// the single tailer goroutine owns it.
type interactiveTranscriptTailer struct {
	stateDir string
	offsets  map[string]int64
	seen     map[string]struct{}
}

func newInteractiveTranscriptTailer(stateDir string) *interactiveTranscriptTailer {
	return &interactiveTranscriptTailer{
		stateDir: filepath.Clean(stateDir),
		offsets:  make(map[string]int64),
		seen:     make(map[string]struct{}),
	}
}

// sweep reads newly appended transcript lines and maps them to agent events
// in file order. Unreadable files, torn trailing lines, and unparsable lines
// are skipped without moving the offsets that guard them, so one bad read
// never wedges the tail nor replays it.
func (t *interactiveTranscriptTailer) sweep() []agent.Event {
	if t.stateDir == "" {
		return nil
	}
	paths := listInteractiveTranscriptFiles(t.stateDir, interactiveTranscriptMaxFilesPerSweep)
	var out []agent.Event
	lines := 0
	for _, path := range paths {
		if len(out) >= interactiveTranscriptMaxEventsPerSweep || lines >= interactiveTranscriptMaxLinesPerSweep {
			break
		}
		newLines, nextOffset := readNewTranscriptLines(path, t.offsets[path])
		t.offsets[path] = nextOffset
		for _, line := range newLines {
			if lines >= interactiveTranscriptMaxLinesPerSweep {
				break
			}
			lines++
			for _, ev := range mapInteractiveTranscriptLine(line, t.seen) {
				if len(out) >= interactiveTranscriptMaxEventsPerSweep {
					break
				}
				out = append(out, ev)
			}
		}
	}
	return out
}

// runInteractiveTranscriptTailer polls the state dir until done closes,
// emitting newly mapped transcript events via emit, then performs one final
// sweep so trailing writes that landed with the session exit still reach the
// sink. Every emit must be non-blocking — this goroutine never slows the PTY.
func runInteractiveTranscriptTailer(stateDir string, done <-chan struct{}, emit func(agent.Event)) {
	tailer := newInteractiveTranscriptTailer(stateDir)
	ticker := time.NewTicker(interactiveTranscriptPollInterval)
	defer ticker.Stop()
	for {
		for _, ev := range tailer.sweep() {
			emit(ev)
		}
		select {
		case <-done:
			for _, ev := range tailer.sweep() {
				emit(ev)
			}
			return
		case <-ticker.C:
		}
	}
}

// listInteractiveTranscriptFiles returns up to max .jsonl files under dir in
// deterministic (path) order, recursing into subdirectories (older pi builds
// nest transcripts under sessions/) but never into the per-session config
// home or the injected-extensions dir, which hold no transcripts.
func listInteractiveTranscriptFiles(dir string, limit int) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case agentHomeDir, injectedExtensionsDir:
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= limit || !(entry.Type().IsRegular() || entry.Type() == 0) {
			return nil
		}
		if filepath.Ext(path) != ".jsonl" {
			return nil
		}
		out = append(out, path)
		return nil
	})
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// readNewTranscriptLines reads bytes appended to path since offset and
// returns the complete (newline-terminated) lines plus the offset that
// consumes exactly them. A torn trailing line without its newline stays
// unread until the writer completes it; a shrunk file (rotation/rewrite)
// restarts from zero. Best-effort: any IO failure returns no lines and
// leaves the offset untouched.
func readNewTranscriptLines(path string, offset int64) ([][]byte, int64) {
	//nolint:gosec // G304: path is discovered by walking the session's own state dir.
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, offset
	}
	if st.Size() < offset {
		offset = 0
	}
	if st.Size() == offset {
		return nil, offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	data, err := io.ReadAll(io.LimitReader(f, interactiveTranscriptMaxReadBytes))
	if err != nil || len(data) == 0 {
		return nil, offset
	}
	var lines [][]byte
	consumed := int64(0)
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimRight(data[:idx], "\r")
		consumed += int64(idx + 1)
		data = data[idx+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines = append(lines, line)
	}
	return lines, offset + consumed
}

// interactiveTranscriptRecord is one decoded transcript JSONL line. Type is
// the discriminant pi stamps ("session", "session_info", "model_change",
// "message", …); ID dedupes re-reads; Message carries the payload for
// "message" lines.
type interactiveTranscriptRecord struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Message json.RawMessage `json:"message"`
}

// mapInteractiveTranscriptLine maps one transcript line to the same agent
// event types the headless mapper emits for the equivalent turn activity:
// assistant text chunks, tool calls, tool results, and per-turn usage. Only
// "message" lines map; session metadata, user messages (the initial prompt
// already travels as a lifecycle marker), and unknown shapes map to nothing.
// Entries already in seen are skipped so a re-read never double-emits.
func mapInteractiveTranscriptLine(line []byte, seen map[string]struct{}) []agent.Event {
	var rec interactiveTranscriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil
	}
	if rec.Type != "message" || len(rec.Message) == 0 {
		return nil
	}
	if rec.ID != "" {
		if _, dup := seen[rec.ID]; dup {
			return nil
		}
		seen[rec.ID] = struct{}{}
	}
	var msg interactiveTranscriptMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return nil
	}
	raw := string(line)
	switch msg.Role {
	case "assistant":
		var out []agent.Event
		for _, part := range msg.Content {
			switch part.Type {
			case "text":
				if part.Text == "" {
					continue
				}
				out = append(out, agent.AssistantTextEvent{Text: part.Text, Raw: raw})
			case "toolCall":
				if part.Name == "" {
					continue
				}
				input := part.Arguments
				if input == nil {
					input = map[string]any{}
				}
				out = append(out, agent.ToolUseEvent{
					ToolName:  part.Name,
					ToolUseID: part.ID,
					Input:     input,
					Raw:       raw,
				})
			}
		}
		if msg.Usage != nil {
			out = append(out, agent.LlmCallEvent{
				System:          msg.Provider,
				Model:           msg.Model,
				InputTokens:     int64(msg.Usage.Input),
				OutputTokens:    int64(msg.Usage.Output),
				UsageSource:     agent.LlmUsageProvider,
				ObservedCostUsd: transcriptCost(msg.Usage.Cost.Total),
				TurnCompleted:   true,
			})
		}
		return out
	case "toolResult":
		var b bytes.Buffer
		for _, part := range msg.Content {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			b.WriteString(part.Text)
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  msg.ToolName,
			ToolUseID: msg.ToolCallID,
			Content:   b.String(),
			IsError:   msg.IsError,
			Raw:       raw,
		}}
	default:
		return nil
	}
}

// interactiveTranscriptMessage is the payload of a transcript "message" line,
// verified against the real pi session transcript (pi 0.85.x): assistant
// content parts carry {"type":"text","text"} or
// {"type":"toolCall","id","name","arguments"}; tool results arrive as
// role:"toolResult" with text content parts; per-turn usage rides the
// assistant message.
type interactiveTranscriptMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"content"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Usage      *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
		Cost   struct {
			Total *float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

// transcriptCost mirrors the headless observedPiCost guard: only a finite,
// non-negative reported total becomes an ObservedCostUsd.
func transcriptCost(total *float64) *float64 {
	if total == nil || math.IsNaN(*total) || math.IsInf(*total, 0) || *total < 0 {
		return nil
	}
	return total
}
