package codeintel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func fsCase() Case {
	return Case{
		ID:             "fs",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "r", Ref: "s", Prompt: "where is X?"},
		ExpectedOutput: json.RawMessage(`{"file":"afcli/agent_run.go","lineRange":[70,100]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}
}

func luCase() Case {
	return Case{
		ID:             "lu",
		Input:          CaseInput{TaskType: TaskLocateUsage, Repo: "r", Ref: "s", Prompt: "who uses X?"},
		ExpectedOutput: json.RawMessage(`{"files":["runner/loop.go","runner/runner.go","daemon/daemon.go","afcli/agent_run.go"],"minRecall":0.75}`),
		Tags:           []string{tagSuite, "locate-usage", tagVersion},
	}
}

func dupCase(isDup bool) Case {
	exp := `{"isDuplicate":false}`
	if isDup {
		exp = `{"isDuplicate":true,"file":"afclient/codeintel/gitroot.go"}`
	}
	return Case{
		ID:             "dd",
		Input:          CaseInput{TaskType: TaskDedup, Repo: "r", Ref: "s", Prompt: "is this a dup?"},
		ExpectedOutput: json.RawMessage(exp),
		Tags:           []string{tagSuite, "dedup", tagVersion},
	}
}

func ctx() context.Context { return context.Background() }

// ---- find-symbol ---------------------------------------------------------

func TestFindSymbolGrader(t *testing.T) {
	g := NewFindSymbolGrader()
	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{"correct file+line passes", "The symbol is defined at afcli/agent_run.go:80.", true},
		{"correct file, line in-range prose", "It lives in afcli/agent_run.go around line 80.", true},
		{"EMPTY answer FAILS", "", false},
		{"wrong file FAILS", "It is in runner/loop.go:80.", false},
		{"right file wrong line FAILS", "afcli/agent_run.go:900", false},
		{"only-a-number FAILS", "line 80", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.Grade(ctx(), fsCase(), Transcript{FinalAnswer: tt.answer})
			if got.Pass != tt.want {
				t.Errorf("Pass = %v, want %v (score=%v reason=%q)", got.Pass, tt.want, got.Score, got.Reasoning)
			}
		})
	}
}

// ---- locate-usage --------------------------------------------------------

func TestLocateUsageGrader(t *testing.T) {
	g := NewLocateUsageGrader()
	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{"all four files passes (recall 1.0)", "runner/loop.go, runner/runner.go, daemon/daemon.go, afcli/agent_run.go", true},
		{"three of four passes (recall 0.75)", "runner/loop.go, runner/runner.go, daemon/daemon.go", true},
		{"two of four FAILS (recall 0.5 < 0.75)", "runner/loop.go and runner/runner.go", false},
		{"EMPTY answer FAILS", "", false},
		{"wrong files FAIL", "src/foo.ts and src/bar.ts", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.Grade(ctx(), luCase(), Transcript{FinalAnswer: tt.answer})
			if got.Pass != tt.want {
				t.Errorf("Pass = %v, want %v (recall=%v reason=%q)", got.Pass, tt.want, got.Metadata["recall"], got.Reasoning)
			}
		})
	}
}

// ---- dedup ---------------------------------------------------------------

func TestDedupGrader_Positive(t *testing.T) {
	g := NewDedupGrader()
	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{"yes + correct file passes", "Yes, this is a near-duplicate of afclient/codeintel/gitroot.go.", true},
		{"EMPTY answer FAILS", "", false},
		{"says NOT a dup FAILS", "No, this is not a duplicate of anything in the repo.", false},
		{"yes but wrong file FAILS", "Yes, it duplicates runner/loop.go.", false},
		{"yes but NO file named FAILS", "Yes, this is a duplicate.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.Grade(ctx(), dupCase(true), Transcript{FinalAnswer: tt.answer})
			if got.Pass != tt.want {
				t.Errorf("Pass = %v, want %v (reason=%q)", got.Pass, tt.want, got.Reasoning)
			}
		})
	}
}

func TestDedupGrader_Negative(t *testing.T) {
	g := NewDedupGrader()
	tests := []struct {
		name   string
		answer string
		want   bool
	}{
		{"correctly says NOT a dup passes", "No, this snippet is not a duplicate; it is novel.", true},
		{"EMPTY answer FAILS", "", false},
		{"wrongly says it IS a dup FAILS", "Yes, this duplicates src/lib/memory/hashing.ts.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.Grade(ctx(), dupCase(false), Transcript{FinalAnswer: tt.answer})
			if got.Pass != tt.want {
				t.Errorf("Pass = %v, want %v (reason=%q)", got.Pass, tt.want, got.Reasoning)
			}
		})
	}
}

// ---- tool-use-correctness ------------------------------------------------

func TestToolUseGrader(t *testing.T) {
	g := NewToolUseGrader()
	c := fsCase()

	// Correct tool for find-symbol (MCP FQ name) → pass.
	withCorrect := Transcript{Arm: ArmWith, ToolCalls: []ToolCall{
		{Name: "mcp__af-code-intelligence__af_code_search_symbols"},
	}}
	if got := g.Grade(ctx(), c, withCorrect); !got.Pass {
		t.Errorf("correct tool should pass; got %+v", got)
	}

	// Adoption but WRONG tool (called check_duplicate for a find-symbol task) → fail.
	withWrong := Transcript{Arm: ArmWith, ToolCalls: []ToolCall{
		{Name: "mcp__af-code-intelligence__af_code_check_duplicate"},
	}}
	if got := g.Grade(ctx(), c, withWrong); got.Pass {
		t.Errorf("wrong tool must fail; got %+v", got)
	}

	// NO adoption (only grep) → fail, adopted=false.
	noAdopt := Transcript{Arm: ArmWith, ToolCalls: []ToolCall{{Name: "Bash", Arguments: json.RawMessage(`{"command":"grep -rn X"}`)}}}
	got := g.Grade(ctx(), c, noAdopt)
	if got.Pass {
		t.Errorf("no adoption must fail; got %+v", got)
	}
	if adopted, _ := got.Metadata["adopted"].(bool); adopted {
		t.Errorf("adopted should be false when only grep was used; got %+v", got.Metadata)
	}

	// WITHOUT arm → not applicable (never counts as a pass).
	if got := g.Grade(ctx(), c, Transcript{Arm: ArmWithout}); got.Pass {
		t.Errorf("tool-use grader must not pass on the control arm; got %+v", got)
	}
}

// ---- refactor (LLM judge) ------------------------------------------------

type stubJudge struct {
	score  float64
	err    error
	gotRub string
}

func (s *stubJudge) Score(_ context.Context, rubric, _, _ string) (float64, string, error) {
	s.gotRub = rubric
	return s.score, "stub", s.err
}

func rfCase() Case {
	return Case{
		ID:     "rf",
		Input:  CaseInput{TaskType: TaskRefactorAcrossFiles, Repo: "r", Ref: "s", Prompt: "rename X to Y"},
		Rubric: "Score 1.0 only if all sites renamed.",
		Tags:   []string{tagSuite, "refactor-across-files", tagVersion},
	}
}

func TestRubricGrader(t *testing.T) {
	// High judge score → pass; the rubric must reach the judge.
	hi := &stubJudge{score: 0.9}
	if got := NewRubricGrader(hi).Grade(ctx(), rfCase(), Transcript{FinalAnswer: "did it"}); !got.Pass {
		t.Errorf("score 0.9 should pass; got %+v", got)
	}
	if hi.gotRub == "" {
		t.Error("judge did not receive the rubric")
	}

	// Low judge score (wrong/empty answer the judge marks down) → FAIL.
	lo := &stubJudge{score: 0.2}
	if got := NewRubricGrader(lo).Grade(ctx(), rfCase(), Transcript{FinalAnswer: ""}); got.Pass {
		t.Errorf("score 0.2 must fail; got %+v", got)
	}

	// Judge error → FAIL (never a silent pass).
	er := &stubJudge{err: errors.New("boom")}
	if got := NewRubricGrader(er).Grade(ctx(), rfCase(), Transcript{FinalAnswer: "x"}); got.Pass {
		t.Errorf("judge error must fail; got %+v", got)
	}

	// No judge configured → FAIL (a refactor is never scored pass without a judge).
	if got := NewRubricGrader(nil).Grade(ctx(), rfCase(), Transcript{FinalAnswer: "x"}); got.Pass {
		t.Errorf("nil judge must fail; got %+v", got)
	}
}

// TestCodeIntelToolForFamily pins the WS13-lite family→tool map against
// today's six-tool surface: the expected tool must be the one that actually
// serves the family AND is advertised to that family's WITH arm (WS2 subset).
func TestCodeIntelToolForFamily(t *testing.T) {
	tests := []struct {
		family TaskType
		want   []string
	}{
		{TaskFindSymbol, []string{"af_code_search_symbols"}},
		// locate-usage: search_code lists usage sites; find_type_usages is a
		// type-xref (and not advertised for this family post-WS2).
		{TaskLocateUsage, []string{"af_code_search_code"}},
		{TaskDedup, []string{"af_code_check_duplicate"}},
		// refactor: the xref tool enumerates edit sites; repo-map orients.
		// search_symbols does not enumerate sites and must not count as correct.
		{TaskRefactorAcrossFiles, []string{"af_code_find_type_usages", "af_code_get_repo_map"}},
	}
	for _, tt := range tests {
		got := codeIntelToolForFamily(tt.family)
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %v, want %v", tt.family, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: got %v, want %v", tt.family, got, tt.want)
				break
			}
		}
	}
}
