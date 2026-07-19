package codeintel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Budget is the equal per-arm turn/token cap (brief 06 §4.3.4). Both arms MUST
// carry the same budget so an unbounded agent can't brute-force the answer and
// flatten the delta.
type Budget struct {
	MaxTurns  int
	MaxTokens int64
}

// ArmSpec is one arm's fully-resolved run request. The driver assembles it
// (provisioned workarea, arm env with/without donmai on PATH, advertisement
// output) and hands it to an Executor.
type ArmSpec struct {
	Arm           Arm
	Case          Case
	Workarea      string   // provisioned repo@ref path
	DonmaiBin     string   // absolute donmai binary path (WITH arm)
	Env           []string // arm env: WITHOUT has donmai scrubbed from PATH
	Budget        Budget
	AdvertiseMode AdvertiseMode
	// MCPServers is the authored af-code-intelligence entry set (WITH + mcp mode).
	MCPServers []agent.MCPServerConfig
	// MCPConfigPath is the written --mcp-config file a real agent harness would
	// consume (WITH + mcp mode). Empty in plumbing/prompt-help.
	MCPConfigPath string
	// PromptSuffix is the advertisement text appended to the agent system prompt
	// (WITH arm). Empty for the control arm.
	PromptSuffix string
	// AdvertisedTools is the tool-name set the arm was told about (for the
	// tool-use grader).
	AdvertisedTools []string
	// SnapshotID labels the workarea snapshot (SnapshotRef.snapshotId).
	SnapshotID string
}

// Executor runs one arm and returns a captured Transcript.
type Executor interface {
	Name() string
	Execute(ctx context.Context, spec ArmSpec) (Transcript, error)
}

// PlumbingExecutor is a deterministic, no-LLM stand-in agent used by the
// --dry/plumbing path and by tests. It PROVES the harness end-to-end — two-arm
// provisioning, the PATH-strip contamination guard, a REAL MCP round-trip on the
// WITH arm, and transcript capture in the EvalTrace shape — WITHOUT the
// cost/nondeterminism of a live LLM. It is a scripted agent: it derives a query
// from the prompt, and
//
//   - WITHOUT arm: asserts donmai is unreachable, then runs a real `grep` on the
//     workarea (baseline tools only) and reports what grep found.
//   - WITH arm (mcp): drives the real af-code-intelligence MCP server for the
//     family's tool and reports the tool's result.
//   - WITH arm (prompt-help): execs the equivalent `donmai code` subcommand via
//     the arm env (donmai on PATH) and reports its result.
//
// The real-LLM executor is a separate seam (see AgentInvocation): the driver
// assembles argv/env/mcp-config for a live harness; binding and stream-parsing
// that harness is the needs-live-env step. PlumbingExecutor is what runs here.
type PlumbingExecutor struct{}

// NewPlumbingExecutor returns the deterministic plumbing executor.
func NewPlumbingExecutor() PlumbingExecutor { return PlumbingExecutor{} }

// Name identifies the executor in logs/reports.
func (PlumbingExecutor) Name() string { return "plumbing" }

// Execute runs one arm of the plumbing agent and returns its transcript.
func (e PlumbingExecutor) Execute(ctx context.Context, spec ArmSpec) (Transcript, error) {
	snap := &SnapshotRef{Provider: "local", SnapshotID: spec.SnapshotID, Retain: RetainEvalPermanent, CapturedAt: nowISO()}
	if spec.Arm == ArmWithout {
		return e.executeWithout(ctx, spec, snap)
	}
	return e.executeWith(ctx, spec, snap)
}

// executeWithout is the control arm: it MUST prove donmai is unreachable, then
// fall back to baseline grep.
func (e PlumbingExecutor) executeWithout(ctx context.Context, spec ArmSpec, snap *SnapshotRef) (Transcript, error) {
	// Mandatory contamination guard — fail the run if the control can still reach donmai.
	if err := VerifyControlClean(spec.Env, "donmai"); err != nil {
		return Transcript{}, err
	}
	query := queryFromCase(spec.Case)
	tc, resultText := e.grep(ctx, spec, query)
	answer := deriveAnswerFromGrep(spec.Case, resultText)
	return Transcript{
		Arm:         ArmWithout,
		FinalAnswer: answer,
		ToolCalls:   []ToolCall{tc},
		TurnCount:   1,
		TokenCounts: synthTokens(len(spec.Case.Input.Prompt), len(resultText)),
		SnapshotRef: snap,
	}, nil
}

// executeWith is the treatment arm: it drives the real code-intel surface.
func (e PlumbingExecutor) executeWith(ctx context.Context, spec ArmSpec, snap *SnapshotRef) (Transcript, error) {
	if spec.AdvertiseMode == AdvertisePromptHelp {
		return e.executeWithCLI(ctx, spec, snap)
	}
	return e.executeWithMCP(ctx, spec, snap)
}

// executeWithMCP drives the authored af-code-intelligence MCP server.
func (e PlumbingExecutor) executeWithMCP(ctx context.Context, spec ArmSpec, snap *SnapshotRef) (Transcript, error) {
	if len(spec.MCPServers) == 0 {
		return Transcript{}, fmt.Errorf("WITH/mcp arm has no authored MCP entry")
	}
	entry := spec.MCPServers[0]
	tool, args := mcpToolAndArgsFor(spec.Case)
	if tool == "" {
		return e.placeholderWith(spec, snap, "refactor tasks require a live agent; plumbing records orientation only")
	}
	res, err := callMCPTool(ctx, entry, spec.Env, tool, args)
	if err != nil {
		return Transcript{}, fmt.Errorf("WITH/mcp %s: %w", tool, err)
	}
	fq := "mcp__" + CodeIntelServerName + "__" + tool
	argsJSON, _ := json.Marshal(args)
	tc := ToolCall{Name: fq, Arguments: argsJSON, ResultText: truncate(res.Text, 4000), IsError: res.IsError}
	answer := deriveAnswerFromTool(spec.Case, res.Text)
	return Transcript{
		Arm:             ArmWith,
		FinalAnswer:     answer,
		ToolCalls:       []ToolCall{tc},
		TurnCount:       1,
		TokenCounts:     synthTokens(len(spec.Case.Input.Prompt), len(res.Text)),
		SnapshotRef:     snap,
		AdvertisedTools: spec.AdvertisedTools,
	}, nil
}

// executeWithCLI drives the equivalent `donmai code` subcommand via the arm env
// (donmai reachable on PATH) — the prompt-help advertisement path.
func (e PlumbingExecutor) executeWithCLI(ctx context.Context, spec ArmSpec, snap *SnapshotRef) (Transcript, error) {
	sub, cliArgs := cliSubcommandFor(spec.Case)
	if sub == "" {
		return e.placeholderWith(spec, snap, "refactor tasks require a live agent; plumbing records orientation only")
	}
	full := append([]string{"code", sub}, cliArgs...)
	cmd := exec.CommandContext(ctx, spec.DonmaiBin, full...) // nolint:gosec // donmai bin + fixed subcommand.
	cmd.Env = runtimeenv.FilterRunnerOnly(spec.Env)
	cmd.Dir = spec.Workarea
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A non-zero exit still yields output we record; the grader judges the answer.
		out = append(out, []byte("\n[exit error: "+err.Error()+"]")...)
	}
	cmdText := "donmai " + strings.Join(full, " ")
	argsJSON, _ := json.Marshal(map[string]string{"command": cmdText})
	tc := ToolCall{Name: "Bash", Arguments: argsJSON, ResultText: truncate(string(out), 4000)}
	answer := deriveAnswerFromTool(spec.Case, string(out))
	return Transcript{
		Arm:             ArmWith,
		FinalAnswer:     answer,
		ToolCalls:       []ToolCall{tc},
		TurnCount:       1,
		TokenCounts:     synthTokens(len(spec.Case.Input.Prompt), len(out)),
		SnapshotRef:     snap,
		AdvertisedTools: spec.AdvertisedTools,
	}, nil
}

// placeholderWith returns a benign WITH transcript for families the plumbing
// agent can't objectively complete (refactor), recording a repo-map orientation
// tool call so the shape is still real.
func (e PlumbingExecutor) placeholderWith(spec ArmSpec, snap *SnapshotRef, note string) (Transcript, error) {
	args, _ := json.Marshal(map[string]any{})
	tc := ToolCall{Name: "mcp__" + CodeIntelServerName + "__af_code_get_repo_map", Arguments: args, ResultText: note}
	return Transcript{
		Arm: ArmWith, FinalAnswer: note, ToolCalls: []ToolCall{tc}, TurnCount: 1,
		TokenCounts: synthTokens(len(spec.Case.Input.Prompt), len(note)), SnapshotRef: snap,
		AdvertisedTools: spec.AdvertisedTools,
	}, nil
}

// grep runs a baseline `grep -rn` for query over the workarea (excluding .git)
// and returns the captured tool call plus the raw output.
func (e PlumbingExecutor) grep(ctx context.Context, spec ArmSpec, query string) (ToolCall, string) {
	if query == "" {
		query = "TODO-no-query"
	}
	args := []string{"-rn", "--exclude-dir=.git", query, "."}
	cmd := exec.CommandContext(ctx, "grep", args...) // nolint:gosec // fixed grep args; query derived from the prompt.
	cmd.Env = runtimeenv.FilterRunnerOnly(spec.Env)
	cmd.Dir = spec.Workarea
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run() // grep exits 1 on no match; that's not a harness error.
	text := truncate(out.String(), 4000)
	cmdText := "grep " + strings.Join(args, " ")
	argsJSON, _ := json.Marshal(map[string]string{"command": cmdText})
	return ToolCall{Name: "Bash", Arguments: argsJSON, ResultText: text}, text
}

// ── Query / answer derivation ────────────────────────────────────────────────

// identifierRe matches identifier-shaped tokens (letters, digits, underscore).
var identifierRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// englishStop are prompt words that are identifier-shaped but not symbol names.
var englishStop = map[string]bool{
	"where": true, "defined": true, "function": true, "give": true, "file": true, "path": true,
	"line": true, "number": true, "relative": true, "repo": true, "root": true, "class": true,
	"interface": true, "type": true, "declaration": true, "export": true, "list": true, "every": true,
	"reference": true, "references": true, "which": true, "files": true, "call": true, "calls": true,
	"consumes": true, "constant": true, "typescript": true, "under": true, "constructor": true,
	"identifier": true, "method": true, "worktree": true, "manager": true, "package": true,
	"exported": true, "shared": true, "writer": true, "config": true, "grader": true, "judge": true,
	"snippet": true, "duplicate": true, "repository": true, "code": true, "that": true, "this": true,
	"give the": true, "paths": true, "referenced": true,
}

// queryFromCase extracts the most distinctive symbol-like token from the case
// prompt, for the WITHOUT-arm grep and the WITH-arm tool query. It prefers
// tokens with an uppercase letter or underscore (identifier-shaped), longest
// first — which cleanly picks e.g. newAgentRunCmd / StructuralZodGrader /
// CodeIntelWork out of an English sentence. Dedup cases (snippet-based) return "".
func queryFromCase(c Case) string {
	if c.Family() == TaskDedup {
		return dedupProbe(c.Input.Prompt)
	}
	toks := identifierRe.FindAllString(c.Input.Prompt, -1)
	var candidates []string
	for _, t := range toks {
		low := strings.ToLower(t)
		if englishStop[low] {
			continue
		}
		hasUpperOrUnderscore := strings.ContainsAny(t, "_") || t != low
		if hasUpperOrUnderscore {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	return candidates[0]
}

// dedupProbe extracts a distinctive line from the fenced snippet in a dedup
// prompt, for a grep probe (the snippet's signature line is a good needle).
func dedupProbe(prompt string) string {
	inFence := false
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "func ") || strings.HasPrefix(t, "export function") {
				// The signature line, minus the name we renamed, is still distinctive.
				return t
			}
		}
	}
	return ""
}

// deriveAnswerFromTool turns a code-intel tool's JSON result into a concise
// final answer string the graders refine over.
func deriveAnswerFromTool(c Case, resultText string) string {
	switch c.Family() {
	case TaskFindSymbol:
		if f, l, ok := topSymbolHit(resultText); ok {
			return fmt.Sprintf("%s:%d", f, l)
		}
		return resultText
	case TaskLocateUsage:
		files := collectFilePaths(resultText)
		if len(files) > 0 {
			return strings.Join(files, ", ")
		}
		return resultText
	case TaskDedup:
		return dedupAnswerFromResult(resultText)
	default:
		return resultText
	}
}

// deriveAnswerFromGrep turns grep -rn output into a concise answer per family.
func deriveAnswerFromGrep(c Case, grepOut string) string {
	lines := nonEmptyLines(grepOut)
	switch c.Family() {
	case TaskFindSymbol:
		// First hit "path:line: …" → "path:line". grep paths are relative to Dir.
		for _, ln := range lines {
			if f, l, ok := parseGrepLoc(ln); ok {
				return fmt.Sprintf("%s:%d", strings.TrimPrefix(f, "./"), l)
			}
		}
		return grepOut
	case TaskLocateUsage:
		files := map[string]bool{}
		var order []string
		for _, ln := range lines {
			if f, _, ok := parseGrepLoc(ln); ok {
				f = strings.TrimPrefix(f, "./")
				if !files[f] {
					files[f] = true
					order = append(order, f)
				}
			}
		}
		return strings.Join(order, ", ")
	case TaskDedup:
		if len(lines) == 0 {
			return "No, this snippet is not a duplicate of existing code (grep found nothing)."
		}
		if f, _, ok := parseGrepLoc(lines[0]); ok {
			return fmt.Sprintf("Yes, this appears to duplicate %s.", strings.TrimPrefix(f, "./"))
		}
		return grepOut
	default:
		return grepOut
	}
}

// dedupAnswerFromResult formats a check-duplicate result into a verdict
// answer. It prefers the v4 symbol-granular shape (filePath + symbolName +
// line — one authoritative answer, no grep follow-up), falls back to the flat
// v2 native shape (existingId), then to the legacy TS shape (match.filePath /
// duplicates[]).
func dedupAnswerFromResult(resultText string) string {
	var r struct {
		IsDuplicate bool   `json:"isDuplicate"`
		FilePath    string `json:"filePath"`
		SymbolName  string `json:"symbolName"`
		Line        int    `json:"line"`
		ExistingID  string `json:"existingId"`
		Match       struct {
			FilePath string `json:"filePath"`
		} `json:"match"`
		Duplicates []struct {
			FilePath string `json:"filePath"`
		} `json:"duplicates"`
	}
	if err := json.Unmarshal([]byte(resultText), &r); err == nil {
		file := r.FilePath
		if file == "" {
			file = r.ExistingID
		}
		if file == "" {
			file = r.Match.FilePath
		}
		if file == "" && len(r.Duplicates) > 0 {
			file = r.Duplicates[0].FilePath
		}
		if r.IsDuplicate || file != "" {
			if r.SymbolName != "" {
				return fmt.Sprintf("Yes, this is a duplicate of %s (symbol %s, line %d).", file, r.SymbolName, r.Line)
			}
			return fmt.Sprintf("Yes, this is a duplicate of %s.", file)
		}
		return "No, this snippet is not a duplicate of existing code."
	}
	// Fall back to the raw text; the grader classifies the verdict from it.
	return resultText
}

// topSymbolHit parses a search-symbols result (an array of {symbol:{filePath,line}})
// and returns the first hit's file+line.
func topSymbolHit(resultText string) (string, int, bool) {
	var hits []struct {
		Symbol struct {
			FilePath string `json:"filePath"`
			Line     int    `json:"line"`
		} `json:"symbol"`
	}
	if err := json.Unmarshal([]byte(resultText), &hits); err != nil || len(hits) == 0 {
		return "", 0, false
	}
	return hits[0].Symbol.FilePath, hits[0].Symbol.Line, true
}

// collectFilePaths pulls distinct "filePath" values out of an arbitrary tool
// result JSON (search-code / find-type-usages), preserving first-seen order.
func collectFilePaths(resultText string) []string {
	var parsed interface{}
	if err := json.Unmarshal([]byte(resultText), &parsed); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var order []string
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, val := range t {
				if k == "filePath" || k == "file" {
					if s, ok := val.(string); ok && s != "" && !seen[s] {
						seen[s] = true
						order = append(order, s)
					}
				}
				walk(val)
			}
		case []interface{}:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(parsed)
	return order
}

// mcpToolAndArgsFor maps a case to the MCP tool + arguments the WITH arm invokes.
func mcpToolAndArgsFor(c Case) (string, map[string]any) {
	switch c.Family() {
	case TaskFindSymbol:
		return "af_code_search_symbols", map[string]any{"query": queryFromCase(c)}
	case TaskLocateUsage:
		return "af_code_search_code", map[string]any{"query": queryFromCase(c)}
	case TaskDedup:
		return "af_code_check_duplicate", map[string]any{"content": dedupSnippet(c.Input.Prompt)}
	default:
		return "", nil // refactor: no single objective tool call.
	}
}

// cliSubcommandFor maps a case to the `donmai code` subcommand + args (prompt-help arm).
func cliSubcommandFor(c Case) (string, []string) {
	switch c.Family() {
	case TaskFindSymbol:
		return "search-symbols", []string{queryFromCase(c)}
	case TaskLocateUsage:
		return "search-code", []string{queryFromCase(c)}
	case TaskDedup:
		return "check-duplicate", []string{"--content", dedupSnippet(c.Input.Prompt)}
	default:
		return "", nil
	}
}

// dedupSnippet extracts the fenced code snippet from a dedup prompt.
func dedupSnippet(prompt string) string {
	inFence := false
	var b strings.Builder
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				break
			}
			inFence = true
			continue
		}
		if inFence {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── small utilities ──────────────────────────────────────────────────────────

var grepLocRe = regexp.MustCompile(`^(.+?):(\d+):`)

func parseGrepLoc(line string) (file string, lineNo int, ok bool) {
	m := grepLocRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	n := 0
	for _, ch := range m[2] {
		n = n*10 + int(ch-'0')
	}
	return m[1], n, true
}

func nonEmptyLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 1<<16), 8<<20)
	for sc.Scan() {
		if t := sc.Text(); strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// synthTokens fabricates plausible, deterministic token counts for the plumbing
// executor (a real executor reads them from the harness billing stream). Derived
// from prompt+result sizes so they are nonzero and monotone, never claimed as
// real model usage.
func synthTokens(promptLen, resultLen int) TokenCounts {
	in := int64(200 + promptLen/4)
	out := int64(40 + resultLen/8)
	return TokenCounts{Input: in, Output: out}
}

// AgentInvocation is the fully-assembled command a LIVE agent harness would run
// for an arm — argv + env + the written --mcp-config path. It is the seam a
// real-LLM executor consumes; the plumbing executor does not need it, but
// exposing (and testing) the assembly proves the WITH/WITHOUT wiring — PATH
// strip, mcp-config, budget flags — independent of any live harness.
type AgentInvocation struct {
	Argv          []string
	Env           []string
	MCPConfigPath string
}

// BuildClaudeInvocation assembles a claude-CLI-style invocation for spec: the
// prompt (with advertisement suffix), the budget flags, and --mcp-config /
// --strict-mcp-config when the WITH arm has an authored MCP entry. It does NOT
// run anything — it is the contract a live executor binds to.
func BuildClaudeInvocation(spec ArmSpec) AgentInvocation {
	argv := []string{"claude", "-p", spec.Case.Input.Prompt}
	if spec.Budget.MaxTurns > 0 {
		argv = append(argv, "--max-turns", fmt.Sprintf("%d", spec.Budget.MaxTurns))
	}
	if spec.PromptSuffix != "" {
		argv = append(argv, "--append-system-prompt", spec.PromptSuffix)
	}
	if spec.Arm == ArmWith && spec.MCPConfigPath != "" {
		argv = append(argv, "--mcp-config", spec.MCPConfigPath, "--strict-mcp-config")
		for _, fq := range spec.AdvertisedTools {
			argv = append(argv, "--allowedTools", fq)
		}
	} else {
		// Every arm WITHOUT an authored MCP config (the control arm, and a WITH
		// prompt-help arm) still gets --strict-mcp-config — with no --mcp-config
		// it yields ZERO MCP servers, so the agent never auto-loads the operator's
		// ambient ~/.claude.json or a target repo's committed .mcp.json. This
		// guarantees symmetric MCP isolation: WITH sees exactly
		// af-code-intelligence, WITHOUT sees exactly nothing. Omitting it would
		// leave the control open to a dogfooded code-intel MCP server, contaminating
		// the very capability under test.
		argv = append(argv, "--strict-mcp-config")
	}
	return AgentInvocation{Argv: argv, Env: spec.Env, MCPConfigPath: spec.MCPConfigPath}
}

// workareaLeaf is a helper to name a snapshot from a workarea path.
func workareaLeaf(path string) string { return filepath.Base(path) }
