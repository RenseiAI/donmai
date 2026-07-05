package codeintel

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Grader IDs — registry keys mirroring the platform grader naming
// (structural/… and model-grader/…), brief 06 §4.4.
const (
	GraderFindSymbol  = "structural/codeintel-find-symbol-v1"
	GraderLocateUsage = "structural/codeintel-locate-usage-v1"
	GraderDedup       = "structural/codeintel-dedup-v1"
	GraderToolUse     = "structural/codeintel-tooluse-v1"
	GraderRefactor    = "model-grader/codeintel-refactor-v1"
)

// rubricPassThreshold is the LLM-judge pass bar (ADR-017 ModelGrader default 0.7).
const rubricPassThreshold = 0.7

// Grader scores one transcript against one benchmark case.
type Grader interface {
	ID() string
	Grade(ctx context.Context, c Case, tr Transcript) GradeResult
}

// ---------------------------------------------------------------------------
// find-symbol: the answer must name the right file AND a line in the tolerance
// window. Mirrors implement-result.ts — a string-refinement over the agent's
// final response, with reasoning naming the missing signal.
// ---------------------------------------------------------------------------

type findSymbolGrader struct{}

// NewFindSymbolGrader returns the find-symbol task-success grader.
func NewFindSymbolGrader() Grader { return findSymbolGrader{} }

func (findSymbolGrader) ID() string { return GraderFindSymbol }

func (g findSymbolGrader) Grade(_ context.Context, c Case, tr Transcript) GradeResult {
	exp, err := c.FindSymbolExpected()
	if err != nil {
		return fail(g.ID(), "malformed expectedOutput: "+err.Error())
	}
	answer := strings.TrimSpace(tr.FinalAnswer)
	if answer == "" {
		return fail(g.ID(), "empty answer: no file:line named")
	}
	fileOK := answerMentionsFile(answer, exp.File)
	lineOK := answerHasLineInRange(answer, exp.LineRange)
	meta := map[string]any{"expectedFile": exp.File, "fileNamed": fileOK, "lineInRange": lineOK}
	switch {
	case fileOK && lineOK:
		return GradeResult{GraderID: g.ID(), Score: 1, Pass: true, Metadata: meta}
	case !fileOK:
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
			Reasoning: fmt.Sprintf("answer does not name the expected file %q", exp.File),
		}
	default:
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
			Reasoning: fmt.Sprintf("answer names the file but no line in the expected range %v", exp.LineRange),
		}
	}
}

// ---------------------------------------------------------------------------
// locate-usage: recall over the expected usage-site file set.
// ---------------------------------------------------------------------------

type locateUsageGrader struct{}

// NewLocateUsageGrader returns the locate-usage task-success grader.
func NewLocateUsageGrader() Grader { return locateUsageGrader{} }

func (locateUsageGrader) ID() string { return GraderLocateUsage }

func (g locateUsageGrader) Grade(_ context.Context, c Case, tr Transcript) GradeResult {
	exp, err := c.LocateUsageExpected()
	if err != nil {
		return fail(g.ID(), "malformed expectedOutput: "+err.Error())
	}
	want := sortedUnique(exp.Files)
	var found, missing []string
	for _, f := range want {
		if answerMentionsFile(tr.FinalAnswer, f) {
			found = append(found, f)
		} else {
			missing = append(missing, f)
		}
	}
	recall := 0.0
	if len(want) > 0 {
		recall = float64(len(found)) / float64(len(want))
	}
	meta := map[string]any{
		"recall": recall, "minRecall": exp.MinRecall,
		"foundFiles": found, "missingFiles": missing,
	}
	if recall+1e-9 >= exp.MinRecall {
		return GradeResult{GraderID: g.ID(), Score: recall, Pass: true, Metadata: meta}
	}
	return GradeResult{
		GraderID: g.ID(), Score: recall, Pass: false, Metadata: meta,
		Reasoning: fmt.Sprintf("recall %.2f < required %.2f; missing %v", recall, exp.MinRecall, missing),
	}
}

// ---------------------------------------------------------------------------
// dedup: verdict (+ file when duplicate) must match.
// ---------------------------------------------------------------------------

type dedupGrader struct{}

// NewDedupGrader returns the dedup task-success grader.
func NewDedupGrader() Grader { return dedupGrader{} }

func (dedupGrader) ID() string { return GraderDedup }

// dedupVerdict classifies an answer as positive (is a dup), negative (not a
// dup), or unknown. Negative phrases are checked first because "not a duplicate"
// contains "a duplicate".
type verdict int

const (
	verdictUnknown verdict = iota
	verdictPositive
	verdictNegative
)

func classifyDedup(answer string) verdict {
	a := strings.ToLower(answer)
	if strings.TrimSpace(a) == "" {
		return verdictUnknown
	}
	negatives := []string{
		"not a duplicate", "not a dup", "no duplicate", "isn't a duplicate",
		"is not a duplicate", "not a near-duplicate", "no near-duplicate", "novel", "not duplicated",
	}
	for _, n := range negatives {
		if strings.Contains(a, n) {
			return verdictNegative
		}
	}
	positives := []string{
		"is a duplicate", "near-duplicate", "near duplicate", "duplicate of",
		"it duplicates", "duplicates ", "is a dup", "exact duplicate",
	}
	for _, p := range positives {
		if strings.Contains(a, p) {
			return verdictPositive
		}
	}
	// Bare yes/no as a last resort.
	if strings.HasPrefix(a, "no") {
		return verdictNegative
	}
	if strings.HasPrefix(a, "yes") {
		return verdictPositive
	}
	return verdictUnknown
}

func (g dedupGrader) Grade(_ context.Context, c Case, tr Transcript) GradeResult {
	exp, err := c.DedupExpected()
	if err != nil {
		return fail(g.ID(), "malformed expectedOutput: "+err.Error())
	}
	v := classifyDedup(tr.FinalAnswer)
	meta := map[string]any{"expectedIsDuplicate": exp.IsDuplicate, "verdict": v}
	if exp.IsDuplicate {
		if v != verdictPositive {
			return GradeResult{
				GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
				Reasoning: "expected a DUPLICATE verdict; answer did not affirm a duplicate",
			}
		}
		if !answerMentionsFile(tr.FinalAnswer, exp.File) {
			return GradeResult{
				GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
				Reasoning: fmt.Sprintf("affirmed a duplicate but did not name the expected file %q", exp.File),
			}
		}
		return GradeResult{GraderID: g.ID(), Score: 1, Pass: true, Metadata: meta}
	}
	// Expected NOT a duplicate.
	if v != verdictNegative {
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
			Reasoning: "expected a NOT-a-duplicate verdict; answer did not deny a duplicate",
		}
	}
	return GradeResult{GraderID: g.ID(), Score: 1, Pass: true, Metadata: meta}
}

// ---------------------------------------------------------------------------
// tool-use-correctness (WITH arm only): adoption + correct tool choice.
// ---------------------------------------------------------------------------

type toolUseGrader struct{}

// NewToolUseGrader returns the tool-use-correctness grader (brief 06 §4.4.3).
func NewToolUseGrader() Grader { return toolUseGrader{} }

func (toolUseGrader) ID() string { return GraderToolUse }

// codeIntelToolForFamily maps a family to the canonical af_code_* tool(s) the
// agent should invoke.
func codeIntelToolForFamily(f TaskType) []string {
	switch f {
	case TaskFindSymbol:
		return []string{"af_code_search_symbols"}
	case TaskLocateUsage:
		return []string{"af_code_find_type_usages", "af_code_search_code"}
	case TaskDedup:
		return []string{"af_code_check_duplicate"}
	case TaskRefactorAcrossFiles:
		return []string{"af_code_get_repo_map", "af_code_find_type_usages", "af_code_search_symbols"}
	default:
		return nil
	}
}

// isCodeIntelTool reports whether a captured tool-call name/args references any
// code-intel tool — an MCP FQ name (…af_code_*) or a CLI-shim call
// (`donmai code …`).
func isCodeIntelTool(text string) bool {
	if strings.Contains(text, "af_code_") {
		return true
	}
	low := strings.ToLower(text)
	for _, frag := range []string{
		"donmai code", "code search-symbols", "find-type-usages",
		"check-duplicate", "get-repo-map", "search-code", "search-symbols",
	} {
		if strings.Contains(low, frag) {
			return true
		}
	}
	return false
}

// toolCallText flattens a tool call's name + arguments for matching (a Bash tool
// call carries the real command in its arguments).
func toolCallText(tc ToolCall) string {
	return tc.Name + " " + string(tc.Arguments)
}

func (g toolUseGrader) Grade(_ context.Context, c Case, tr Transcript) GradeResult {
	if tr.Arm != ArmWith {
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false,
			Reasoning: "tool-use grader is WITH-arm only; not applicable to the control arm",
			Metadata:  map[string]any{"applicable": false},
		}
	}
	want := codeIntelToolForFamily(c.Family())
	adopted := false
	correct := false
	for _, tc := range tr.ToolCalls {
		text := toolCallText(tc)
		if isCodeIntelTool(text) {
			adopted = true
		}
		for _, w := range want {
			if strings.Contains(text, w) {
				correct = true
			}
		}
	}
	meta := map[string]any{"applicable": true, "adopted": adopted, "correctTool": correct, "expectedTools": want}
	switch {
	case correct:
		return GradeResult{GraderID: g.ID(), Score: 1, Pass: true, Metadata: meta}
	case adopted:
		return GradeResult{
			GraderID: g.ID(), Score: 0.5, Pass: false, Metadata: meta,
			Reasoning: fmt.Sprintf("adopted a code-intel tool but not the one matched to %s (%v)", c.Family(), want),
		}
	default:
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false, Metadata: meta,
			Reasoning: "no code-intel tool was invoked despite the WITH-arm advertisement",
		}
	}
}

// ---------------------------------------------------------------------------
// refactor: LLM-judge against the rubric.
// ---------------------------------------------------------------------------

// Judge is the LLM-as-judge seam (brief 06 §4.4.2, ADR-017 ModelGrader). A real
// implementation calls a cross-family judge model over an OpenAI-compatible
// endpoint; tests inject a deterministic stub. Returning an error is a grader
// failure, not a pass.
type Judge interface {
	Score(ctx context.Context, rubric, input, output string) (score float64, reasoning string, err error)
}

type rubricGrader struct{ judge Judge }

// NewRubricGrader returns the refactor-across-files grader. When judge is nil the
// grader cannot pass — a refactor is never scored PASS without a judge (so an
// empty/wrong answer can never sneak through when no judge is configured).
func NewRubricGrader(judge Judge) Grader { return rubricGrader{judge: judge} }

func (rubricGrader) ID() string { return GraderRefactor }

func (g rubricGrader) Grade(ctx context.Context, c Case, tr Transcript) GradeResult {
	if g.judge == nil {
		return GradeResult{
			GraderID: g.ID(), Score: 0, Pass: false,
			Reasoning: "no judge configured: refactor-across-files requires an LLM judge; not scored",
		}
	}
	score, reasoning, err := g.judge.Score(ctx, c.Rubric, c.Input.Prompt, tr.FinalAnswer)
	if err != nil {
		return fail(g.ID(), "judge error: "+err.Error())
	}
	return GradeResult{
		GraderID:  g.ID(),
		Score:     score,
		Pass:      score >= rubricPassThreshold,
		Reasoning: reasoning,
		Metadata:  map[string]any{"threshold": rubricPassThreshold, "judged": true},
	}
}

// ---------------------------------------------------------------------------
// Shared helpers.
// ---------------------------------------------------------------------------

// fail returns a zero-score failing grade with a reason.
func fail(id, reason string) GradeResult {
	return GradeResult{GraderID: id, Score: 0, Pass: false, Reasoning: reason}
}

// intRe matches standalone integers (line numbers) in an answer.
var intRe = regexp.MustCompile(`\d+`)

// answerHasLineInRange reports whether any integer in the answer falls within
// [lo, hi] inclusive.
func answerHasLineInRange(answer string, r [2]int) bool {
	for _, m := range intRe.FindAllString(answer, -1) {
		n, err := strconv.Atoi(m)
		if err != nil {
			continue
		}
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

// normalizePath lowercases and forward-slashes a path fragment for matching.
func normalizePath(p string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
}

// answerMentionsFile reports whether the answer text references file by its full
// normalized path (substring match — the primary signal, robust to surrounding
// prose and line-number suffixes).
func answerMentionsFile(answer, file string) bool {
	na := normalizePath(answer)
	nf := normalizePath(file)
	return nf != "" && strings.Contains(na, nf)
}

// TaskGraderFor returns the objective task-success grader for a family, or nil
// for refactor (which needs a Judge — use NewRubricGrader).
func TaskGraderFor(f TaskType) Grader {
	switch f {
	case TaskFindSymbol:
		return NewFindSymbolGrader()
	case TaskLocateUsage:
		return NewLocateUsageGrader()
	case TaskDedup:
		return NewDedupGrader()
	default:
		return nil
	}
}

// sortedUnique returns the sorted, de-duplicated set of s.
func sortedUnique(s []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(s))
	for _, x := range s {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
