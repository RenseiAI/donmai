package codeintel

import (
	"os"
	"strings"
	"testing"

	engine "github.com/RenseiAI/donmai/afclient/codeintel"
)

// benchmarkDir is the in-repo canonical benchmark location, relative to this
// package directory (eval/codeintel/). Kept alongside the Go engine's own test
// data per brief 06 §4.2.
const benchmarkDir = "../../afclient/codeintel/testdata/eval-benchmark"

func TestLoadPromptExperimentCasesAcceptsGenericTaskFamiliesAndProbeContracts(t *testing.T) {
	input := `{"id":"e2-simple-donmai-001","input":{"taskType":"development-simple","repo":"RenseiAI/donmai","ref":"deadbeef","prompt":"Complete the fixture."},"expectedOutput":{"delegationProbe":{"requiredOutputIncludes":["fixture complete"],"delegationPolicy":"forbid","subagentToolNames":["Task"]}},"tags":["e2","simple"]}` + "\n"
	cases, err := LoadPromptExperimentCases(strings.NewReader(input))
	if err != nil {
		t.Fatalf("LoadPromptExperimentCases: %v", err)
	}
	if len(cases) != 1 || cases[0].Input.TaskType != "development-simple" {
		t.Fatalf("cases = %+v", cases)
	}
}

func TestLoadPromptExperimentCasesRejectsTrailingJSONValue(t *testing.T) {
	input := `{"id":"e2-simple-donmai-001","input":{"taskType":"development-simple","repo":"RenseiAI/donmai","ref":"deadbeef","prompt":"Complete the fixture."},"expectedOutput":{"probe":{"required":true}}} {}` + "\n"
	_, err := LoadPromptExperimentCases(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("error = %v, want strict single-value failure", err)
	}
}

func TestLoadPromptExperimentCasesRejectsMissingProbeContract(t *testing.T) {
	input := `{"id":"e2-simple-donmai-001","input":{"taskType":"development-simple","repo":"RenseiAI/donmai","ref":"deadbeef","prompt":"Complete the fixture."},"expectedOutput":{}}` + "\n"
	_, err := LoadPromptExperimentCases(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "expectedOutput") {
		t.Fatalf("error = %v, want expectedOutput failure", err)
	}
}

// TestBenchmark_Loads_AndMeetsMatrix loads the shipped benchmark and asserts the
// family/repo matrix from brief 06 §4.1 + the locked power floor (>=8 tasks per
// family, both dogfood repos represented in each family).
func TestBenchmark_Loads_AndMeetsMatrix(t *testing.T) {
	cases, err := LoadCasesDir(benchmarkDir)
	if err != nil {
		t.Fatalf("LoadCasesDir(%s): %v", benchmarkDir, err)
	}
	if len(cases) == 0 {
		t.Fatalf("benchmark is empty")
	}

	// The committed OSS benchmark holds only this repo's (public) cases; the
	// second dogfood repo's cases are supplied privately at eval time. The
	// founder's full-corpus floor (>=8/family across both repos) is enforced at
	// run time by the driver's corpus-power gate (computeAggregate). Here we
	// assert the self-repo contribution meets a per-repo floor so the committed
	// half never silently thins out.
	const perRepoFloor = 4
	byFam := CountByFamily(cases)
	for _, fam := range Families() {
		if byFam[fam] < perRepoFloor {
			t.Errorf("family %s has %d committed cases; per-repo floor is >=%d", fam, byFam[fam], perRepoFloor)
		}
	}

	// Every committed case must name this (public) dogfood repo.
	perFamRepo := map[TaskType]map[string]int{}
	for _, c := range cases {
		if perFamRepo[c.Family()] == nil {
			perFamRepo[c.Family()] = map[string]int{}
		}
		perFamRepo[c.Family()][c.Input.Repo]++
	}
	// The committed OSS benchmark ships this repo's own (public) cases; the
	// second dogfood repo's cases are supplied at eval time from a private dir
	// (see resolveRepoRoots / --benchmark-dir), so only the self-repo family
	// coverage is asserted here.
	for _, fam := range Families() {
		if perFamRepo[fam]["RenseiAI/donmai"] == 0 {
			t.Errorf("family %s has no donmai cases", fam)
		}
	}
}

// TestBenchmark_GroundTruth_SpotChecks pins a few hand-verified ground-truth
// facts so a fixture edit that corrupts them fails loudly. These lines were
// verified by `git grep -n` against the pinned SHAs.
func TestBenchmark_GroundTruth_SpotChecks(t *testing.T) {
	cases, err := LoadCasesDir(benchmarkDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	byID := map[string]Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}

	// find-symbol: newAgentRunCmd is defined at afcli/agent_run.go:80 (verified);
	// the tolerance window must contain 80.
	fs, ok := byID["codeintel-find-symbol-donmai-001"]
	if !ok {
		t.Fatal("missing case codeintel-find-symbol-donmai-001")
	}
	fse, err := fs.FindSymbolExpected()
	if err != nil {
		t.Fatalf("decode find-symbol expected: %v", err)
	}
	if fse.File != "afcli/agent_run.go" {
		t.Errorf("find-symbol-001 file = %q, want afcli/agent_run.go", fse.File)
	}
	if !(fse.LineRange[0] <= 80 && 80 <= fse.LineRange[1]) {
		t.Errorf("find-symbol-001 lineRange %v does not contain the true def line 80", fse.LineRange)
	}

	// locate-usage: worktree.Manager consumers are exactly these 4 non-test files.
	lu := byID["codeintel-locate-usage-donmai-001"]
	lue, err := lu.LocateUsageExpected()
	if err != nil {
		t.Fatalf("decode locate-usage expected: %v", err)
	}
	wantFiles := map[string]bool{
		"afcli/agent_run.go": true, "daemon/daemon.go": true,
		"runner/loop.go": true, "runner/runner.go": true,
	}
	if len(lue.Files) != len(wantFiles) {
		t.Errorf("locate-usage-001 has %d files, want %d", len(lue.Files), len(wantFiles))
	}
	for _, f := range lue.Files {
		if !wantFiles[f] {
			t.Errorf("locate-usage-001 unexpected file %q", f)
		}
	}

	// dedup: the FindGitRoot near-dup is a true positive against gitroot.go.
	dd, err := byID["codeintel-dedup-donmai-001"].DedupExpected()
	if err != nil {
		t.Fatalf("decode dedup expected: %v", err)
	}
	if !dd.IsDuplicate || dd.File != "afclient/codeintel/gitroot.go" {
		t.Errorf("dedup-001 = %+v, want {true, afclient/codeintel/gitroot.go}", dd)
	}

	// refactor: rubric present, no expectedOutput requirement.
	rf := byID["codeintel-refactor-donmai-001"]
	if strings.TrimSpace(rf.Rubric) == "" {
		t.Error("refactor-donmai-001 must carry a rubric")
	}
}

// TestLoadCases_RejectsMalformed proves the loader fails LOUD on every invariant
// violation — a degenerate benchmark case must never silently run.
func TestLoadCases_RejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "unknown family",
			line: `{"id":"x","input":{"taskType":"bogus","repo":"r","ref":"s","prompt":"p"},"tags":["codeintel-eval","bogus","v1"]}`,
			want: "unknown taskType",
		},
		{
			name: "missing required tags",
			line: `{"id":"x","input":{"taskType":"find-symbol","repo":"r","ref":"s","prompt":"p"},"expectedOutput":{"file":"a.go","lineRange":[1,2]},"tags":["find-symbol"]}`,
			want: "tags must include",
		},
		{
			name: "find-symbol without expectedOutput",
			line: `{"id":"x","input":{"taskType":"find-symbol","repo":"r","ref":"s","prompt":"p"},"tags":["codeintel-eval","find-symbol","v1"]}`,
			want: "find-symbol requires expectedOutput",
		},
		{
			name: "find-symbol invalid lineRange",
			line: `{"id":"x","input":{"taskType":"find-symbol","repo":"r","ref":"s","prompt":"p"},"expectedOutput":{"file":"a.go","lineRange":[5,2]},"tags":["codeintel-eval","find-symbol","v1"]}`,
			want: "lineRange",
		},
		{
			name: "locate-usage empty files",
			line: `{"id":"x","input":{"taskType":"locate-usage","repo":"r","ref":"s","prompt":"p"},"expectedOutput":{"files":[],"minRecall":1.0},"tags":["codeintel-eval","locate-usage","v1"]}`,
			want: "files must be non-empty",
		},
		{
			name: "refactor without rubric",
			line: `{"id":"x","input":{"taskType":"refactor-across-files","repo":"r","ref":"s","prompt":"p"},"expectedOutput":null,"tags":["codeintel-eval","refactor-across-files","v1"]}`,
			want: "requires a non-empty rubric",
		},
		{
			name: "dedup positive missing file",
			line: `{"id":"x","input":{"taskType":"dedup","repo":"r","ref":"s","prompt":"p"},"expectedOutput":{"isDuplicate":true},"tags":["codeintel-eval","dedup","v1"]}`,
			want: "file is required when isDuplicate=true",
		},
		{
			name: "missing ref",
			line: `{"id":"x","input":{"taskType":"find-symbol","repo":"r","ref":"","prompt":"p"},"expectedOutput":{"file":"a.go","lineRange":[1,2]},"tags":["codeintel-eval","find-symbol","v1"]}`,
			want: "input.ref",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCases(strings.NewReader(tt.line))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.want)
			}
		})
	}
}

// TestLoadCases_RejectsDuplicateID proves a repeated id fails loud.
func TestLoadCases_RejectsDuplicateID(t *testing.T) {
	dup := `{"id":"dupe","input":{"taskType":"find-symbol","repo":"r","ref":"s","prompt":"p"},"expectedOutput":{"file":"a.go","lineRange":[1,2]},"tags":["codeintel-eval","find-symbol","v1"]}`
	_, err := LoadCases(strings.NewReader(dup + "\n" + dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate case id") {
		t.Fatalf("expected duplicate-id error, got %v", err)
	}
}

// TestEngineLine_MatchesGitGrepLine pins the WS8 line-attribution contract on
// live seed symbols from this repo: the engine's reported symbol line must
// equal the 1-based line of the declaration keyword itself — exactly what
// `git grep -n '^func <name>'` returns — with no tolerance window.
func TestEngineLine_MatchesGitGrepLine(t *testing.T) {
	seeds := []struct {
		file   string
		symbol string
		prefix string
	}{
		{"../../afcli/agent_run.go", "newAgentRunCmd", "func newAgentRunCmd("},
		{"../../afclient/codeintel/gitroot.go", "FindGitRoot", "func FindGitRoot("},
	}
	for _, s := range seeds {
		t.Run(s.symbol, func(t *testing.T) {
			data, err := os.ReadFile(s.file)
			if err != nil {
				t.Fatalf("read %s: %v", s.file, err)
			}
			src := string(data)

			// grep-equivalent: 1-based line of the declaration keyword.
			grepLine := 0
			for i, line := range strings.Split(src, "\n") {
				if strings.HasPrefix(line, s.prefix) {
					grepLine = i + 1
					break
				}
			}
			if grepLine == 0 {
				t.Fatalf("seed symbol %s not found in %s (repo drifted?)", s.symbol, s.file)
			}

			ext := &engine.GoExtractor{}
			ast := ext.Extract(src, s.file)
			var got int
			for _, sym := range ast.Symbols {
				if sym.Name == s.symbol {
					got = sym.Line
					break
				}
			}
			if got == 0 {
				t.Fatalf("engine did not extract symbol %s from %s", s.symbol, s.file)
			}
			if got != grepLine {
				t.Errorf("engine line %d != git-grep line %d for %s (no tolerance window allowed)",
					got, grepLine, s.symbol)
			}
		})
	}
}
