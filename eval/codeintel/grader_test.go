package codeintel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// ---- F3: family-aware correct-skip policy ---------------------------------

// TestToolUseGrader_FindSymbol_CorrectSkip pins the correct-skip policy: the
// WS4 advertisement tells the WITH arm "for an exact single-identifier lookup,
// plain grep is fine — do not add a tool call", so a find-symbol trial with
// ZERO code-intel calls AND a passing task grade is a CORRECT skip (Pass true,
// correctSkip metadata), not an adoption failure. adopted stays false so the
// adoption-rate metric remains honest.
func TestToolUseGrader_FindSymbol_CorrectSkip(t *testing.T) {
	g := NewToolUseGrader()
	grepOnly := []ToolCall{{Name: "Bash", Arguments: json.RawMessage(`{"command":"grep -rn newAgentRunCmd"}`)}}

	// Zero adoption + task SUCCEEDED (answer names expected file+line) → correct skip.
	pass := g.Grade(ctx(), fsCase(), Transcript{
		Arm: ArmWith, ToolCalls: grepOnly,
		FinalAnswer: "Defined at afcli/agent_run.go:80.",
	})
	if !pass.Pass {
		t.Errorf("zero-adoption find-symbol with a PASSING task grade must be a correct skip; got %+v", pass)
	}
	if cs, _ := pass.Metadata["correctSkip"].(bool); !cs {
		t.Errorf("correct skip must set metadata correctSkip=true; got %+v", pass.Metadata)
	}
	if adopted, _ := pass.Metadata["adopted"].(bool); adopted {
		t.Errorf("a correct skip must NOT count as adoption; got %+v", pass.Metadata)
	}
	if strings.Contains(pass.Reasoning, "despite the WITH-arm advertisement") {
		t.Errorf("correct-skip reasoning must not claim the advertisement demanded adoption; got %q", pass.Reasoning)
	}

	// Zero adoption + task FAILED → still a fail (the tool might have helped).
	fail := g.Grade(ctx(), fsCase(), Transcript{
		Arm: ArmWith, ToolCalls: grepOnly,
		FinalAnswer: "I could not find it.",
	})
	if fail.Pass {
		t.Errorf("zero-adoption find-symbol with a FAILING task grade must fail; got %+v", fail)
	}
	if cs, _ := fail.Metadata["correctSkip"].(bool); cs {
		t.Errorf("failed task must not be marked correctSkip; got %+v", fail.Metadata)
	}
}

// TestToolUseGrader_Refactor_NoTools_StillFails: the de-scope clause covers
// ONLY the find-symbol family; a refactor trial with zero code-intel calls
// fails the tool-use grade regardless of the task outcome.
func TestToolUseGrader_Refactor_NoTools_StillFails(t *testing.T) {
	g := NewToolUseGrader()
	got := g.Grade(ctx(), rfCase(), Transcript{
		Arm:         ArmWith,
		ToolCalls:   []ToolCall{{Name: "Bash", Arguments: json.RawMessage(`{"command":"sed -i s/X/Y/ *.go"}`)}},
		FinalAnswer: "renamed all sites",
	})
	if got.Pass {
		t.Errorf("zero-adoption refactor must still fail the tool-use grade; got %+v", got)
	}
	if cs, _ := got.Metadata["correctSkip"].(bool); cs {
		t.Errorf("refactor must never be marked correctSkip; got %+v", got.Metadata)
	}
}

// ---- F7: family→tool map vs advertised subset ------------------------------

// TestCodeIntelToolForFamily_SubsetOfAdvertised is the property pin behind the
// WS13-lite map: for EVERY family, every tool the grader expects must be in
// the WS2 default advertised subset for that family — the grader may never
// demand a tool the WITH arm was not shown.
func TestCodeIntelToolForFamily_SubsetOfAdvertised(t *testing.T) {
	for _, f := range families {
		advertised := map[string]bool{}
		for _, n := range advertisedToolSubset(f, false) {
			advertised[n] = true
		}
		for _, n := range codeIntelToolForFamily(f) {
			if !advertised[n] {
				t.Errorf("family %s: grader expects %s, but the WS2 subset %v never advertises it",
					f, n, advertisedToolSubset(f, false))
			}
		}
	}
}

// TestToolUseGrader_LocateUsage_TypeUsagesOnlyDoesNotPass is the behavioral
// side of the map pin: find_type_usages is a type-xref, is NOT advertised to
// the locate-usage family, and must not count as the correct tool there —
// only partial credit for adoption, never a pass.
func TestToolUseGrader_LocateUsage_TypeUsagesOnlyDoesNotPass(t *testing.T) {
	g := NewToolUseGrader()
	tr := Transcript{Arm: ArmWith, ToolCalls: []ToolCall{
		{Name: "mcp__af-code-intelligence__af_code_find_type_usages"},
	}}
	got := g.Grade(ctx(), luCase(), tr)
	if got.Pass {
		t.Errorf("locate-usage graded PASS on find_type_usages alone; got %+v", got)
	}
	if adopted, _ := got.Metadata["adopted"].(bool); !adopted {
		t.Errorf("find_type_usages is still a code-intel adoption; got %+v", got.Metadata)
	}
	if correct, _ := got.Metadata["correctTool"].(bool); correct {
		t.Errorf("find_type_usages must not be the correct tool for locate-usage; got %+v", got.Metadata)
	}
}
