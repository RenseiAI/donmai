package codeintel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TaskType is one of the four benchmark task families (brief 06 §4.1). Each
// family maps onto the founder's v1 code-intel tools and has a distinct ground
// truth shape.
type TaskType string

const (
	// TaskFindSymbol — "where is X defined?"; ground truth is an exact
	// file + a tolerance line-range for the symbol's definition. Backed by
	// af_code_search_symbols. Objectively checkable.
	TaskFindSymbol TaskType = "find-symbol"
	// TaskLocateUsage — "which files reference X?"; ground truth is the SET of
	// usage-site files (recall/precision scorable). Backed by
	// af_code_find_type_usages / af_code_search_code. Objectively checkable.
	TaskLocateUsage TaskType = "locate-usage"
	// TaskRefactorAcrossFiles — an open-ended multi-file change; graded by an
	// LLM judge against a rubric (no objectively-checkable expectedOutput).
	// Backed by the combination of get-repo-map + find-type-usages +
	// search-symbols.
	TaskRefactorAcrossFiles TaskType = "refactor-across-files"
	// TaskDedup — "is this snippet a duplicate of existing code?"; ground truth
	// is a boolean verdict plus (when true) the file it duplicates. Backed by
	// af_code_check_duplicate. Objectively checkable.
	TaskDedup TaskType = "dedup"
)

// families is the canonical set of the four task families, in report order.
var families = []TaskType{TaskFindSymbol, TaskLocateUsage, TaskRefactorAcrossFiles, TaskDedup}

// Families returns the canonical ordered task-family set.
func Families() []TaskType { return append([]TaskType(nil), families...) }

// isKnownFamily reports whether t is one of the four canonical families.
func isKnownFamily(t TaskType) bool {
	for _, f := range families {
		if f == t {
			return true
		}
	}
	return false
}

// requiredTags every benchmark case must carry, per brief 06 §4.2
// (tags:['codeintel-eval', <family>, 'v1']).
const (
	tagSuite   = "codeintel-eval"
	tagVersion = "v1"
)

// CaseInput is the `input` object of an EvalDatasetCase (brief 06 §4.2):
// {taskType, repo, ref, prompt}. RepoPath is an optional monorepo-subtree
// scope forwarded to the code-intel index root.
type CaseInput struct {
	TaskType TaskType `json:"taskType"`
	// Repo is the owner/name slug the task runs against (e.g. "RenseiAI/donmai").
	Repo string `json:"repo"`
	// Ref is the pinned commit SHA the ground truth was derived from — REQUIRED
	// for reproducibility (never HEAD). The driver provisions the workarea at
	// exactly this ref.
	Ref string `json:"ref"`
	// Prompt is the natural-language task handed to the agent.
	Prompt string `json:"prompt"`
	// RepoPath optionally scopes indexing to a subtree under the worktree root.
	RepoPath string `json:"repoPath,omitempty"`
}

// Case is one benchmark row: the Go mirror of the platform's EvalDatasetCase
// (platform/src/lib/evals/types.ts:65-71), serialised as canonical JSONL.
type Case struct {
	ID             string          `json:"id"`
	Input          CaseInput       `json:"input"`
	ExpectedOutput json.RawMessage `json:"expectedOutput,omitempty"`
	Rubric         string          `json:"rubric,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
}

// Family returns the case's task family.
func (c Case) Family() TaskType { return c.Input.TaskType }

// HasTag reports whether the case carries tag t.
func (c Case) HasTag(t string) bool {
	for _, x := range c.Tags {
		if x == t {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Typed expectedOutput views — one per objectively-checkable family.
// ---------------------------------------------------------------------------

// FindSymbolExpected is the ground truth for a find-symbol task: the exact file
// and a tolerance line-range that must contain the symbol's true definition
// line. The range gives slack for the engine's off-by-one line reporting while
// still failing a wrong file or a wildly wrong location.
type FindSymbolExpected struct {
	File      string `json:"file"`
	LineRange [2]int `json:"lineRange"`
}

// LocateUsageExpected is the ground truth for a locate-usage task: the set of
// usage-site files and the minimum recall (fraction of Files the agent must
// name) required to pass. MinRecall of 1.0 means the set is exhaustive and all
// files must be found.
type LocateUsageExpected struct {
	Files     []string `json:"files"`
	MinRecall float64  `json:"minRecall"`
}

// DedupExpected is the ground truth for a dedup task: the boolean verdict and,
// when the snippet is a duplicate, the file it duplicates.
type DedupExpected struct {
	IsDuplicate bool   `json:"isDuplicate"`
	File        string `json:"file,omitempty"`
}

// FindSymbolExpected decodes the case's expectedOutput as a FindSymbolExpected.
func (c Case) FindSymbolExpected() (FindSymbolExpected, error) {
	var e FindSymbolExpected
	if err := json.Unmarshal(c.ExpectedOutput, &e); err != nil {
		return e, fmt.Errorf("case %s: decode find-symbol expectedOutput: %w", c.ID, err)
	}
	return e, nil
}

// LocateUsageExpected decodes the case's expectedOutput as a LocateUsageExpected.
func (c Case) LocateUsageExpected() (LocateUsageExpected, error) {
	var e LocateUsageExpected
	if err := json.Unmarshal(c.ExpectedOutput, &e); err != nil {
		return e, fmt.Errorf("case %s: decode locate-usage expectedOutput: %w", c.ID, err)
	}
	return e, nil
}

// DedupExpected decodes the case's expectedOutput as a DedupExpected.
func (c Case) DedupExpected() (DedupExpected, error) {
	var e DedupExpected
	if err := json.Unmarshal(c.ExpectedOutput, &e); err != nil {
		return e, fmt.Errorf("case %s: decode dedup expectedOutput: %w", c.ID, err)
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Loading + validation.
// ---------------------------------------------------------------------------

// LoadCases reads canonical JSONL (one Case per non-blank line) from r and
// validates every row. It fails loud on the FIRST invalid case so a malformed
// benchmark can never silently run a degenerate task.
func LoadCases(r io.Reader) ([]Case, error) {
	sc := bufio.NewScanner(r)
	// Cases can carry a long rubric / snippet; raise the line cap well above the
	// 64KiB default.
	sc.Buffer(make([]byte, 0, 1<<16), 4<<20)

	var out []Case
	seen := map[string]struct{}{}
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("line %d: parse case: %w", line, err)
		}
		if err := validateCase(c); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if _, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("line %d: duplicate case id %q", line, c.ID)
		}
		seen[c.ID] = struct{}{}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read jsonl: %w", err)
	}
	return out, nil
}

// LoadCasesFile loads and validates the JSONL benchmark at path.
func LoadCasesFile(path string) ([]Case, error) {
	f, err := os.Open(path) // nolint:gosec // benchmark path is operator/flag-supplied, not user-tainted.
	if err != nil {
		return nil, fmt.Errorf("open benchmark %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	cases, err := LoadCases(f)
	if err != nil {
		return nil, fmt.Errorf("benchmark %q: %w", path, err)
	}
	return cases, nil
}

// LoadCasesDir loads and validates every *.jsonl file under dir (sorted by
// filename for deterministic order) and returns the concatenated, dedup-checked
// case set.
func LoadCasesDir(dir string) ([]Case, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("glob benchmark dir %q: %w", dir, err)
	}
	sort.Strings(entries)
	var out []Case
	seen := map[string]struct{}{}
	for _, p := range entries {
		cases, err := LoadCasesFile(p)
		if err != nil {
			return nil, err
		}
		for _, c := range cases {
			if _, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("duplicate case id %q across benchmark files (in %s)", c.ID, filepath.Base(p))
			}
			seen[c.ID] = struct{}{}
			out = append(out, c)
		}
	}
	return out, nil
}

// validateCase enforces the invariants of brief 06 §4.2: known family, required
// tags, required input fields, and a family-appropriate ground truth
// (expectedOutput for the three objective families; a rubric for refactor).
func validateCase(c Case) error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("case has empty id")
	}
	if !isKnownFamily(c.Input.TaskType) {
		return fmt.Errorf("case %s: unknown taskType %q", c.ID, c.Input.TaskType)
	}
	if strings.TrimSpace(c.Input.Repo) == "" {
		return fmt.Errorf("case %s: input.repo is required", c.ID)
	}
	if strings.TrimSpace(c.Input.Ref) == "" {
		return fmt.Errorf("case %s: input.ref (pinned SHA) is required for reproducibility", c.ID)
	}
	if strings.TrimSpace(c.Input.Prompt) == "" {
		return fmt.Errorf("case %s: input.prompt is required", c.ID)
	}
	if !c.HasTag(tagSuite) || !c.HasTag(string(c.Input.TaskType)) || !c.HasTag(tagVersion) {
		return fmt.Errorf("case %s: tags must include %q, %q and %q (got %v)",
			c.ID, tagSuite, c.Input.TaskType, tagVersion, c.Tags)
	}

	switch c.Input.TaskType {
	case TaskRefactorAcrossFiles:
		if strings.TrimSpace(c.Rubric) == "" {
			return fmt.Errorf("case %s: refactor-across-files requires a non-empty rubric (LLM-judged)", c.ID)
		}
	case TaskFindSymbol:
		if len(c.ExpectedOutput) == 0 {
			return fmt.Errorf("case %s: find-symbol requires expectedOutput", c.ID)
		}
		e, err := c.FindSymbolExpected()
		if err != nil {
			return err
		}
		if strings.TrimSpace(e.File) == "" {
			return fmt.Errorf("case %s: find-symbol expectedOutput.file is required", c.ID)
		}
		if e.LineRange[0] <= 0 || e.LineRange[1] < e.LineRange[0] {
			return fmt.Errorf("case %s: find-symbol expectedOutput.lineRange %v is invalid", c.ID, e.LineRange)
		}
	case TaskLocateUsage:
		if len(c.ExpectedOutput) == 0 {
			return fmt.Errorf("case %s: locate-usage requires expectedOutput", c.ID)
		}
		e, err := c.LocateUsageExpected()
		if err != nil {
			return err
		}
		if len(e.Files) == 0 {
			return fmt.Errorf("case %s: locate-usage expectedOutput.files must be non-empty", c.ID)
		}
		if e.MinRecall <= 0 || e.MinRecall > 1 {
			return fmt.Errorf("case %s: locate-usage expectedOutput.minRecall %v must be in (0,1]", c.ID, e.MinRecall)
		}
	case TaskDedup:
		if len(c.ExpectedOutput) == 0 {
			return fmt.Errorf("case %s: dedup requires expectedOutput", c.ID)
		}
		e, err := c.DedupExpected()
		if err != nil {
			return err
		}
		if e.IsDuplicate && strings.TrimSpace(e.File) == "" {
			return fmt.Errorf("case %s: dedup expectedOutput.file is required when isDuplicate=true", c.ID)
		}
	}
	return nil
}

// CountByFamily returns the number of cases per family, for reporting and for
// the floor check (brief threshold: >=8-12 tasks/family).
func CountByFamily(cases []Case) map[TaskType]int {
	m := map[TaskType]int{}
	for _, c := range cases {
		m[c.Family()]++
	}
	return m
}

// CountByRepo returns the number of cases per repo slug.
func CountByRepo(cases []Case) map[string]int {
	m := map[string]int{}
	for _, c := range cases {
		m[c.Input.Repo]++
	}
	return m
}
