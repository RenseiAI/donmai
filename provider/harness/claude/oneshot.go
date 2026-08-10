package claude

// oneshot.go — the NON-AGENTIC single-shot completion lane.
//
// The claude CLI has two headless modes and they are not the same thing:
//
//	AGENTIC   `claude -p --output-format stream-json --verbose
//	           --dangerously-skip-permissions --add-dir <cwd>
//	           --permission-mode bypassPermissions --append-system-prompt …`
//	          — the full agent harness. It loads the built-in tool set, spawns
//	          every configured MCP server, discovers CLAUDE.md up from cwd, runs
//	          hooks, and keeps the Claude Code system prompt with the caller's
//	          text APPENDED to it. `--max-turns 1` caps the loop; it does not
//	          avoid paying for it. This is what buildArgs (cli_args.go) builds
//	          and what Spawn drives.
//
//	ONE-SHOT  `claude -p --output-format json --max-turns 1 --strict-mcp-config
//	           --no-session-persistence --system-prompt <text> --tools ""`
//	          — one process, one completion. No tools (so no agent loop is even
//	          reachable), no MCP servers, no session file, and the caller's text
//	          REPLACES the Claude Code system prompt rather than appending to it.
//	          This is what buildOneShotArgs builds and what Complete drives.
//
// The distinction is load-bearing, not cosmetic. kgextract's constrained triple
// extraction ran on the AGENTIC shape and blew a 120s per-observation deadline
// on every observation in the fleet — `succeeded=0 failed=N`, zero graph nodes
// written. The same extraction on the ONE-SHOT shape completes in ~1.3–8s
// (measured against claude CLI 2.1.226).
//
// AUTH: this lane adds no credential requirement. It passes no `--bare` (which
// would force ANTHROPIC_API_KEY / apiKeyHelper and refuse to read OAuth or the
// keychain) and no `--setting-sources` override (which could drop a host's
// apiKeyHelper), so a host authenticated by subscription/login — the whole
// reason KG extraction rides the fleet rather than the platform — keeps working
// exactly as it does for an agent session.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Compile-time assertion: the claude provider satisfies OneShotProvider, so
// agent.Complete resolves a one-shot to the non-agentic lane below instead of
// falling back to agent.SpawnComplete's agent-harness projection.
var _ agent.OneShotProvider = (*Provider)(nil)

// oneShotStdoutLimit bounds how much of the CLI's stdout is read. The
// `--output-format json` envelope is a single document whose `result` field is
// the whole completion; 8 MiB is far past any realistic completion and keeps a
// runaway process from being read into memory unbounded.
const oneShotStdoutLimit = 8 << 20

// oneShotStderrLimit bounds captured stderr, which is only ever used to build
// an error message.
const oneShotStderrLimit = 8 << 10

// oneShotEnvelope is the `--output-format json` document the CLI prints exactly
// once on stdout. Only the fields this lane needs are declared; the CLI emits
// many more (usage, modelUsage, ttft_ms, …) and unknown fields are ignored.
//
// IsError is the ONLY reliable success signal. Measured against CLI 2.1.226: a
// request against a nonexistent model returns exit status 0, `subtype:"success"`,
// AND `is_error:true` with `terminal_reason:"api_error"`, `api_error_status:404`
// and the human-readable failure in `result`. Branching on `subtype` or on the
// process exit code would report that failure as a successful completion whose
// text happens to be an apology — precisely the silent-success shape that hid
// this lane's earlier defects.
type oneShotEnvelope struct {
	Type           string   `json:"type"`
	Subtype        string   `json:"subtype"`
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	TerminalReason string   `json:"terminal_reason"`
	APIErrorStatus *int     `json:"api_error_status"`
	SessionID      string   `json:"session_id"`
	NumTurns       int      `json:"num_turns"`
	TotalCostUSD   *float64 `json:"total_cost_usd"`
	Usage          struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadInputToken int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// Complete runs ONE non-agentic completion through the claude CLI's print mode
// and projects it onto an agent.OneShotResult.
//
// It is the OneShotProvider implementation, so agent.Complete routes here for
// this harness. Callers that want the one-shot strategy resolved for them should
// call agent.Complete, never agent.SpawnComplete directly.
//
// The CLI is invoked with the request's messages on STDIN (keeping observation
// text off the process listing and clear of argv length limits) and its system
// prompt REPLACED by req.System. The child runs in os.TempDir() rather than the
// caller's working directory: with `--tools ""` there is no filesystem access to
// grant, and an arbitrary cwd would drag that directory's CLAUDE.md chain into a
// pure completion.
//
// An error means the completion could not be produced (spawn failure,
// unparseable envelope, `is_error:true`). A successful-but-unparseable-by-the-
// caller completion is NOT an error here — that is the caller's
// validate-repair-drop concern, signalled via OneShotResult.SchemaOK.
func (p *Provider) Complete(ctx context.Context, req agent.OneShotRequest) (agent.OneShotResult, error) {
	argv, stdinPrompt := buildOneShotArgs(req)

	cmd := exec.CommandContext(ctx, p.binary, argv...) //nolint:gosec // p.binary is the resolved claude CLI
	cmd.Dir = os.TempDir()
	cmd.Env = runtimeenv.ComposeChildEnv(os.Environ(), oneShotEnv(req))
	cmd.Stdin = strings.NewReader(stdinPrompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	// Report a cancelled/expired context as such: exec surfaces a kill as a
	// generic "signal: killed", which reads like a crash rather than a deadline.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return agent.OneShotResult{}, fmt.Errorf("provider/claude: one-shot: %w", ctxErr)
	}

	env, err := parseOneShotEnvelope(stdout.Bytes())
	if err != nil {
		if runErr != nil {
			return agent.OneShotResult{}, fmt.Errorf(
				"provider/claude: one-shot: %w: %v (stderr: %s)",
				agent.ErrSpawnFailed, runErr, truncate(stderr.String(), oneShotStderrLimit))
		}
		return agent.OneShotResult{}, fmt.Errorf("provider/claude: one-shot: %w", err)
	}
	if env.IsError {
		return agent.OneShotResult{}, fmt.Errorf(
			"provider/claude: one-shot failed (%s%s): %s",
			envReason(env), envStatus(env), truncate(env.Result, oneShotStderrLimit))
	}

	return agent.ProjectOneShot(req, env.Result, oneShotCost(env), p.Manifest().Caps.Transport), nil
}

// buildOneShotArgs translates a OneShotRequest into the NON-AGENTIC argv and the
// stdin prompt. Every flag is load-bearing; see the file header for the contrast
// with buildArgs' agentic shape.
//
//	-p                        non-interactive: print the result and exit.
//	--output-format json      one JSON envelope on stdout (not the JSONL stream
//	                          the agent lane parses).
//	--max-turns 1             one completion, never a loop.
//	--strict-mcp-config       with NO --mcp-config, this means zero MCP servers.
//	                          Without it the CLI starts every MCP server the host
//	                          has configured before answering — startup cost and
//	                          a hang surface a pure completion has no use for.
//	--no-session-persistence  nothing here is ever resumed; skip the session file.
//	--system-prompt <text>    REPLACES the Claude Code agent system prompt. The
//	                          agentic lane's --append-system-prompt keeps it.
//	--model / --effort        pass-through when the request binds them.
//	--tools ""                disable ALL built-in tools. This is what makes the
//	                          invocation non-agentic rather than merely capped:
//	                          with no tools the model cannot take an agent step.
//
// Deliberately ABSENT: --dangerously-skip-permissions / --permission-mode (with
// no tools there is nothing to permit, and the former is refused outright under
// root), --add-dir (no filesystem access to grant), --verbose, --bare (see the
// AUTH note in the file header).
//
// --tools is placed LAST because the CLI declares it variadic (`--tools
// <tools...>`); keeping it in final position means its empty-string value can
// never swallow a following flag, whatever the CLI's parser does with an empty
// variadic.
func buildOneShotArgs(req agent.OneShotRequest) (argv []string, stdinPrompt string) {
	argv = []string{
		"-p",
		"--output-format", "json",
		"--max-turns", strconv.Itoa(1),
		"--strict-mcp-config",
		"--no-session-persistence",
	}
	if req.System != "" {
		argv = append(argv, "--system-prompt", req.System)
	}
	model := req.Model
	if req.Endpoint != nil && req.Endpoint.Model != "" {
		model = req.Endpoint.Model // a bound endpoint's model wins, mirroring specFromOneShot
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if req.Effort != "" {
		argv = append(argv, "--effort", string(req.Effort))
	}
	argv = append(argv, "--tools", "")

	return argv, agent.OneShotPrompt(req)
}

// oneShotEnv projects a bound endpoint onto the child environment (serving-host
// knobs + binding credentials), reusing the SAME applyEndpoint projection the
// agent lane runs so a bedrock/vertex/direct binding routes identically on both
// lanes. A request with no Endpoint contributes nothing and the child simply
// inherits the parent environment — which is what carries host-session auth.
func oneShotEnv(req agent.OneShotRequest) map[string]string {
	if req.Endpoint == nil {
		return nil
	}
	spec, err := applyEndpoint(agent.Spec{Endpoint: req.Endpoint})
	if err != nil {
		// A mis-bound endpoint is reported by the CLI itself (unknown model /
		// missing credentials) rather than silently routed to the default host;
		// there is no env to add in that case.
		return nil
	}
	return spec.Env
}

// parseOneShotEnvelope decodes the single `--output-format json` document. It
// tolerates leading noise on stdout (a warning line ahead of the JSON) by
// falling back to the outermost brace span, and requires the decoded document to
// actually be a result envelope.
func parseOneShotEnvelope(stdout []byte) (oneShotEnvelope, error) {
	var env oneShotEnvelope
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return env, fmt.Errorf("CLI produced no output")
	}
	if len(trimmed) > oneShotStdoutLimit {
		trimmed = trimmed[:oneShotStdoutLimit]
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		start := bytes.IndexByte(trimmed, '{')
		end := bytes.LastIndexByte(trimmed, '}')
		if start < 0 || end <= start {
			return oneShotEnvelope{}, fmt.Errorf("unparseable CLI output: %w", err)
		}
		if err2 := json.Unmarshal(trimmed[start:end+1], &env); err2 != nil {
			return oneShotEnvelope{}, fmt.Errorf("unparseable CLI output: %w", err2)
		}
	}
	if env.Type != "" && env.Type != "result" {
		return oneShotEnvelope{}, fmt.Errorf("unexpected CLI envelope type %q (want %q)", env.Type, "result")
	}
	return env, nil
}

// oneShotCost projects the envelope's usage onto agent.CostData. Returns nil
// when the CLI reported nothing to attribute (a subscription/login cell).
func oneShotCost(env oneShotEnvelope) *agent.CostData {
	if env.TotalCostUSD == nil && env.Usage.InputTokens == 0 && env.Usage.OutputTokens == 0 {
		return nil
	}
	c := &agent.CostData{
		InputTokens:       env.Usage.InputTokens,
		OutputTokens:      env.Usage.OutputTokens,
		CachedInputTokens: env.Usage.CacheReadInputToken,
		NumTurns:          env.NumTurns,
	}
	if env.TotalCostUSD != nil {
		c.TotalCostUsd = *env.TotalCostUSD
	}
	return c
}

// envReason renders the envelope's terminal reason for an error message.
func envReason(env oneShotEnvelope) string {
	if env.TerminalReason != "" {
		return env.TerminalReason
	}
	return "is_error"
}

// envStatus renders the envelope's API error status, when present.
func envStatus(env oneShotEnvelope) string {
	if env.APIErrorStatus == nil {
		return ""
	}
	return " status=" + strconv.Itoa(*env.APIErrorStatus)
}

// truncate bounds a string used inside an error message.
func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…(truncated)"
}
