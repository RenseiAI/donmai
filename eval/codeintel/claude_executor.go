package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
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

// claudeSpawner launches the claude CLI in the working directory dir for the
// fully-assembled argv/env and returns its stdout stream plus a wait func. dir
// MUST be the provisioned workarea: the agent's built-in Read/Grep/Bash tools
// operate on the process cwd. The caller reads stdout to EOF, then calls wait to
// collect the process exit status and a stderr tail for diagnostics. Abstracting
// the spawn keeps the stream parser hermetically testable.
type claudeSpawner func(ctx context.Context, dir string, argv []string, env []string) (stdout io.ReadCloser, wait func() (stderrTail string, err error), spawnErr error)

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

	// The live agent MUST run inside the provisioned workarea (repo@ref): claude's
	// built-in Read/Grep/Bash/Edit tools operate on the process cwd. An empty
	// workarea would silently run the agent in the operator's own checkout —
	// grading against a tree the agent never saw, fabricating the WITH/WITHOUT
	// delta (the WITH arm's MCP server is rooted at the workarea while the control
	// arm's native tools would read the wrong tree), and, with
	// --dangerously-skip-permissions, letting Edit/Write land in the real working
	// tree. Fail loud rather than measure garbage. Mirrors PlumbingExecutor's
	// cmd.Dir = spec.Workarea (executor.go).
	if strings.TrimSpace(spec.Workarea) == "" {
		return Transcript{}, fmt.Errorf("claude %s arm: no workarea provisioned (driver bug)", spec.Arm)
	}

	inv := BuildClaudeInvocation(spec)
	argv := append(append([]string(nil), inv.Argv...), streamFlags...)

	spawn := e.spawn
	if spawn == nil {
		spawn = spawnClaude
	}
	stdout, wait, err := spawn(ctx, spec.Workarea, argv, inv.Env)
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
// HARNESS error, or nil when the run produced a well-formed terminal result.
// Precedence: context cancellation (timeout) → non-zero exit → no terminal
// result at all (truncated/crashed) → a partially-unparseable stream.
//
// An agent-ERROR terminal result (subtype=error_max_turns / error_during_execution,
// is_error=true) is deliberately NOT a harness error and must not be returned
// here. Exhausting the equal per-arm turn budget is a legitimate, gradeable TASK
// failure — and a disproportionately CONTROL-arm one, since the whole hypothesis
// is that code-intel helps the agent finish in fewer turns. parseClaudeStream
// leaves FinalAnswer empty for such a result (it refuses to mask it with a stale
// mid-session line), so the task-success grader scores it pass=false and the
// driver records a normal failed trial. Returning an error instead would make
// driver.runOne → driver.Run abort and DISCARD the entire A/B report on the first
// control-arm timeout, censoring exactly the positive-delta regime the eval
// exists to measure (W6 review blocker). Only genuine harness/infra faults are
// fatal here.
//
// So a well-formed terminal result with a clean exit is a completed run (nil)
// even if the task answer is wrong or empty — the graders, not this function,
// judge task correctness (cf. the frozen clijsonl decoder, which classifies
// Success off is_error/subtype, not the exit code).
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
	toolCalls     []ToolCall
	turnCount     int
	tokens        TokenCounts
	finalAnswer   string
	sawResult     bool
	resultSubtype string // terminal result event's `subtype` (e.g. "success", "error_max_turns")
	resultIsError bool   // terminal result event's `is_error`
	parseErr      error
	// assistantTokens is the running sum of per-turn usage across `assistant`
	// events. The terminal result's own usage is authoritative when present, but
	// some terminal results (notably error_max_turns) omit usage entirely; this
	// sum is the fallback so a failed session's real token cost is not recorded as
	// zero — which, for the control-arm-heavy max-turns case, would drag the
	// WITHOUT median down and inflate the WITH/WITHOUT token-ratio gate.
	assistantTokens TokenCounts
}

// resultErrored reports whether the terminal result event signalled an
// agent-level failure — an error_max_turns / error_during_execution (or any
// non-"success") subtype, or is_error=true. It mirrors the frozen clijsonl
// decoder's Success = (subtype=="success" && !is_error) (clijsonl/jsonl.go),
// which the CLI can emit with a CLEAN process exit — so exit code alone is not a
// safe success signal. False until a terminal result is actually seen, so a
// truncated stream (sawResult=false) is handled by the no-result branch instead.
func (ps parsedStream) resultErrored() bool {
	return ps.sawResult && (ps.resultIsError || ps.resultSubtype != "success")
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
	// assistant text block ONLY for a stream that produced no *successful*
	// terminal result — i.e. a genuinely truncated/crashed run. An agent-error
	// terminal result (error_max_turns / error_during_execution) typically
	// carries an empty result string; masking it with a stale mid-session line
	// would let the task-success grader (which only rejects EMPTY answers) score
	// an unfinished session as a valid answer, so we leave FinalAnswer empty and
	// let runError fail the run instead.
	if ps.finalAnswer == "" && !ps.resultErrored() {
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
			Usage claudeUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errSentinel, false
	}
	// Accumulate this turn's usage as the token fallback for a terminal result
	// that omits its own usage (see parsedStream.assistantTokens). Per-turn usage
	// is incremental, so the running sum reconstructs the cumulative session total
	// the terminal result would otherwise report. cache_creation is folded into
	// Input for the same reason parseResult does (fresh input tokens).
	u := a.Message.Usage
	ps.assistantTokens.Input += u.InputTokens + u.CacheCreationInputTokens
	ps.assistantTokens.Output += u.OutputTokens
	ps.assistantTokens.CacheRead += u.CacheReadInputTokens
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
		Result  string      `json:"result"`
		Subtype string      `json:"subtype"`
		IsError bool        `json:"is_error"`
		Usage   claudeUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false
	}
	ps.tokens = TokenCounts{
		Input:     res.Usage.InputTokens + res.Usage.CacheCreationInputTokens,
		Output:    res.Usage.OutputTokens,
		CacheRead: res.Usage.CacheReadInputTokens,
	}
	// A terminal result may omit its usage object (observed on error_max_turns);
	// fall back to the per-turn assistant usage sum so a failed session's real
	// token cost is not silently recorded as zero. A zeroed WITHOUT-arm cost would
	// drag the control median down and inflate the WITH/WITHOUT ratio the
	// Q1v2 efficiency bar (aggregate <=1.0x, per-family <=1.10x) is measured
	// against.
	if ps.tokens == (TokenCounts{}) {
		ps.tokens = ps.assistantTokens
	}
	// Preserve the agent-error signal: an error_max_turns / error_during_execution
	// terminal result is a valid stream event the CLI may emit with a CLEAN exit,
	// but the session did NOT finish. resultErrored() drives the FinalAnswer
	// fallback below to refuse masking an empty error result with a stale
	// mid-session line, so the grader scores it pass=false (a recorded failed
	// trial, not a harness abort — see runError, W6 review finding).
	ps.resultSubtype = res.Subtype
	ps.resultIsError = res.IsError
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

// spawnClaude launches the claude CLI as a real subprocess in the working
// directory dir (the provisioned workarea — claude's Read/Grep/Bash tools run on
// the process cwd). The binary is resolved against the ARM env PATH (BinaryOnPath)
// so the invocation is driven entirely by the authoritative arm environment;
// cmd.Env is set from that env without merging os.Environ, after removing
// runner-only attach controls, so the WITHOUT arm's donmai-scrubbed PATH is
// honored without exposing host-role credentials. stdin is left unset (/dev/null → instant
// EOF): the prompt is carried in argv by BuildClaudeInvocation. The context
// deadline is enforced by exec.CommandContext, which kills the process when ctx
// fires; cmd.WaitDelay bounds the post-exit I/O wait so a grandchild that
// inherited a pipe (e.g. an MCP stdio server) can never hang the run past its
// deadline.
func spawnClaude(ctx context.Context, dir string, argv []string, env []string) (io.ReadCloser, func() (string, error), error) {
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
	cmd.Env = runtimeenv.FilterRunnerOnly(env)
	cmd.Dir = dir
	// After ctx cancellation SIGKILLs claude, a grandchild that inherited the
	// stdout/stderr pipe (an MCP stdio server, or a process the agent backgrounded)
	// can keep the write end open, so cmd.Wait would block forever waiting for I/O
	// to close. WaitDelay caps that: the runtime force-closes the pipes this long
	// after the process exits/cancels, unblocking the drain goroutine so the eval
	// never stalls past its deadline (the orphaned grandchild is reaped when the
	// harness exits).
	cmd.WaitDelay = 15 * time.Second

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
		// Wait FIRST: cmd.Wait returns on process exit (bounded by WaitDelay if a
		// grandchild kept a pipe open) and closes the stderr pipe, which unblocks
		// the drain goroutine below. The previous order (<-stderrDone before Wait)
		// could block forever on such a grandchild since EOF never arrives. The
		// <-stderrDone barrier after Wait guarantees the goroutine has finished
		// writing `tail` before we read it (no data race).
		werr := cmd.Wait()
		<-stderrDone
		return tail.String(), werr
	}
	return stdout, wait, nil
}
