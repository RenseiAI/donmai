package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Transcript usage accounting for interactive sessions.
//
// An interactive session runs the harness's own REPL under a PTY: no
// stream-json wire exists, so the terminal ResultEvent carries no cost and
// the session would otherwise end with zero usage. The durable record of
// what the session spent is the harness's own JSONL transcript — the same
// file the Stop hook's stdin names via transcript_path — whose assistant
// lines carry message.usage (input, cache creation, cache read, output).
// Summing those per-message totals at exit restores the real token counts
// for a session that ran under the operator's own login, where no other
// meter observes the spend.
//
// Deduping is on the message id: the transcript repeats a content block
// under several message ids or repeats one message's usage across lines,
// and counting the usage per line would double count. Each message id is
// credited once; a line with no id is credited by its content hash, so an
// id-less repeat is still counted once while two genuinely different id-less
// lines both count. A restart or resume re-reads the same file, so the same
// rule keeps a re-read from counting twice: the totals are a property of
// the transcript, not of how many times it was read.
//
// One session can write more than one transcript, and a transcript can hold
// turns this session did not run, so the exit totals are taken over every
// transcript the session named, counting only this session's turns:
//
//   - Clearing the conversation starts a new transcript under a new session
//     id; the hook then names the new file. Every transcript path the session
//     named is summed (one shared dedupe set), so the turns before the switch
//     still count.
//   - Subagent turns are written to their own files beside the transcript
//     (<transcript minus .jsonl>/subagents/*.jsonl), not into it. They are
//     summed with it: a session that delegates spends most of its tokens
//     there.
//   - Resuming an earlier conversation continues its file, whose earlier
//     turns were spent before this session existed. A line stamped before the
//     session began is not this session's spend and is skipped.
//
// A count no model call produces (negative, or a total that would overflow)
// means the file cannot be trusted: the session's usage then reads as
// unreported rather than as a wrong number. A transcript path that is not a
// regular file is unreadable the same way, and is never opened for reading
// (a FIFO or device would block or never end).

// transcriptUsageLine is the narrow slice of the transcript schema this
// accounting reads: assistant lines with a message id and its usage. Every
// other record type — user turns, tool results, system summaries, hook
// bookkeeping — carries no model spend and is ignored, as are assistant
// lines whose usage is all zero (a streamed repeat that carried no new
// tokens). Unknown fields stay unread on purpose: the fewer fields depended
// on, the fewer ways an upstream schema change turns into a miscount.
type transcriptUsageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Role  string `json:"role"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// errTranscriptUntrusted reports a transcript whose usage cannot be taken at
// face value: a count no model call produces, or a path that is not a regular
// file. The session's usage then reads as unreported.
var errTranscriptUntrusted = errors.New("transcript usage: transcript cannot be trusted")

// transcriptUsageSum accumulates one session's token totals across every
// transcript file it reads, with one dedupe set so a message credited in one
// file is never credited again in another.
type transcriptUsageSum struct {
	total agent.CostData
	// seen tracks credited message identities so a repeated content block
	// counts once. The key is the message id when the line carries one;
	// id-less lines fall back to their content hash (see usageLineKey).
	seen map[string]struct{}
	// since is when the session began; a line stamped earlier is history
	// this session did not spend. Zero disables the filter.
	since time.Time
	// untrusted is set once a line carried an impossible count.
	untrusted bool
}

func newTranscriptUsageSum(since time.Time) *transcriptUsageSum {
	return &transcriptUsageSum{seen: make(map[string]struct{}), since: since}
}

// sumTranscriptUsage reads the transcript at path and returns the session's
// token totals: every assistant message.usage summed once per message id.
// A missing file is not an error — a session whose transcript never
// materialized reports no usage, not a failure — and neither is a torn
// trailing line, which the next read picks up whole. Malformed lines are
// skipped: one corrupt record must not zero the session's whole accounting.
func sumTranscriptUsage(path string) (agent.CostData, error) {
	sum := newTranscriptUsageSum(time.Time{})
	if err := sum.addFile(path); err != nil {
		return agent.CostData{}, err
	}
	return sum.result()
}

// result returns the accumulated totals, or errTranscriptUntrusted when a
// line carried an impossible count.
func (s *transcriptUsageSum) result() (agent.CostData, error) {
	if s.untrusted {
		return agent.CostData{}, errTranscriptUntrusted
	}
	return s.total, nil
}

// addTranscript credits one session transcript and the subagent transcripts
// written beside it.
func (s *transcriptUsageSum) addTranscript(path string) error {
	if err := s.addFile(path); err != nil {
		return err
	}
	subagents, err := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	if err != nil {
		return fmt.Errorf("transcript usage: list subagent transcripts: %w", err)
	}
	for _, sub := range subagents {
		if err := s.addFile(sub); err != nil {
			return err
		}
	}
	return nil
}

// addFile credits every complete line of one transcript file.
func (s *transcriptUsageSum) addFile(path string) error {
	f, err := openTranscript(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return fmt.Errorf("transcript usage: read transcript: %w", err)
			}
			continue
		}
		// A line without its terminator is still being written: stop here
		// so the next read sees it whole instead of crediting a fragment.
		if len(line) > 0 && line[len(line)-1] != '\n' {
			return nil
		}
		s.addLine(line)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("transcript usage: read transcript: %w", err)
		}
	}
}

// openTranscript opens a transcript for reading, refusing anything that is
// not a regular file: the path comes from a hook payload, and a FIFO or a
// device named there would block the read or never end it. The open itself is
// non-blocking (opening a FIFO for reading otherwise waits for a writer); the
// flag does not change how a regular file reads.
func openTranscript(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // path is the harness's own transcript, resolved from its hook payload; non-regular files are refused below
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("transcript usage: open transcript: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", errTranscriptUntrusted, path)
	}
	return f, nil
}

// usageLineKey identifies one transcript line for dedupe: the message id
// when present, else the raw content bytes so identical id-less repeats
// still collapse while distinct lines keep their own credit.
func usageLineKey(line transcriptUsageLine) (string, bool) {
	if line.Message != nil && line.Message.ID != "" {
		return "id:" + line.Message.ID, true
	}
	if line.Message != nil && len(line.Message.Content) > 0 {
		return "content:" + string(line.Message.Content), true
	}
	return "", false
}

// transcriptUsageVerdict classifies one parsed line for crediting.
type transcriptUsageVerdict int

const (
	// usageSkip carries no spend for this session: not an assistant line,
	// all-zero usage, no identity, or stamped before the session began.
	usageSkip transcriptUsageVerdict = iota
	// usageCredit carries this session's spend.
	usageCredit
	// usageImpossible carries a count no model call produces.
	usageImpossible
)

// classifyUsageLine decides whether a parsed line is this session's spend.
// Both the exit sum and the live tail use it, so the two never disagree on
// which lines count.
func classifyUsageLine(line transcriptUsageLine, since time.Time) transcriptUsageVerdict {
	if line.Type != "assistant" || line.Message == nil || line.Message.Usage == nil {
		return usageSkip
	}
	u := line.Message.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 {
		return usageImpossible
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return usageSkip
	}
	if !since.IsZero() && line.Timestamp != "" {
		if at, err := time.Parse(time.RFC3339Nano, line.Timestamp); err == nil && at.Before(since) {
			return usageSkip
		}
	}
	return usageCredit
}

// addLine credits one transcript line toward the total unless it was already
// credited. Only this session's assistant lines with nonzero usage move the
// total; everything else is a no-op so the caller stays a plain loop.
func (s *transcriptUsageSum) addLine(rawLine []byte) {
	var line transcriptUsageLine
	if err := json.Unmarshal(rawLine, &line); err != nil {
		return
	}
	switch classifyUsageLine(line, s.since) {
	case usageImpossible:
		s.untrusted = true
		return
	case usageSkip:
		return
	}
	key, ok := usageLineKey(line)
	if !ok {
		return
	}
	if _, dup := s.seen[key]; dup {
		return
	}
	s.seen[key] = struct{}{}
	u := line.Message.Usage
	next := s.total
	if !addTranscriptTokens(&next.InputTokens, u.InputTokens) ||
		!addTranscriptTokens(&next.OutputTokens, u.OutputTokens) ||
		!addTranscriptTokens(&next.CachedInputTokens, u.CacheReadInputTokens) ||
		!addTranscriptTokens(&next.CacheWriteTokens, u.CacheCreationInputTokens) {
		s.untrusted = true
		return
	}
	next.NumTurns++
	s.total = next
}

// addTranscriptTokens adds a count into a running total, refusing a negative
// count and a sum that would overflow.
func addTranscriptTokens(total *int64, n int64) bool {
	if n < 0 || *total > math.MaxInt64-n {
		return false
	}
	*total += n
	return true
}
