package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

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

// transcriptUsageLine is the narrow slice of the transcript schema this
// accounting reads: assistant lines with a message id and its usage. Every
// other record type — user turns, tool results, system summaries, hook
// bookkeeping — carries no model spend and is ignored, as are assistant
// lines whose usage is all zero (a streamed repeat that carried no new
// tokens). Unknown fields stay unread on purpose: the fewer fields depended
// on, the fewer ways an upstream schema change turns into a miscount.
type transcriptUsageLine struct {
	Type    string `json:"type"`
	Message *struct {
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

// sumTranscriptUsage reads the transcript at path and returns the session's
// token totals: every assistant message.usage summed once per message id.
// A missing file is not an error — a session whose transcript never
// materialized reports no usage, not a failure — and neither is a torn
// trailing line, which the next read picks up whole. Malformed lines are
// skipped: one corrupt record must not zero the session's whole accounting.
func sumTranscriptUsage(path string) (agent.CostData, error) {
	var total agent.CostData
	// seen tracks credited message identities so a repeated content block
	// counts once. The key is the message id when the line carries one;
	// id-less lines fall back to their content hash (see usageLineKey).
	seen := make(map[string]struct{})
	f, err := os.Open(path) //nolint:gosec // path is the harness's own transcript, resolved from its hook payload
	if err != nil {
		if os.IsNotExist(err) {
			return total, nil
		}
		return total, fmt.Errorf("transcript usage: open transcript: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 {
			if err != nil {
				if err == io.EOF {
					return total, nil
				}
				return total, fmt.Errorf("transcript usage: read transcript: %w", err)
			}
			continue
		}
		// A line without its terminator is still being written: stop here
		// so the next read sees it whole instead of crediting a fragment.
		if len(line) > 0 && line[len(line)-1] != '\n' {
			return total, nil
		}
		total = addTranscriptUsageLine(total, line, seen)
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, fmt.Errorf("transcript usage: read transcript: %w", err)
		}
	}
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

// addTranscriptUsageLine credits one transcript line toward total unless it
// was already credited. Only assistant-role lines with nonzero usage move
// the total; everything else is a no-op so the caller stays a plain loop.
func addTranscriptUsageLine(total agent.CostData, rawLine []byte, seen map[string]struct{}) agent.CostData {
	var line transcriptUsageLine
	if err := json.Unmarshal(rawLine, &line); err != nil {
		return total
	}
	if line.Type != "assistant" || line.Message == nil || line.Message.Usage == nil {
		return total
	}
	u := line.Message.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return total
	}
	key, ok := usageLineKey(line)
	if !ok {
		return total
	}
	if _, dup := seen[key]; dup {
		return total
	}
	seen[key] = struct{}{}
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.CachedInputTokens += u.CacheReadInputTokens
	total.CacheWriteTokens += u.CacheCreationInputTokens
	total.NumTurns++
	return total
}
