package codex

// This file reads the native session file a host interactive session leaves
// behind. Codex persists one JSONL rollout per thread under
// <home>/sessions/<date>/rollout-*-<threadID>.jsonl, and appends a
// token_usage_record per model call carrying both that call's usage and the
// thread's cumulative thread_token_usage. The interactive PTY surface emits
// no structured usage of its own, so at exit the last cumulative record is
// the session's real token total.
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
// line shape is skipped without decoding its payload.
type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		ThreadID         string             `json:"thread_id"`
		Usage            *rolloutTokenUsage `json:"usage"`
		ThreadTokenUsage *rolloutTokenUsage `json:"thread_token_usage"`
	} `json:"payload"`
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

// readRolloutUsage scans path for the last token_usage_record whose payload
// names threadID and returns its cumulative thread total. Records naming a
// different thread — a forked subagent's appended segment, or any other
// thread sharing the file — never move the total. A nil usage is returned
// when no record for the thread exists, so the caller can tell "nothing
// recorded" from "recorded zero".
//
// input excludes cache read and cache write, matching CostData's accounting
// rule (agent/types.go): the recorded cumulative input already carries both
// buckets separately, so the reported input subtracts them. Reasoning is a
// count inside output and rides along unchanged.
//
// Malformed lines are skipped, not fatal: the file is append-only native
// state that may be mid-flush when it is read at exit.
func readRolloutUsage(path, threadID string) *rolloutUsageTotals {
	if path == "" || threadID == "" {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // path is the runner-owned session file under test control
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
			cumulative = line.Payload.Usage
		}
		if cumulative == nil {
			continue
		}
		totals = &rolloutUsageTotals{
			InputTokens:       max(cumulative.InputTokens-cumulative.CachedInputTokens-cumulative.CacheWriteInputTokens, 0),
			CachedInputTokens: cumulative.CachedInputTokens,
			CacheWriteTokens:  cumulative.CacheWriteInputTokens,
			OutputTokens:      cumulative.OutputTokens,
			ReasoningTokens:   cumulative.ReasoningOutputTokens,
			Found:             true,
		}
	}
	return totals
}

// findThreadRollout locates the session file for threadID under home: the
// rollout-*-<threadID>.jsonl entry anywhere under the sessions subtree. The
// thread id is the filename's last dash group, so the match is on the
// "-<threadID>.jsonl" suffix rather than a substring. The newest match wins
// by modification time (then name) when more than one exists.
func findThreadRollout(home, threadID string) string {
	if home == "" || threadID == "" {
		return ""
	}
	root := filepath.Join(home, codexSessionStateSubdir)
	var best string
	var bestMtime int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if !strings.HasSuffix(strings.TrimSuffix(name, ".jsonl"), "-"+threadID) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		mtime := info.ModTime().UnixNano()
		if best == "" || mtime > bestMtime || (mtime == bestMtime && path > best) {
			best, bestMtime = path, mtime
		}
		return nil
	})
	return best
}

// rolloutUsageCost reads the thread's session file under home and converts
// its last cumulative token total into the terminal cost. It returns nil
// when the file or the thread's records are absent, so the caller keeps the
// session's previous cost instead of reporting a zero that never ran.
//
// Host sessions run under the operator's own login, so the dollar price is
// deliberately left at zero: real tokens, zero marginal price. NumTurns is
// left unset — the file records model calls, not runner turns.
func rolloutUsageCost(home, threadID string) *agent.CostData {
	totals := readRolloutUsage(findThreadRollout(home, threadID), threadID)
	if totals == nil {
		return nil
	}
	return &agent.CostData{
		InputTokens:       totals.InputTokens,
		OutputTokens:      totals.OutputTokens,
		CachedInputTokens: totals.CachedInputTokens,
		CacheWriteTokens:  totals.CacheWriteTokens,
		ReasoningTokens:   totals.ReasoningTokens,
	}
}
