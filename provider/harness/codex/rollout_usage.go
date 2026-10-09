package codex

// This file reads the native session files a host interactive session leaves
// behind. Codex persists one JSONL rollout per thread under
// <home>/sessions/<date>/rollout-*-<threadID>.jsonl, and appends a
// token_usage_record per model call carrying both that call's usage and the
// thread's cumulative thread_token_usage. The interactive PTY surface emits
// no structured usage of its own, so at exit the last cumulative record is
// the session's real token total.
//
// A session's spend is not confined to its own file: subagents run as
// separate threads in their own rollout files, each named by a session_meta
// record carrying source.subagent.thread_spawn.parent_thread_id. Each
// subagent's cumulative starts fresh (its first record's usage equals its
// thread total), and the parent's cumulative excludes subagent spend — so
// the session total is the parent's last cumulative PLUS every descendant
// subagent's own last cumulative, transitively. Reading only the parent's
// file would report a large undercount as a complete total.
//
// Everything here is best-effort: any failure (no file yet, unreadable or
// malformed content) returns nil and the session keeps its previous cost,
// never a fabricated zero. Re-validate against a real session file on every
// CLI bump.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// codexSessionStateSubdir is defined in orphan_sweep.go: the one home
// subdirectory SweepOrphans preserves, because it carries the rollout files
// a later resume needs.

// rolloutTokenUsage is the token-count shape on a token_usage_record line.
// thread_token_usage is cumulative for the thread; usage is that call's.
type rolloutTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// rolloutLine is the envelope of one JSONL line in a rollout file. Only the
// type and (for token_usage_record lines) the payload matter; every other
// line shape is skipped without decoding its payload. session_meta lines
// carry the subagent parent link in Payload.Source.
type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		ThreadID         string             `json:"thread_id"`
		Usage            *rolloutTokenUsage `json:"usage"`
		ThreadTokenUsage *rolloutTokenUsage `json:"thread_token_usage"`
		Source           *rolloutSource     `json:"source"`
	} `json:"payload"`
}

// rolloutSource is the session_meta origin marker. A subagent-spawned thread
// carries source.subagent.thread_spawn.parent_thread_id naming the thread
// that spawned it; a top-level session carries no source (or a non-subagent
// one).
type rolloutSource struct {
	Subagent *rolloutSubagentSource `json:"subagent"`
}

type rolloutSubagentSource struct {
	ThreadSpawn *rolloutThreadSpawn `json:"thread_spawn"`
}

type rolloutThreadSpawn struct {
	ParentThreadID string `json:"parent_thread_id"`
}

// rolloutUsageTotals is the cumulative thread total read from the last
// token_usage_record that belongs to the thread.
type rolloutUsageTotals struct {
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	Found             bool
}

func (t *rolloutUsageTotals) add(o rolloutUsageTotals) {
	t.InputTokens += o.InputTokens
	t.CachedInputTokens += o.CachedInputTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.OutputTokens += o.OutputTokens
	t.ReasoningTokens += o.ReasoningTokens
	t.Found = true
}

// readRolloutUsage scans path for the last token_usage_record whose payload
// names threadID and returns its cumulative thread total. Records naming a
// different thread — or any other thread sharing the file — never move the
// total. A nil usage is returned when no record for the thread exists, so
// the caller can tell "nothing recorded" from "recorded zero".
//
// input excludes cache read and cache write, matching CostData's accounting
// rule (agent/types.go): the recorded cumulative input already carries both
// buckets separately, so the reported input subtracts them. Reasoning is a
// count inside output and rides along unchanged.
//
// A record whose counts are internally impossible (any negative field, or
// cached + cache-write exceeding input) is unreported: the file is native
// state that may be mid-flush when read at exit, and posting a negative
// count on the wire — or a total derived from a torn record — would present
// corruption as a measurement. The last CONSISTENT cumulative wins; when no
// consistent record exists the thread reads as unknown.
//
// Malformed lines are skipped, not fatal: the file is append-only native
// state that may be mid-flush when it is read at exit.
func readRolloutUsage(path, threadID string) *rolloutUsageTotals {
	if path == "" || threadID == "" {
		return nil
	}
	f, err := openRolloutFile(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var totals *rolloutUsageTotals
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var line rolloutLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "token_usage_record" || line.Payload.ThreadID != threadID {
			continue
		}
		cumulative := line.Payload.ThreadTokenUsage
		if cumulative == nil {
			// No cumulative total on this record: skip it. A per-call
			// usage without the thread total is one call's count, and
			// reporting it as the thread TOTAL would undercount while
			// looking exact. The writer always carries the cumulative
			// field, so skipping only drops shapes it never emits.
			continue
		}
		fresh := cumulative.InputTokens - cumulative.CachedInputTokens - cumulative.CacheWriteInputTokens
		if cumulative.InputTokens < 0 || cumulative.CachedInputTokens < 0 ||
			cumulative.CacheWriteInputTokens < 0 || cumulative.OutputTokens < 0 ||
			cumulative.ReasoningOutputTokens < 0 || fresh < 0 {
			// Impossible counts (negative fields, or cache buckets
			// exceeding input) are corruption, not measurement: skip
			// the record and keep the last consistent cumulative.
			continue
		}
		totals = &rolloutUsageTotals{
			InputTokens:       fresh,
			CachedInputTokens: cumulative.CachedInputTokens,
			CacheWriteTokens:  cumulative.CacheWriteInputTokens,
			OutputTokens:      cumulative.OutputTokens,
			ReasoningTokens:   cumulative.ReasoningOutputTokens,
			Found:             true,
		}
	}
	return totals
}

// rolloutParentThreadID reads path's session_meta record and reports the
// parent thread id when the file belongs to a subagent-spawned thread:
// source.subagent.thread_spawn.parent_thread_id. It returns "" for a
// top-level session file, a file with no session_meta, or any read failure.
// Only the FIRST session_meta record is read: it is written once when the
// thread starts, before any usage record.
func rolloutParentThreadID(path string) string {
	f, err := openRolloutFile(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var line rolloutLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "session_meta" {
			continue
		}
		if src := line.Payload.Source; src != nil && src.Subagent != nil &&
			src.Subagent.ThreadSpawn != nil {
			return strings.TrimSpace(src.Subagent.ThreadSpawn.ParentThreadID)
		}
		return ""
	}
	return ""
}

// listRolloutFiles returns every candidate session file under home: the
// rollout-*.jsonl entries anywhere under the sessions subtree. FIFOs,
// devices, and anything else that is not a regular file are excluded: the
// session file is runner-owned state, but a substituted non-regular entry
// at a walked path would block the open (a FIFO waits for a writer) before
// any read happens — and the read runs inside ptycli run() before cleanup,
// so a hang there would leave the terminal unsent and the session's
// resources unreleased.
func listRolloutFiles(home string) []string {
	if home == "" {
		return nil
	}
	root := filepath.Join(home, codexSessionStateSubdir)
	var out []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if info, err := entry.Info(); err != nil || !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// findThreadRollout locates the session file for threadID under home: the
// rollout-*-<threadID>.jsonl entry anywhere under the sessions subtree. The
// thread id is the filename's last dash group, so the match is on the
// "-<threadID>.jsonl" suffix rather than a substring. The newest match wins
// by modification time (then name) when more than one exists. Non-regular
// entries are never candidates (see listRolloutFiles).
func findThreadRollout(home, threadID string) string {
	if home == "" || threadID == "" {
		return ""
	}
	var best string
	var bestMtime int64
	for _, path := range listRolloutFiles(home) {
		name := filepath.Base(path)
		if !strings.HasSuffix(strings.TrimSuffix(name, ".jsonl"), "-"+threadID) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		mtime := info.ModTime().UnixNano()
		if best == "" || mtime > bestMtime || (mtime == bestMtime && path > best) {
			best, bestMtime = path, mtime
		}
	}
	return best
}

// subagentThreadIDs returns the thread ids of every rollout file under home
// that descends from threadID: files whose session_meta names threadID as
// the subagent parent, transitively. A file that names no parent, names an
// unknown parent, or cannot be read contributes nothing.
func subagentThreadIDs(home, threadID string) []string {
	if home == "" || threadID == "" {
		return nil
	}
	parentOf := make(map[string]string, 16)
	var threads []string
	for _, path := range listRolloutFiles(home) {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		thread := threadIDFromRolloutName(name)
		if thread == "" {
			continue
		}
		parentOf[thread] = rolloutParentThreadID(path)
		threads = append(threads, thread)
	}
	var out []string
	for _, thread := range threads {
		if thread == threadID {
			continue
		}
		for parent := parentOf[thread]; parent != ""; {
			if parent == threadID {
				out = append(out, thread)
				break
			}
			next, ok := parentOf[parent]
			if !ok {
				break
			}
			parent = next
		}
	}
	return out
}

// threadIDFromRolloutName extracts the thread id from a rollout filename:
// everything after the "rollout-<timestamp>-" prefix. The timestamp
// itself holds dashes (rollout-2026-10-09T00-00-00-<threadID>), so the
// thread id cannot be the last dash group in general — it is whatever
// follows the fixed prefix. It returns "" when the name carries no
// thread id to take.
func threadIDFromRolloutName(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	if !strings.HasPrefix(name, "rollout-") {
		return ""
	}
	rest := strings.TrimPrefix(name, "rollout-")
	// The timestamp prefix is "YYYY-MM-DDTHH-MM-SS": date, a "T", then
	// time. The thread id starts after the first dash past the "T"
	// separator's time part — i.e. split the timestamp off field-wise:
	// <date>-<time>-<thread...> is ambiguous in general, so locate the
	// boundary structurally: the date is 10 chars (YYYY-MM-DD), then
	// "T", then the time's HH-MM-SS (8 chars), then "-".
	if len(rest) > len("2026-10-09T00-00-00") && rest[10] == 'T' && rest[19] == '-' {
		return rest[20:]
	}
	// Unknown timestamp shape: fall back to the last dash group only
	// when the remainder holds no further structure to parse.
	idx := strings.LastIndex(rest, "-")
	if idx < 0 || idx+1 >= len(rest) {
		return ""
	}
	return rest[idx+1:]
}

// rolloutUsageCost reads the thread's session file under home and converts
// its cumulative token total into the terminal cost — the thread's own last
// cumulative PLUS every descendant subagent thread's own last cumulative
// (each subagent's total starts fresh in its own file, and the parent's
// cumulative excludes subagent spend). It returns nil when the file or the
// thread's records are absent, so the caller keeps the session's previous
// cost instead of reporting a zero that never ran.
//
// A thread whose records are all inconsistent (see readRolloutUsage), or a
// subagent file that cannot be credited, contributes nothing: the reported
// total covers exactly the records that read as measurements, and a session
// with no consistent record at all reads as unknown.
//
// Host sessions run under the operator's own login, so the dollar price is
// deliberately left at zero: real tokens, zero marginal price. NumTurns is
// left unset — the file records model calls, not runner turns.
func rolloutUsageCost(home, threadID string) *agent.CostData {
	if home == "" || threadID == "" {
		return nil
	}
	var total rolloutUsageTotals
	if own := readRolloutUsage(findThreadRollout(home, threadID), threadID); own != nil {
		total.add(*own)
	}
	for _, sub := range subagentThreadIDs(home, threadID) {
		if sub := readRolloutUsage(findThreadRollout(home, sub), sub); sub != nil {
			total.add(*sub)
		}
	}
	if !total.Found {
		return nil
	}
	return &agent.CostData{
		InputTokens:       total.InputTokens,
		OutputTokens:      total.OutputTokens,
		CachedInputTokens: total.CachedInputTokens,
		CacheWriteTokens:  total.CacheWriteTokens,
		ReasoningTokens:   total.ReasoningTokens,
	}
}
