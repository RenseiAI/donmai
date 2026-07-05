package codeintel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTempRepo creates a throwaway git repo with a known Go file and returns its
// path and HEAD SHA — a fast, self-contained provisioning source.
func initTempRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) // nolint:gosec // fixed git command; args are test-controlled
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package x\n\nfunc Target() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	sha := run("rev-parse", "HEAD")
	return dir, sha
}

// recordingExecutor captures the ArmSpecs it receives and returns a deterministic
// transcript that makes the WITH arm succeed and the WITHOUT arm fail — enough to
// exercise the driver's provisioning, env wiring, grading, and aggregation
// without a live MCP server.
type recordingExecutor struct {
	specs       []ArmSpec
	sawWorkFile map[Arm]bool
	// donmaiResolvable records, per arm, whether donmai resolved on the arm's
	// PATH AT EXECUTE TIME. The driver stages the WITH arm's donmai into a
	// per-run temp dir it tears down right after each arm returns, so this must
	// be captured here (before cleanup), not by inspecting spec.Env after Run.
	donmaiResolvable map[Arm]bool
}

func (e *recordingExecutor) Name() string { return "recording" }

func (e *recordingExecutor) Execute(_ context.Context, spec ArmSpec) (Transcript, error) {
	if e.sawWorkFile == nil {
		e.sawWorkFile = map[Arm]bool{}
	}
	if e.donmaiResolvable == nil {
		e.donmaiResolvable = map[Arm]bool{}
	}
	e.specs = append(e.specs, spec)
	_, e.donmaiResolvable[spec.Arm] = BinaryOnPath("donmai", envPath(spec.Env))
	// Confirm provisioning materialised the pinned content into the workarea.
	if _, err := os.Stat(filepath.Join(spec.Workarea, "foo.go")); err == nil {
		e.sawWorkFile[spec.Arm] = true
	}
	snap := &SnapshotRef{Provider: "local", SnapshotID: spec.SnapshotID, Retain: RetainEvalPermanent}
	if spec.Arm == ArmWith {
		return Transcript{
			Arm: ArmWith, FinalAnswer: "foo.go:3",
			ToolCalls: []ToolCall{{Name: "mcp__af-code-intelligence__af_code_search_symbols"}},
			TurnCount: 1, TokenCounts: TokenCounts{Input: 100, Output: 20},
			SnapshotRef: snap, AdvertisedTools: spec.AdvertisedTools,
		}, nil
	}
	return Transcript{
		Arm: ArmWithout, FinalAnswer: "I could not find it.",
		ToolCalls: []ToolCall{{Name: "Bash", Arguments: json.RawMessage(`{"command":"grep -rn Target"}`)}},
		TurnCount: 1, TokenCounts: TokenCounts{Input: 400, Output: 80},
		SnapshotRef: snap,
	}, nil
}

func TestDriver_EndToEnd_ProvisionArmsGradeAggregate(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")

	c := Case{
		ID:             "codeintel-find-symbol-test-001",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Where is the function Target defined?"},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}

	rec := &recordingExecutor{}
	d, err := NewDriver(Config{
		Trials:         1,
		Advertise:      AdvertiseMCP,
		DonmaiBin:      filepath.Join(donmaiDir, "donmai"),
		RepoRoots:      map[string]string{"test/repo": repoDir},
		WorkareaParent: t.TempDir(),
		Executor:       rec,
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := d.Run(context.Background(), []Case{c})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two arms x one trial.
	if len(rep.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(rep.Records))
	}
	// Provisioning materialised the pinned file for BOTH arms (two fresh workareas).
	if !rec.sawWorkFile[ArmWith] || !rec.sawWorkFile[ArmWithout] {
		t.Errorf("both arms must be provisioned at the pinned ref with foo.go present; got %+v", rec.sawWorkFile)
	}

	// PATH wiring: WITH resolves donmai; WITHOUT does not (the contamination
	// guard). Checked at EXECUTE time (see recordingExecutor.donmaiResolvable) —
	// the driver stages the WITH-arm donmai in a per-run temp dir it removes after
	// each arm, so a post-Run env inspection would race that teardown.
	if !rec.donmaiResolvable[ArmWith] {
		t.Error("WITH arm env must resolve donmai (at execute time)")
	}
	if rec.donmaiResolvable[ArmWithout] {
		t.Error("WITHOUT arm env must NOT resolve donmai (control contamination)")
	}

	// Grading + delta: WITH passes, WITHOUT fails → +100pp on find-symbol.
	fam := rep.Families[TaskFindSymbol]
	if fam.WithPasses != 1 || fam.WithoutPasses != 0 {
		t.Errorf("expected WITH pass / WITHOUT fail; got with=%d without=%d", fam.WithPasses, fam.WithoutPasses)
	}
	if fam.DeltaPP() != 100 {
		t.Errorf("family delta = %.0f, want 100", fam.DeltaPP())
	}
	if fam.AdoptionRate() != 1.0 {
		t.Errorf("adoption rate = %.2f, want 1.0", fam.AdoptionRate())
	}
	// Tokens: WITH cheaper than WITHOUT → ratio < 1.
	if fam.TokenRatio() >= 1.0 {
		t.Errorf("token ratio = %.2f, want < 1", fam.TokenRatio())
	}
	if rep.Aggregate.DeltaPP != 100 {
		t.Errorf("aggregate delta = %.0f, want 100", rep.Aggregate.DeltaPP)
	}

	// The report Summary renders without panicking and mentions the family.
	if !strings.Contains(rep.Summary(), "find-symbol") {
		t.Error("summary should mention find-symbol")
	}

	// The WITH record carries a cross-linked run+trace envelope.
	for _, r := range rep.Records {
		if r.Envelope.Run.DatasetCaseID != c.ID {
			t.Errorf("envelope datasetCaseId = %q, want %q", r.Envelope.Run.DatasetCaseID, c.ID)
		}
	}
}

// writeExecInto drops an executable file `name` into an existing dir (unlike
// writeFakeBinary, which makes its own temp dir) — so several tools can share a
// single directory, simulating a co-install location like /opt/homebrew/bin.
func writeExecInto(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+name+"\n"), 0o755); err != nil { // nolint:gosec // must be executable to exercise PATH resolution
		t.Fatalf("write exec %s: %v", name, err)
	}
}

// TestBuildArmSpec_SymmetricNonDonmaiTools is the contamination-guard proof for
// the dogfooding host where donmai is co-installed ALONGSIDE baseline tools
// (rg/gh/git) in one shared PATH dir. The two arms' PATHs must resolve an
// IDENTICAL set of non-donmai tools and differ ONLY on donmai — otherwise the
// A/B delta is inflated by the control losing baseline search tools the
// treatment keeps.
func TestBuildArmSpec_SymmetricNonDonmaiTools(t *testing.T) {
	// Simulate /opt/homebrew/bin: donmai + rg + gh + git in ONE directory.
	shared := t.TempDir()
	for _, name := range []string{"donmai", "rg", "gh", "git"} {
		writeExecInto(t, shared, name)
	}
	// The base PATH the process (and thus buildArmSpec) sees is exactly that dir.
	t.Setenv("PATH", shared)

	d, err := NewDriver(Config{
		Trials:         1,
		Advertise:      AdvertiseMCP,
		DonmaiBin:      filepath.Join(shared, "donmai"),
		RepoRoots:      map[string]string{"test/repo": t.TempDir()},
		WorkareaParent: t.TempDir(),
		Executor:       &recordingExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}

	c := Case{ID: "codeintel-find-symbol-test-001", Input: CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: "deadbeef", Prompt: "Where is Target defined?"}}
	wa := t.TempDir()

	withoutSpec, wocleanup, err := d.buildArmSpec(context.Background(), c, ArmWithout, wa, "sess-wo")
	if err != nil {
		t.Fatalf("buildArmSpec WITHOUT: %v", err)
	}
	if wocleanup != nil {
		defer wocleanup()
	}
	withSpec, wcleanup, err := d.buildArmSpec(context.Background(), c, ArmWith, wa, "sess-w")
	if err != nil {
		t.Fatalf("buildArmSpec WITH: %v", err)
	}
	if wcleanup != nil {
		defer wcleanup()
	}

	woPath := envPath(withoutSpec.Env)
	wPath := envPath(withSpec.Env)

	// donmai: reachable in WITH, NOT in WITHOUT.
	if _, ok := BinaryOnPath("donmai", wPath); !ok {
		t.Error("WITH arm must resolve donmai")
	}
	if _, ok := BinaryOnPath("donmai", woPath); ok {
		t.Error("WITHOUT arm must NOT resolve donmai (control contamination)")
	}

	// Every baseline (non-donmai) tool must resolve IDENTICALLY in both arms.
	for _, tool := range []string{"rg", "gh", "git"} {
		_, wOK := BinaryOnPath(tool, wPath)
		_, woOK := BinaryOnPath(tool, woPath)
		if !woOK {
			t.Errorf("WITHOUT arm lost baseline tool %q — asymmetric scrub inflates the A/B delta", tool)
		}
		if !wOK {
			t.Errorf("WITH arm unexpectedly lost baseline tool %q", tool)
		}
		if wOK != woOK {
			t.Errorf("baseline tool %q asymmetric between arms: with=%v without=%v", tool, wOK, woOK)
		}
	}
}

// TestDriver_Refactor_NilJudge_NeverPasses proves a refactor task with no judge
// configured never counts as a pass (an empty/wrong answer cannot sneak through).
func TestDriver_Refactor_NilJudge_NeverPasses(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	c := Case{
		ID:     "codeintel-refactor-test-001",
		Input:  CaseInput{TaskType: TaskRefactorAcrossFiles, Repo: "test/repo", Ref: sha, Prompt: "rename Target to Goal"},
		Rubric: "Score 1.0 only if all sites renamed.",
		Tags:   []string{tagSuite, "refactor-across-files", tagVersion},
	}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: filepath.Join(donmaiDir, "donmai"),
		RepoRoots: map[string]string{"test/repo": repoDir}, WorkareaParent: t.TempDir(),
		Executor: &recordingExecutor{}, Judge: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := d.Run(context.Background(), []Case{c})
	if err != nil {
		t.Fatal(err)
	}
	fam := rep.Families[TaskRefactorAcrossFiles]
	if fam.WithPasses != 0 || fam.WithoutPasses != 0 {
		t.Errorf("refactor with nil judge must never pass; got with=%d without=%d", fam.WithPasses, fam.WithoutPasses)
	}
}
