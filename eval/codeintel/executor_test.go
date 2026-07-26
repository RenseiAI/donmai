package codeintel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/eval/experiment"
)

func TestQueryFromCase(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{"Where is the function newAgentRunCmd defined? Give the file path and line number.", "newAgentRunCmd"},
		{"Where is the class StructuralZodGrader defined? Give the file path and line number.", "StructuralZodGrader"},
		{"List every non-test Go file that references the identifier codeIntelMCPEntry.", "codeIntelMCPEntry"},
		{"Where is the interface EvalDatasetCase defined?", "EvalDatasetCase"},
	}
	for _, tt := range tests {
		c := Case{Input: CaseInput{TaskType: TaskFindSymbol, Prompt: tt.prompt}}
		if got := queryFromCase(c); got != tt.want {
			t.Errorf("queryFromCase(%q) = %q, want %q", tt.prompt, got, tt.want)
		}
	}
}

func fsCaseFor(symbol string) Case {
	return Case{
		ID:             "fs",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "r", Ref: "s", Prompt: "Where is the function " + symbol + " defined?"},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}
}

// TestExecuteWithout_ControlCleanGuard_And_Grep proves: (a) the WITHOUT arm
// FAILS LOUD when donmai is still reachable (contamination), and (b) with a
// scrubbed env it runs baseline grep and captures a real transcript.
func TestExecuteWithout_ControlCleanGuard_And_Grep(t *testing.T) {
	// A workarea with the target symbol.
	wa := t.TempDir()
	if err := os.WriteFile(filepath.Join(wa, "foo.go"), []byte("package x\n\nfunc newAgentRunCmd() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dir holding a fake donmai (the "baked-in" binary).
	donmaiDir := writeFakeBinary(t, "donmai")

	exec := NewPlumbingExecutor()
	spec := ArmSpec{Arm: ArmWithout, Case: fsCaseFor("newAgentRunCmd"), Workarea: wa, SnapshotID: "wa"}

	// (a) Contaminated env → executeWithout must error.
	spec.Env = []string{"PATH=" + donmaiDir}
	if _, err := exec.Execute(context.Background(), spec); err == nil {
		t.Fatal("WITHOUT arm must fail when donmai is reachable on PATH")
	} else if !strings.Contains(err.Error(), "contamination") {
		t.Errorf("expected contamination error, got %v", err)
	}

	// (b) Scrubbed env → runs grep, captures a transcript naming the file.
	scrubbed, _ := ScrubBinaryFromEnv(spec.Env, "donmai")
	spec.Env = scrubbed
	tr, err := exec.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("scrubbed WITHOUT arm: %v", err)
	}
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "Bash" {
		t.Errorf("expected one Bash grep tool call, got %+v", tr.ToolCalls)
	}
	if !strings.Contains(tr.ToolCalls[0].ResultText, "foo.go") {
		t.Errorf("grep result should mention foo.go; got %q", tr.ToolCalls[0].ResultText)
	}
	if !strings.Contains(tr.FinalAnswer, "foo.go") {
		t.Errorf("WITHOUT answer should name foo.go; got %q", tr.FinalAnswer)
	}
	if tr.SnapshotRef == nil || tr.SnapshotRef.Retain != RetainEvalPermanent {
		t.Errorf("transcript must carry an eval-permanent snapshotRef; got %+v", tr.SnapshotRef)
	}
	if len(tr.AdvertisedTools) != 0 {
		t.Errorf("control arm must advertise no tools; got %v", tr.AdvertisedTools)
	}
	if tr.TokenCounts.Total() == 0 {
		t.Error("transcript must carry nonzero token counts")
	}
}

// TestBuildClaudeInvocation_WiresArms proves the live-executor invocation seam:
// the WITH arm gets --mcp-config/--strict-mcp-config + budget + allowedTools;
// the WITHOUT arm gets strict zero-MCP isolation; both carry the arm env verbatim.
func TestBuildClaudeInvocation_WiresArms(t *testing.T) {
	withSpec := ArmSpec{
		Arm:             ArmWith,
		Case:            fsCaseFor("X"),
		Env:             []string{"PATH=/with/bin"},
		Budget:          Budget{MaxTurns: 5, MaxTokens: 10000},
		AdvertiseMode:   AdvertiseMCP,
		UseCodeIntel:    true,
		MCPConfigPath:   "/tmp/mcp.json",
		AdvertisedTools: []string{"mcp__af-code-intelligence__af_code_search_symbols"},
	}
	inv := BuildClaudeInvocation(withSpec)
	joined := strings.Join(inv.Argv, " ")
	for _, want := range []string{"--mcp-config /tmp/mcp.json", "--strict-mcp-config", "--max-turns 5", "--allowedTools mcp__af-code-intelligence__af_code_search_symbols"} {
		if !strings.Contains(joined, want) {
			t.Errorf("WITH invocation missing %q; got %s", want, joined)
		}
	}
	if strings.Join(inv.Env, " ") != "PATH=/with/bin" {
		t.Errorf("WITH env not carried through: %v", inv.Env)
	}

	withoutSpec := ArmSpec{Arm: ArmWithout, Case: fsCaseFor("X"), Env: []string{"PATH=/clean/bin"}, Budget: Budget{MaxTurns: 5}}
	winv := BuildClaudeInvocation(withoutSpec)
	wjoined := strings.Join(winv.Argv, " ")
	if strings.Contains(wjoined, "--mcp-config") {
		t.Errorf("WITHOUT invocation must NOT wire an MCP config; got %v", winv.Argv)
	}
	// Symmetric MCP isolation: the control MUST also pass --strict-mcp-config so
	// the agent loads ZERO MCP servers, never the operator's ambient
	// ~/.claude.json or a target repo's committed .mcp.json. Without it the
	// control silently gains any dogfooded af-code-intelligence server, collapsing
	// the measured delta.
	if !strings.Contains(wjoined, "--strict-mcp-config") {
		t.Errorf("WITHOUT invocation must pass --strict-mcp-config (zero MCP servers); got %v", winv.Argv)
	}
	if !strings.Contains(wjoined, "--max-turns 5") {
		t.Error("both arms must carry the equal budget")
	}
}

func TestBuildClaudeInvocation_InjectsVariantWithoutChangingTask(t *testing.T) {
	spec := ArmSpec{
		Arm: "candidate", Case: fsCaseFor("legacy prompt"), Prompt: "shared benign task",
		UseCodeIntel: true, MCPConfigPath: "/tmp/prompt-experiment-mcp.json",
		PromptSuffix: "shared tool guidance", VariantSystemPrompt: "candidate clause",
		Env: []string{"PATH=/bin"},
	}
	inv := BuildClaudeInvocation(spec)
	joined := strings.Join(inv.Argv, " ")
	if !strings.Contains(joined, "-p shared benign task") {
		t.Fatalf("invocation did not use planned shared task: %v", inv.Argv)
	}
	if strings.Contains(joined, "legacy prompt") {
		t.Fatalf("invocation leaked unplanned case prompt: %v", inv.Argv)
	}
	if !strings.Contains(joined, "--append-system-prompt shared tool guidance\n\ncandidate clause") {
		t.Fatalf("invocation did not compose shared guidance + variant: %v", inv.Argv)
	}
	if !strings.Contains(joined, "--mcp-config /tmp/prompt-experiment-mcp.json") {
		t.Fatalf("opaque treatment arm did not receive authored MCP config: %v", inv.Argv)
	}
}

func TestPlumbingExecutor_ContextResetFailsClosed(t *testing.T) {
	_, err := NewPlumbingExecutor().Execute(context.Background(), ArmSpec{
		Arm:          "candidate",
		ContextReset: &experiment.ContextReset{AfterTurn: 4, ContinuationPrompt: "resume"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not implement context-reset") {
		t.Fatalf("error = %v, want fail-closed unsupported perturbation", err)
	}
}

func TestPlumbingExecutor_PromptVariantFailsClosed(t *testing.T) {
	_, err := NewPlumbingExecutor().Execute(context.Background(), ArmSpec{
		Arm: "candidate", Case: fsCaseFor("Target"), VariantSystemPrompt: "candidate clause",
	})
	if err == nil || !strings.Contains(err.Error(), "does not implement prompt-variant") {
		t.Fatalf("error = %v, want fail-closed unsupported prompt variant", err)
	}
}

func TestDedupSnippetExtraction(t *testing.T) {
	prompt := "Consider the following code snippet:\n\n```go\nfunc LocateRepoRoot(start string) (string, bool) {\n\treturn \"\", false\n}\n```\n\nIs this a duplicate?"
	snip := dedupSnippet(prompt)
	if !strings.Contains(snip, "func LocateRepoRoot") {
		t.Errorf("dedupSnippet did not extract the fenced code; got %q", snip)
	}
	if strings.Contains(snip, "```") || strings.Contains(snip, "Consider") {
		t.Errorf("dedupSnippet leaked fence/prose; got %q", snip)
	}
}

// TestDedupAnswerFromResult_Shapes pins the parser over every check-duplicate
// result shape the WITH arm can see:
//
//   - the v4 symbol-granular shape (filePath + symbolName + line) — the answer
//     must carry the file AND the symbol site so the dedup grader's
//     file-mention check passes with no grep follow-up;
//   - the flat v2 native shape (existingId only) — the file must still be
//     named (previously this shape yielded "duplicate of ." and failed the
//     grader, forcing the both-costs grep);
//   - the legacy TS shape (match.filePath / duplicates[]);
//   - the negative shape.
func TestDedupAnswerFromResult_Shapes(t *testing.T) {
	v4 := `{"isDuplicate":true,"matchType":"exact","existingId":"big.go","hammingDistance":0,"filePath":"big.go","symbolName":"computeRollingChecksum","line":42}`
	got := dedupAnswerFromResult(v4)
	for _, want := range []string{"big.go", "computeRollingChecksum", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("v4 shape answer %q missing %q", got, want)
		}
	}

	v2 := `{"isDuplicate":true,"matchType":"near","existingId":"records.go","hammingDistance":1}`
	if got := dedupAnswerFromResult(v2); !strings.Contains(got, "records.go") {
		t.Errorf("flat v2 shape answer %q does not name records.go", got)
	}

	legacy := `{"isDuplicate":true,"match":{"filePath":"x.ts"}}`
	if got := dedupAnswerFromResult(legacy); !strings.Contains(got, "x.ts") {
		t.Errorf("legacy TS shape answer %q does not name x.ts", got)
	}

	neg := `{"isDuplicate":false,"matchType":"none","existingId":"","hammingDistance":0}`
	if got := dedupAnswerFromResult(neg); !strings.Contains(strings.ToLower(got), "not a duplicate") {
		t.Errorf("negative shape answer %q is not a NOT-a-duplicate verdict", got)
	}
}

// TestResultParsers_TolerateTruncationSentinel pins the F1 wire contract: the
// exact-match short-circuit may APPEND a sentinel element
// {"truncatedExactMatches": n, "hint": …} to a search-symbols result. The
// harness parsers must ignore it — topSymbolHit still reads the first REAL
// hit, and collectFilePaths collects only elements carrying filePath keys.
func TestResultParsers_TolerateTruncationSentinel(t *testing.T) {
	result := `[
	  {"symbol":{"name":"Extract","filePath":"a.go","line":3},"score":15,"matchType":"exact"},
	  {"symbol":{"name":"Extract","filePath":"b.go","line":7},"score":15,"matchType":"exact"},
	  {"truncatedExactMatches":2,"hint":"raise maxResults to see all exact definitions"}
	]`

	f, l, ok := topSymbolHit(result)
	if !ok || f != "a.go" || l != 3 {
		t.Errorf("topSymbolHit = (%q,%d,%v), want (a.go,3,true) — sentinel must not break the top hit", f, l, ok)
	}

	files := collectFilePaths(result)
	if len(files) != 2 || files[0] != "a.go" || files[1] != "b.go" {
		t.Errorf("collectFilePaths = %v, want [a.go b.go] — sentinel must contribute nothing", files)
	}
}
