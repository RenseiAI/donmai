package agycli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// agy writes a structured JSONL transcript per conversation under:
//
//	<stateHome>/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript.jsonl
//
// This is an INTERNAL path (note ".system_generated") and may change across agy
// versions. Everything in this file is best-effort: any failure returns nil and
// the provider degrades to the stdout spine. Re-validate on every agy bump.

// defaultStateHomeDir is the agy state root when Options.StateHome is empty.
// agy stores state under ~/.gemini (shared with the legacy gemini CLI layout).
func defaultStateHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini")
}

// resolveStateHome returns the configured StateHome or the default ~/.gemini.
func resolveStateHome(override string) string {
	if override != "" {
		return override
	}
	return defaultStateHomeDir()
}

// brainDir is the directory holding per-conversation subdirectories.
func brainDir(stateHome string) string {
	return filepath.Join(stateHome, "antigravity-cli", "brain")
}

// transcriptPath is the transcript.jsonl path for a conversation id.
func transcriptPath(stateHome, convID string) string {
	return filepath.Join(brainDir(stateHome), convID, ".system_generated", "logs", "transcript.jsonl")
}

// snapshotConvIDs lists the conversation-id subdirectories currently in the
// brain dir. Returns an empty set (not nil) on any error so a missing dir is
// treated as "no conversations yet" rather than failing discovery.
func snapshotConvIDs(stateHome string) map[string]struct{} {
	out := make(map[string]struct{})
	entries, err := os.ReadDir(brainDir(stateHome))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = struct{}{}
		}
	}
	return out
}

// discoverConvID identifies the conversation id created by this run.
//
// Strategy (most-reliable first):
//  1. Prefer last_conversations.json[cwd] when its conv-id is NEW (not in the
//     pre-spawn snapshot) — the cwd→conv map is authoritative and the runner
//     uses a unique per-session cwd.
//  2. Otherwise, if exactly one conv-id appeared since the snapshot, use it.
//  3. Otherwise, among the new conv-ids pick the most-recently-modified
//     transcript (handles a rare concurrent-run tie).
//
// Returns ("", false) when nothing new is found.
func discoverConvID(stateHome, cwd string, before map[string]struct{}) (string, bool) {
	after := snapshotConvIDs(stateHome)
	var fresh []string
	for id := range after {
		if _, existed := before[id]; !existed {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) == 0 {
		return "", false
	}
	freshSet := make(map[string]struct{}, len(fresh))
	for _, id := range fresh {
		freshSet[id] = struct{}{}
	}

	// (1) cwd→conv map, if its id is fresh.
	if id, ok := convIDForCwd(stateHome, cwd); ok {
		if _, isFresh := freshSet[id]; isFresh {
			return id, true
		}
	}

	// (2) exactly one fresh conv-id.
	if len(fresh) == 1 {
		return fresh[0], true
	}

	// (3) newest transcript among the fresh set (deterministic: mtime, then name).
	sort.Slice(fresh, func(i, j int) bool {
		mi := transcriptMtime(stateHome, fresh[i])
		mj := transcriptMtime(stateHome, fresh[j])
		if mi != mj {
			return mi > mj
		}
		return fresh[i] > fresh[j]
	})
	return fresh[0], true
}

func transcriptMtime(stateHome, convID string) int64 {
	fi, err := os.Stat(transcriptPath(stateHome, convID))
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// lastConversations is the shape of cache/last_conversations.json: {cwd: convID}.
func convIDForCwd(stateHome, cwd string) (string, bool) {
	path := filepath.Join(stateHome, "antigravity-cli", "cache", "last_conversations.json")
	data, err := os.ReadFile(path) //nolint:gosec // path derived from config, not user input
	if err != nil {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return "", false
	}
	// Try the path as given, then the symlink-resolved form (macOS /tmp →
	// /private/tmp etc.). The runner's worktree cwd is a real path, so the
	// first match normally wins.
	for _, candidate := range cwdCandidates(cwd) {
		if id, ok := m[candidate]; ok && id != "" {
			return id, true
		}
	}
	return "", false
}

func cwdCandidates(cwd string) []string {
	out := []string{cwd}
	if cleaned := filepath.Clean(cwd); cleaned != cwd {
		out = append(out, cleaned)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil && resolved != cwd {
		out = append(out, resolved)
	}
	return out
}

// transcriptLine is one parsed line of transcript.jsonl. Unknown fields are
// ignored; the schema may grow across agy versions.
type transcriptLine struct {
	StepIndex int                  `json:"step_index"`
	Source    string               `json:"source"`     // USER_EXPLICIT | SYSTEM | MODEL
	Type      string               `json:"type"`       // USER_INPUT | PLANNER_RESPONSE | LIST_DIRECTORY | ...
	Status    string               `json:"status"`     // DONE | ...
	Content   string               `json:"content"`    // assistant prose / tool output
	ToolCalls []transcriptToolCall `json:"tool_calls"` // present on tool-invoking PLANNER_RESPONSE
}

type transcriptToolCall struct {
	Name string                     `json:"name"`
	Args map[string]json.RawMessage `json:"args"`
}

// parseTranscriptFile reads a complete transcript.jsonl file and maps its
// lines to structured events in step order.
//
// AssistantTextEvents are NOT emitted — agy's prose already streamed live
// off stdout, so re-emitting PLANNER_RESPONSE text would duplicate it; only
// the tool structure stdout could not carry is recovered.
//
// This is the whole-file convenience over the incremental transcriptTailer
// (tail.go); live sessions tail the file during the run instead.
func parseTranscriptFile(path string) []agent.Event {
	var events []agent.Event
	t := &transcriptTailer{path: path, emit: func(ev agent.Event) { events = append(events, ev) }}
	t.drain(true)
	return events
}

// transcriptState threads cross-line state so tool-use and tool-result events
// can be id-paired. agy's transcript carries NO shared id between a tool call
// (in a PLANNER_RESPONSE) and its result step, but the steps appear in order:
// PLANNER_RESPONSE(calls A,B) → result(A) → result(B). We synthesize a stable
// ToolUseID per call and FIFO-match each subsequent result step to it.
type transcriptState struct {
	pending []pendingCall
}

type pendingCall struct {
	id   string
	name string
}

// mapLine maps one transcript line to zero or more agent events, updating the
// pending-call queue for id-pairing.
//
//	PLANNER_RESPONSE w/ tool_calls → one ToolUseEvent per call (synthesized id)
//	MODEL-source non-PLANNER step  → ToolResultEvent (FIFO-paired to a pending call)
//	USER_INPUT / CONVERSATION_HISTORY / PLANNER_RESPONSE text → skipped
func (s *transcriptState) mapLine(tl transcriptLine, raw []byte) []agent.Event {
	switch {
	case tl.Type == "USER_INPUT", tl.Type == "CONVERSATION_HISTORY":
		return nil

	case tl.Type == "PLANNER_RESPONSE":
		if len(tl.ToolCalls) == 0 {
			return nil // assistant text already streamed via stdout
		}
		out := make([]agent.Event, 0, len(tl.ToolCalls))
		for j, tc := range tl.ToolCalls {
			id := fmt.Sprintf("agy-step%d-call%d", tl.StepIndex, j)
			s.pending = append(s.pending, pendingCall{id: id, name: tc.Name})
			out = append(out, agent.ToolUseEvent{
				ToolName:  tc.Name,
				ToolUseID: id,
				Input:     decodeToolArgs(tc.Args),
				Raw:       json.RawMessage(raw),
			})
		}
		return out

	case tl.Source == "MODEL":
		// A MODEL-source step that is not a PLANNER_RESPONSE is a tool result
		// (LIST_DIRECTORY, VIEW_FILE, RUN_COMMAND, EDIT_FILE, …). FIFO-pair it to
		// the oldest pending call so the runner can correlate use↔result. The
		// step type stays as ToolName (e.g. "LIST_DIRECTORY"); the paired call's
		// id links it to the originating ToolUseEvent.
		var toolUseID string
		if len(s.pending) > 0 {
			toolUseID = s.pending[0].id
			s.pending = s.pending[1:]
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  tl.Type,
			ToolUseID: toolUseID,
			Content:   tl.Content,
			IsError:   isErrorStatus(tl.Status),
			Raw:       json.RawMessage(raw),
		}}

	default:
		return nil
	}
}

// mapTranscriptLine maps a single line with a fresh (unpaired) state. Retained
// for unit tests that exercise one line in isolation; parseTranscriptFile uses
// a shared transcriptState for cross-line id-pairing.
func mapTranscriptLine(tl transcriptLine, raw []byte) []agent.Event {
	var s transcriptState
	return s.mapLine(tl, raw)
}

// decodeToolArgs converts agy's tool-call args into a map[string]any. agy
// double-encodes arg VALUES as JSON strings (e.g. "AbsolutePath":"\"/p\""), so
// each value is unmarshaled once; on failure the raw token is kept as a string.
func decodeToolArgs(args map[string]json.RawMessage) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		var decoded any
		if err := json.Unmarshal(v, &decoded); err == nil {
			out[k] = decoded
		} else {
			out[k] = string(v)
		}
	}
	return out
}

func isErrorStatus(status string) bool {
	switch status {
	case "", "DONE", "SUCCESS", "COMPLETED":
		return false
	case "ERROR", "FAILED", "FAILURE", "CANCELLED", "CANCELED":
		return true
	default:
		return false // unknown statuses are treated as non-error
	}
}
