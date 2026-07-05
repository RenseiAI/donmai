package codeintel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := os.WriteFile(filepath.Join(wa, "foo.go"), []byte("package x\n\nfunc newAgentRunCmd() {}\n"), 0o644); err != nil {
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
// the WITH arm gets --mcp-config/--strict-mcp-config + budget + allowedTools; the
// WITHOUT arm gets neither; both carry the arm env verbatim (PATH strip intact).
func TestBuildClaudeInvocation_WiresArms(t *testing.T) {
	withSpec := ArmSpec{
		Arm:             ArmWith,
		Case:            fsCaseFor("X"),
		Env:             []string{"PATH=/with/bin"},
		Budget:          Budget{MaxTurns: 5, MaxTokens: 10000},
		AdvertiseMode:   AdvertiseMCP,
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
	if strings.Contains(strings.Join(winv.Argv, " "), "--mcp-config") {
		t.Errorf("WITHOUT invocation must NOT wire an MCP config; got %v", winv.Argv)
	}
	if !strings.Contains(strings.Join(winv.Argv, " "), "--max-turns 5") {
		t.Error("both arms must carry the equal budget")
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
