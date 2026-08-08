package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
)

// This file implements agent.NoticeDeliveryHook for the Claude Code harness:
// delivery of a message INTO A LIVE TURN of an already-running interactive
// session, over the CLI's own Stop hook.
//
// # The mechanism, and why it is not a PTY write
//
// When an interactive Claude Code turn ends, the CLI runs its configured `Stop`
// hooks. A hook that prints {"decision":"block","reason":"<text>"} to stdout
// makes the CLI refuse to end the turn and hand <text> to the model as a
// synthetic user message. Nothing is typed at the terminal: the message enters
// through the harness's own application-level door, which is exactly what the
// notice-delivery axis reserves for harnesses that have an agent behind the
// terminal.
//
// # Shape: the runner cannot push, so it offers
//
// The runner cannot make a turn end, so it cannot make delivery happen. It
// writes the rendered decision into a per-session drop directory and waits; the
// harness collects it when it next stops. That is why this implements
// agent.NoticeChannel (pull) rather than agent.InteractiveNotifier (push).
//
// # The hook parses nothing
//
// The hook is a five-line POSIX script that renders no JSON and parses none.
// The runner renders the exact stdout payload in Go, where escaping is correct;
// the hook's own stdin is captured verbatim for the runner to parse in Go, for
// the same reason. A shell that has to escape a message body into JSON, or dig
// a path out of one, is a shell that corrupts a message on the first quote
// character.
//
// # The claim IS the loop guard
//
// The hook claims a notice by renaming pending.json to claimed.json. Exactly
// one fire can win that rename, so the re-fire the CLI performs after a block
// (the one carrying stop_hook_active=true) finds nothing to emit and the forced
// continuation terminates. The guard is structural — it does not depend on the
// hook reading, or the CLI supplying, stop_hook_active.
//
// # Consumption, not placement
//
// Writing the drop proves nothing, and neither does the hook printing it: a
// hook that overruns its timeout has its stdout DISCARDED, silently, with the
// turn ending normally. The only evidence that a message reached the model is
// the CLI's own transcript, whose path every hook invocation is handed. So
// Consumed re-reads that transcript and looks for the record the CLI writes
// when it accepts a block into the conversation. Nothing here acks on a write.

const (
	// stopHookPendingFile holds the rendered {"decision":"block",...} payload
	// awaiting collection. Its presence means "outstanding"; its absence means
	// claimed, retracted, or never offered.
	stopHookPendingFile = "pending.json"

	// stopHookClaimedFile is where the hook moves the pending payload to claim
	// it. The rename is atomic, so the claim is single-winner.
	stopHookClaimedFile = "claimed.json"

	// stopHookStdinFile is the hook's own stdin, captured verbatim. It carries
	// transcript_path, which is the only way to find the evidence Consumed
	// needs without out-of-band state.
	stopHookStdinFile = "claim-stdin.json"

	// stopHookScriptFile is the POSIX script the CLI invokes.
	stopHookScriptFile = "stop-hook.sh"

	// stopHookTimeoutSeconds bounds the hook from the CLI's side. The CLI kills
	// an overrunning hook and DISCARDS its stdout with no visible error, so a
	// generous default is not generosity — it is a longer window in which a
	// turn is stalled by a stuck filesystem. This hook does three syscalls;
	// ten seconds is already enormous for it, and an overrun surfaces as an
	// unconsumed notice rather than as a lost turn.
	stopHookTimeoutSeconds = 10
)

// stopHookChannel is one interactive session's Stop-hook notice channel. It
// owns a private directory containing the hook script and the drop; the
// directory is removed when the session's handle cleans up.
type stopHookChannel struct {
	dir string

	mu sync.Mutex
	// text is the outstanding offer's message body, empty when nothing is
	// outstanding.
	text string
	// deliveryID is the outstanding offer's id, carried for logging only —
	// evidence matching is on the body, because the body is what the CLI
	// records.
	deliveryID string
	// scanned is how many bytes of the transcript have already been examined
	// and credited. It only ever advances, which is what stops a repeated
	// message from acking itself against the record of its predecessor.
	scanned int64
}

// Compile-time assertion: the drop implements the pull seam the runner drives.
var _ agent.NoticeChannel = (*stopHookChannel)(nil)

// newStopHookChannel materializes a private drop directory and the hook script
// inside it.
//
// The script lives in a temp directory rather than in the session's worktree on
// purpose: a hook file inside the tree is a file the agent can read, edit, or
// commit by accident, and a settings FILE there would be worse still. The CLI
// accepts its --settings as a JSON STRING, so nothing about this channel needs
// to exist inside the workspace.
func newStopHookChannel() (*stopHookChannel, error) {
	dir, err := os.MkdirTemp("", "donmai-stophook-")
	if err != nil {
		return nil, fmt.Errorf("stop-hook channel: create drop dir: %w", err)
	}
	c := &stopHookChannel{dir: dir}
	script := stopHookScript(dir)
	if err := os.WriteFile(c.path(stopHookScriptFile), []byte(script), 0o700); err != nil { //nolint:gosec // the hook is executed by the CLI; it is owner-only
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("stop-hook channel: write hook script: %w", err)
	}
	return c, nil
}

func (c *stopHookChannel) path(name string) string { return filepath.Join(c.dir, name) }

// close removes the drop directory. Idempotent; wired into the handle's
// per-session cleanup so a notice still outstanding when the session ends dies
// with the session instead of lingering where a later process could serve it.
func (c *stopHookChannel) close() error {
	if err := os.RemoveAll(c.dir); err != nil {
		return fmt.Errorf("stop-hook channel: remove drop dir: %w", err)
	}
	return nil
}

// settingsJSON is the value for `claude --settings`, which the CLI documents as
// "a settings JSON file OR a JSON string" and applies ADDITIVELY on top of
// whichever base layer --setting-sources selects. Declaring only `hooks` here
// therefore adds this Stop hook without disturbing any other setting.
func (c *stopHookChannel) settingsJSON() (string, error) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/bin/sh " + shellQuote(c.path(stopHookScriptFile)),
							"timeout": stopHookTimeoutSeconds,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("stop-hook channel: marshal settings: %w", err)
	}
	return string(b), nil
}

// Offer writes the rendered block decision into the drop.
//
// The write is atomic (temp file + rename) because the hook may fire at any
// instant: a half-written pending.json is a truncated JSON document on the
// CLI's stdin, which is a corrupted message rather than a missing one.
func (c *stopHookChannel) Offer(deliveryID, text string) error {
	decision, err := json.Marshal(map[string]any{"decision": "block", "reason": text})
	if err != nil {
		return fmt.Errorf("stop-hook channel: marshal decision: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Clear the previous delivery's claim evidence first. Leaving it in place
	// would let Consumed answer this offer with the last one's receipt.
	for _, f := range []string{stopHookClaimedFile, stopHookStdinFile} {
		if err := os.Remove(c.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stop-hook channel: clear %s: %w", f, err)
		}
	}

	tmp := c.path(stopHookPendingFile + ".tmp")
	if err := os.WriteFile(tmp, append(decision, '\n'), 0o600); err != nil {
		return fmt.Errorf("stop-hook channel: stage offer: %w", err)
	}
	if err := os.Rename(tmp, c.path(stopHookPendingFile)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("stop-hook channel: publish offer: %w", err)
	}
	c.text, c.deliveryID = text, deliveryID
	return nil
}

// Retract removes an outstanding offer.
//
// retracted == true means pending.json was still there and is now gone, so the
// hook can never emit it. false means the hook already claimed it — at which
// point the message is out of our hands and only Consumed can say what became
// of it.
func (c *stopHookChannel) Retract() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := os.Remove(c.path(stopHookPendingFile))
	switch {
	case err == nil:
		c.text, c.deliveryID = "", ""
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		c.text, c.deliveryID = "", ""
		return false, nil
	default:
		return false, fmt.Errorf("stop-hook channel: retract offer: %w", err)
	}
}

// Consumed answers from the CLI's OWN transcript, never from the drop.
//
// The chain of evidence is: the hook captured its stdin (so we know which
// transcript this session writes), and the transcript contains the user-role
// message the CLI synthesizes when it accepts a blocked Stop hook's reason into
// the conversation. Only the second fact is consumption. A claim receipt with
// no transcript record is precisely the discarded-output case the CLI reports
// nowhere else, and it must read as NOT delivered.
func (c *stopHookChannel) Consumed() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.text == "" {
		return false, nil
	}

	transcript, ok, err := c.claimedTranscriptPath()
	if err != nil || !ok {
		return false, err
	}

	found, next, err := transcriptShowsConsumed(transcript, c.scanned, c.text)
	if err != nil {
		return false, err
	}
	// Advance the cursor whether or not this poll matched: every line below it
	// has been examined, and crediting a line twice is how an identical
	// message acks itself against its predecessor's record.
	c.scanned = next
	if found {
		c.text, c.deliveryID = "", ""
	}
	return found, nil
}

// claimedTranscriptPath reads transcript_path out of the verbatim hook stdin.
// ok is false while the hook has not fired for this offer yet, which is the
// ordinary waiting state and not an error.
func (c *stopHookChannel) claimedTranscriptPath() (string, bool, error) {
	b, err := os.ReadFile(c.path(stopHookStdinFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("stop-hook channel: read claim receipt: %w", err)
	}
	var payload struct {
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return "", false, fmt.Errorf("stop-hook channel: parse claim receipt: %w", err)
	}
	if strings.TrimSpace(payload.TranscriptPath) == "" {
		return "", false, errors.New("stop-hook channel: claim receipt carried no transcript_path")
	}
	return payload.TranscriptPath, true, nil
}

// transcriptRecord is the narrow slice of the CLI's JSONL schema this channel
// reads. Everything else in a transcript line is ignored on purpose: the fewer
// fields depended on, the fewer ways an upstream schema change turns into a
// message reported as delivered.
type transcriptRecord struct {
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// transcriptShowsConsumed scans the transcript from byte offset `from` for the
// record proving `text` entered the conversation, and returns the offset of the
// end of the last COMPLETE line examined.
//
// Two things it deliberately does not do:
//
//   - It does not require the CLI's "Stop hook feedback:" framing. The body is
//     ours and the framing is theirs; matching on their wording would turn a
//     cosmetic upstream edit into a channel that never acks.
//   - It does not credit a partial trailing line. A transcript is appended to
//     live, so the last line may be half-written; stopping at the last newline
//     means the next poll re-reads it whole.
func transcriptShowsConsumed(path string, from int64, text string) (bool, int64, error) {
	f, err := os.Open(path) //nolint:gosec // the path comes from the harness's own hook payload
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, from, nil
		}
		return false, from, fmt.Errorf("stop-hook channel: open transcript: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return false, from, fmt.Errorf("stop-hook channel: seek transcript: %w", err)
	}

	offset := from
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if !strings.HasSuffix(string(line), "\n") {
			// Incomplete trailing line (or EOF): do not credit it.
			if err != nil && !errors.Is(err, io.EOF) {
				return false, offset, fmt.Errorf("stop-hook channel: read transcript: %w", err)
			}
			return false, offset, nil
		}
		offset += int64(len(line))
		if recordShowsConsumed(line, text) {
			return true, offset, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, offset, nil
			}
			return false, offset, fmt.Errorf("stop-hook channel: read transcript: %w", err)
		}
	}
}

// recordShowsConsumed reports whether one transcript line is the CLI's record
// of `text` having entered the conversation as a message.
//
// The record it looks for is a user-role entry flagged isMeta — the synthetic
// message the CLI builds from an accepted block. The sibling records the same
// transaction writes (a hook_blocking_error attachment, a stop_hook_summary)
// are deliberately NOT accepted: they attest that a hook produced output, which
// is one step short of the message reaching the model.
func recordShowsConsumed(line []byte, text string) bool {
	var rec transcriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return false // transcripts carry record types this struct does not model
	}
	if rec.Type != "user" || !rec.IsMeta || rec.Message.Role != "user" {
		return false
	}
	return strings.Contains(transcriptContentText(rec.Message.Content), text)
}

// transcriptContentText flattens a message content field, which the CLI writes
// either as a bare string or as an array of typed blocks.
func transcriptContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		b.WriteString(blk.Text)
	}
	return b.String()
}

// stopHookScript renders the hook. It takes the drop directory by value and
// bakes the absolute paths in, so the hook needs no environment: hook processes
// inherit whatever the CLI hands them, and a channel that depends on a variable
// surviving that is a channel that fails silently on the day it does not.
func stopHookScript(dir string) string {
	d := shellQuote(dir)
	return `#!/bin/sh
# donmai Stop-hook notice channel. Generated per session; do not edit.
#
# Parses nothing and renders nothing: the runner writes the exact stdout payload
# and reads this hook's stdin in Go. The rename below is BOTH the claim and the
# forced-continuation loop guard — only one fire can win it, so the re-fire that
# follows a block finds nothing to emit.
set -u
d=` + d + `
tmp="$d/claim-stdin.$$.tmp"
cat > "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 0; }
if mv "$d/` + stopHookPendingFile + `" "$d/` + stopHookClaimedFile + `" 2>/dev/null; then
  mv "$tmp" "$d/` + stopHookStdinFile + `" 2>/dev/null
  cat "$d/` + stopHookClaimedFile + `"
else
  rm -f "$tmp"
fi
exit 0
`
}

// shellQuote wraps s in single quotes for /bin/sh, escaping any embedded single
// quote. Temp paths rarely contain one; a channel that assumes they never do is
// a channel that breaks on the machine where TMPDIR does.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
