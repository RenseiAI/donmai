package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ClaudeExecutor is the LIVE-LLM Executor: it spawns the real `claude` CLI in
// headless stream-json mode, drives one arm to completion, and captures the
// EvalTrace-shaped Transcript from the streamed events. It is the production
// counterpart to PlumbingExecutor — the plumbing executor proves the two-arm
// wiring deterministically; this one runs an actual agent so the A/B delta is a
// real GA signal.
//
// Both arms are assembled by BuildClaudeInvocation (the shared invocation
// contract), so the WITH arm sees exactly the af-code-intelligence MCP surface
// (via --mcp-config + --strict-mcp-config) and the WITHOUT arm sees ZERO MCP
// servers (--strict-mcp-config with no config). The control arm additionally
// runs the mandatory contamination guard (VerifyControlClean) BEFORE spawning:
// if donmai is still reachable on the arm PATH the run fails loud rather than
// silently leaking the capability under test into the control group.
type ClaudeExecutor struct {
	// spawn is the exec seam. Production uses spawnClaude (a real subprocess);
	// tests inject a scripted stream so the parser is exercised without a live
	// claude on PATH. A nil spawn falls back to spawnClaude.
	spawn claudeSpawner
}

// claudeSpawner launches the claude CLI for the fully-assembled argv/env and
// returns its stdout stream plus a wait func. The caller reads stdout to EOF,
// then calls wait to collect the process exit status and a stderr tail for
// diagnostics. Abstracting the spawn keeps the stream parser hermetically
// testable.
type claudeSpawner func(ctx context.Context, argv []string, env []string) (stdout io.ReadCloser, wait func() (stderrTail string, err error), spawnErr error)

// NewClaudeExecutor returns the live claude executor.
func NewClaudeExecutor() ClaudeExecutor { return ClaudeExecutor{spawn: spawnClaude} }

// newClaudeExecutorWithSpawner injects a scripted spawner (tests only).
func newClaudeExecutorWithSpawner(s claudeSpawner) ClaudeExecutor { return ClaudeExecutor{spawn: s} }

// Name identifies the executor in logs/reports.
func (ClaudeExecutor) Name() string { return "claude" }

// streamFlags are appended on top of the BuildClaudeInvocation argv when the
// executor actually spawns claude: newline-delimited JSON events on stdout
// (--output-format stream-json --verbose) and non-interactive tool use
// (--dangerously-skip-permissions) so the headless agent never blocks on a
// permission prompt. They live here (not in BuildClaudeInvocation) so the
// invocation contract stays a pure, side-effect-free description while the
// executor owns the streaming/permission concerns of a live run.
var streamFlags = []string{"--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}

// Execute runs one arm via the live claude CLI and returns its Transcript.
//
// Robustness contract: a spawn failure, a non-zero exit, a truncated/garbled
// stream, or a context timeout each yield a well-formed (partial) Transcript
// plus a clear wrapped error — never a panic. Whatever tool calls / answer were
// parsed before the fault are preserved on the returned Transcript.
func (e ClaudeExecutor) Execute(ctx context.Context, spec ArmSpec) (Transcript, error) {
	// Control arm: mandatory contamination guard FIRST, before any spawn. The
	// arm env is authoritative here (PATH scrubbed of donmai); a dirty control
	// must fail the run, mirroring PlumbingExecutor.executeWithout.
	if spec.Arm == ArmWithout {
		if err := VerifyControlClean(spec.Env, "donmai"); err != nil {
			return Transcript{}, err
		}
	}

	inv := BuildClaudeInvocation(spec)
	argv := append(append([]string(nil), inv.Argv...), streamFlags...)

	spawn := e.spawn
	if spawn == nil {
		spawn = spawnClaude
	}
	stdout, wait, err := spawn(ctx, argv, inv.Env)
	if err != nil {
		return Transcript{}, fmt.Errorf("claude %s arm: spawn: %w", spec.Arm, err)
	}

	ps := parseClaudeStream(stdout)
	_ = stdout.Close()
	stderrTail, waitErr := wait()

	snap := &SnapshotRef{Provider: "local", SnapshotID: spec.SnapshotID, Retain: RetainEvalPermanent, CapturedAt: nowISO()}
	tr := Transcript{
		Arm:         spec.Arm,
		FinalAnswer: ps.finalAnswer,
		ToolCalls:   ps.toolCalls,
		TurnCount:   ps.turnCount,
		TokenCounts: ps.tokens,
		SnapshotRef: snap,
	}
	if spec.Arm == ArmWith {
		// Only the treatment arm was told about the code-intel tools; the grader
		// needs the advertised set to score adoption.
		tr.AdvertisedTools = spec.AdvertisedTools
	}

	if rerr := runError(ctx, ps, waitErr, stderrTail); rerr != nil {
		return tr, fmt.Errorf("claude %s arm: %w", spec.Arm, rerr)
	}
	return tr, nil
}

// runError classifies the outcome of a completed (or aborted) run into a single
// harness error, or nil on a clean success. Precedence: context cancellation
// (timeout) → non-zero exit → no terminal result (truncated/crashed) → a
// partially-unparseable stream. A well-formed terminal result with a clean exit
// is a success even if the task itself failed — the graders judge task success,
// not this function.
func runError(ctx context.Context, ps parsedStream, waitErr error, stderrTail string) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("run aborted: %w%s", cerr, stderrSuffix(stderrTail))
	}
	if waitErr != nil {
		return fmt.Errorf("claude exited non-zero: %w%s", waitErr, stderrSuffix(stderrTail))
	}
	if !ps.sawResult {
		if ps.parseErr != nil {
			return fmt.Errorf("claude stream ended before a terminal result event: %w%s", ps.parseErr, stderrSuffix(stderrTail))
		}
		return fmt.Errorf("claude stream ended before a terminal result event%s", stderrSuffix(stderrTail))
	}
	if ps.parseErr != nil {
		return fmt.Errorf("claude stream partially unparseable: %w", ps.parseErr)
	}
	return nil
}

func stderrSuffix(tail string) string {
	if strings.TrimSpace(tail) == "" {
		return ""
	}
	return " (stderr: " + truncate(strings.TrimSpace(tail), 500) + ")"
}

// ── stream parser ────────────────────────────────────────────────────────────

// parsedStream is the accumulated result of decoding a claude stream-json run.
type parsedStream struct {
	toolCalls   []ToolCall
	turnCount   int
	tokens      TokenCounts
	finalAnswer string
	sawResult   bool
	parseErr    error
}

// claudeUsage mirrors the terminal result event's `usage` object. Only the
// token fields the efficiency metric needs are decoded.
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// parseClaudeStream reads newline-delimited claude stream-json from r and folds
// it into a parsedStream. It is defensive by construction: an unparseable line
// is counted (parseErr) but never aborts the scan, so a partial/garbled stream
// still yields whatever tool calls and answer were recovered. The shapes are
// the ones emitted by claude 2.1.201 (observed) and the frozen clijsonl decoder:
//
//   - "assistant" → message.content[] of {text} and {tool_use: id,name,input};
//     each assistant event is one agent turn.
//   - "user"      → message.content[] of {tool_result: tool_use_id,content,
//     is_error}, paired back to its tool_use by id.
//   - "result"    → terminal {result, usage:{input_tokens, output_tokens,
//     cache_read_input_tokens, cache_creation_input_tokens}}.
//   - everything else (system/init, rate_limit_event, tool_progress,
//     stream_event, hook_*) is orientation noise and ignored.
func parseClaudeStream(r io.Reader) parsedStream {
	sc := bufio.NewScanner(r)
	// Assistant/result lines can be large (content arrays + full usage); raise
	// the line cap well above the 64KiB default.
	sc.Buffer(make([]byte, 0, 1<<16), 8<<20)

	var ps parsedStream
	idIndex := map[string]int{} // tool_use id → index into ps.toolCalls
	var lastAssistantText string
	var badLines int

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		raw := []byte(line)
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			badLines++
			continue
		}
		switch head.Type {
		case "assistant":
			ps.turnCount++
			if txt, ok := parseAssistant(raw, &ps, idIndex); ok {
				lastAssistantText = txt
			} else if txt == errSentinel {
				badLines++
			}
		case "user":
			if !parseUser(raw, &ps, idIndex) {
				badLines++
			}
		case "result":
			if !parseResult(raw, &ps) {
				badLines++
			}
		default:
			// Orientation noise — deliberately ignored.
		}
	}
	if err := sc.Err(); err != nil {
		ps.parseErr = fmt.Errorf("scan claude stream: %w", err)
	}
	// FinalAnswer prefers the terminal result text; fall back to the last
	// assistant text block for a truncated stream with no result.
	if ps.finalAnswer == "" {
		ps.finalAnswer = lastAssistantText
	}
	if badLines > 0 && ps.parseErr == nil {
		ps.parseErr = fmt.Errorf("%d unparseable line(s) in claude stream", badLines)
	}
	return ps
}

// errSentinel flags a decode failure from parseAssistant (distinct from an
// assistant message that simply carried no text).
const errSentinel = "\x00parse-error\x00"

// parseAssistant folds one assistant event's content blocks into ps: text
// blocks are joined into the returned answer candidate; tool_use blocks become
// ToolCalls indexed by id for later result pairing. ok is false when the
// message carried no text; the returned string is errSentinel on a decode
// failure.
func parseAssistant(raw []byte, ps *parsedStream, idIndex map[string]int) (string, bool) {
	var a struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errSentinel, false
	}
	var textParts []string
	for _, b := range a.Message.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			idIndex[b.ID] = len(ps.toolCalls)
			ps.toolCalls = append(ps.toolCalls, ToolCall{Name: b.Name, Arguments: cloneRawArgs(b.Input)})
		}
	}
	if len(textParts) == 0 {
		return "", false
	}
	return strings.Join(textParts, ""), true
}

// parseUser pairs tool_result blocks back onto their originating tool_use call.
func parseUser(raw []byte, ps *parsedStream, idIndex map[string]int) bool {
	var u struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return false
	}
	for _, b := range u.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		idx, ok := idIndex[b.ToolUseID]
		if !ok {
			continue // orphan result (no matching tool_use in this stream)
		}
		ps.toolCalls[idx].ResultText = truncate(toolResultText(b.Content), 4000)
		ps.toolCalls[idx].IsError = b.IsError
	}
	return true
}

// parseResult reads the terminal result event: the final answer text and the
// token usage. cache_creation_input_tokens are folded into Input (they ARE
// fresh input tokens — the one-time cost of loading e.g. the WITH-arm MCP tool
// schemas); cache_read_input_tokens land in CacheRead. Counting BOTH keeps
// TokenCounts.Total() honest: the WITH arm's context overhead — the very cost
// it is most likely to grow — is never reported as free (W5 review finding).
func parseResult(raw []byte, ps *parsedStream) bool {
	var res struct {
		Result string      `json:"result"`
		Usage  claudeUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false
	}
	ps.tokens = TokenCounts{
		Input:     res.Usage.InputTokens + res.Usage.CacheCreationInputTokens,
		Output:    res.Usage.OutputTokens,
		CacheRead: res.Usage.CacheReadInputTokens,
	}
	if res.Result != "" {
		ps.finalAnswer = res.Result
	}
	ps.sawResult = true
	return true
}

// cloneRawArgs returns an independent copy of a tool_use input payload (or nil
// when absent) so the ToolCall.Arguments never aliases the scanner's buffer.
func cloneRawArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// toolResultText normalizes a tool_result `content` payload (a bare string, an
// array of {type:"text",text} content blocks, or an arbitrary object) into a
// single string for the transcript.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			b.WriteString(bl.Text)
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return string(raw)
}

// ── real subprocess spawn ────────────────────────────────────────────────────

// spawnClaude launches the claude CLI as a real subprocess. The binary is
// resolved against the ARM env PATH (BinaryOnPath) so the invocation is driven
// entirely by the authoritative arm environment; cmd.Env is set to that env
// verbatim (NOT merged with os.Environ) so the WITHOUT arm's donmai-scrubbed
// PATH is honored. stdin is left unset (/dev/null → instant EOF): the prompt is
// carried in argv by BuildClaudeInvocation. The context deadline is enforced by
// exec.CommandContext, which kills the process when ctx fires.
func spawnClaude(ctx context.Context, argv []string, env []string) (io.ReadCloser, func() (string, error), error) {
	if len(argv) == 0 {
		return nil, nil, fmt.Errorf("empty argv")
	}
	name := argv[0]
	var bin string
	if p, ok := BinaryOnPath(name, envPath(env)); ok {
		bin = p
	} else if lp, lerr := exec.LookPath(name); lerr == nil {
		bin = lp
	} else {
		return nil, nil, fmt.Errorf("%q not found on the arm PATH", name)
	}

	cmd := exec.CommandContext(ctx, bin, argv[1:]...) // nolint:gosec // bin is the fixed claude CLI; argv = BuildClaudeInvocation + fixed stream flags.
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start: %w", err)
	}

	// Drain stderr into a bounded tail so warm-up logging never blocks the pipe
	// and a non-zero exit can surface a diagnostic.
	var tail strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(&tail, io.LimitReader(stderr, 16*1024))
		_, _ = io.Copy(io.Discard, stderr)
	}()

	wait := func() (string, error) {
		<-stderrDone
		werr := cmd.Wait()
		return tail.String(), werr
	}
	return stdout, wait, nil
}
