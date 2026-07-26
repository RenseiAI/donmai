package codeintel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/eval/experiment"
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

type promptRecordingExecutor struct {
	specs            []ArmSpec
	donmaiResolvable []bool
	returnArm        Arm
}

func (e *promptRecordingExecutor) Name() string { return "prompt-recording" }

func (e *promptRecordingExecutor) SupportsPromptExperiments() bool { return true }

func (e *promptRecordingExecutor) Execute(_ context.Context, spec ArmSpec) (Transcript, error) {
	if _, err := os.Stat(filepath.Join(spec.Workarea, "foo.go")); err != nil {
		return Transcript{}, fmt.Errorf("pinned workarea missing foo.go: %w", err)
	}
	e.specs = append(e.specs, spec)
	_, resolvable := BinaryOnPath("donmai", envPath(spec.Env))
	e.donmaiResolvable = append(e.donmaiResolvable, resolvable)
	arm := spec.Arm
	if e.returnArm != "" {
		arm = e.returnArm
	}
	return Transcript{
		Arm: arm, FinalAnswer: "foo.go:3", TurnCount: 1,
		TokenCounts: TokenCounts{Input: 10, Output: 2}, AdvertisedTools: spec.AdvertisedTools,
		SnapshotRef: &SnapshotRef{Provider: "local", SnapshotID: spec.SnapshotID, Retain: RetainEvalPermanent},
	}, nil
}

func TestDriver_PromptExperimentReusesPinnedExecutionLifecycle(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	c := Case{
		ID:             "prompt-experiment-case-001",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Complete the same benign task."},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}
	exec := &promptRecordingExecutor{}
	d, err := NewDriver(Config{
		Trials: 2, DonmaiBin: filepath.Join(donmaiDir, "donmai"),
		RepoRoots: map[string]string{"test/repo": repoDir}, WorkareaParent: t.TempDir(),
		Executor: exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "injection-clause-v1",
		Arms: []experiment.Arm{
			{ID: "without", SubjectRef: "agent/development", VariantRef: experiment.SHA256VariantRef("incumbent clause"), SystemPrompt: "incumbent clause"},
			{ID: "candidate", SubjectRef: "agent/development", VariantRef: experiment.SHA256VariantRef("candidate clause"), SystemPrompt: "candidate clause"},
		},
	}
	report, err := d.RunPromptExperiment(context.Background(), []Case{c}, definition, []string{"safety/injection-follow-v1"})
	if err != nil {
		t.Fatalf("RunPromptExperiment: %v", err)
	}
	if len(report.Outcomes) != 4 || len(exec.specs) != 4 {
		t.Fatalf("balanced 2 arms x 2 trials = 4 outcomes/specs, got %d/%d", len(report.Outcomes), len(exec.specs))
	}
	for i, spec := range exec.specs {
		if !spec.UseCodeIntel {
			t.Errorf("prompt arm %s did not receive the shared capability profile", spec.Arm)
		}
		if spec.Prompt != c.Input.Prompt {
			t.Errorf("arm %s prompt = %q, want shared task %q", spec.Arm, spec.Prompt, c.Input.Prompt)
		}
		if !exec.donmaiResolvable[i] {
			t.Errorf("prompt arm %s must receive the same treatment tool environment", spec.Arm)
		}
		if spec.MCPConfigPath == "" {
			t.Errorf("prompt arm %s must receive an authored MCP config", spec.Arm)
		}
		if spec.PromptSuffix != "" {
			t.Errorf("prompt arm %s received unbound shared system text %q", spec.Arm, spec.PromptSuffix)
		}
		switch spec.Arm {
		case "without":
			if spec.VariantSystemPrompt != "incumbent clause" {
				t.Errorf("incumbent variant = %q", spec.VariantSystemPrompt)
			}
		case "candidate":
			if spec.VariantSystemPrompt != "candidate clause" {
				t.Errorf("candidate variant = %q", spec.VariantSystemPrompt)
			}
		default:
			t.Errorf("unexpected arm %q", spec.Arm)
		}
	}
}

func TestDriver_PromptExperimentRequiresExplicitGraders(t *testing.T) {
	d, err := NewDriver(Config{DonmaiBin: "/bin/true", RepoRoots: map[string]string{"test/repo": t.TempDir()}, Executor: &promptRecordingExecutor{}})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("")},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("")},
		},
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{{ID: "case", Input: CaseInput{Prompt: "task"}}}, definition, nil)
	if err == nil || !strings.Contains(err.Error(), "explicit grader") {
		t.Fatalf("error = %v, want explicit grader failure", err)
	}
}

func TestDriver_PromptExperimentValidatesGradersBeforeExecution(t *testing.T) {
	exec := &promptRecordingExecutor{}
	d, err := NewDriver(Config{DonmaiBin: "/bin/true", RepoRoots: map[string]string{"test/repo": t.TempDir()}, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("")},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("")},
		},
	}
	for _, graders := range [][]string{{"not-a-path"}, {"safety/injection-follow-v1", "safety/injection-follow-v1"}} {
		_, err := d.RunPromptExperiment(context.Background(), []Case{{ID: "case", Input: CaseInput{Prompt: "task"}}}, definition, graders)
		if err == nil {
			t.Fatalf("graders %v unexpectedly validated", graders)
		}
	}
	if len(exec.specs) != 0 {
		t.Fatalf("executor ran before grader validation: %d specs", len(exec.specs))
	}
}

func TestDriver_PromptExperimentRequiresCapableExecutor(t *testing.T) {
	d, err := NewDriver(Config{DonmaiBin: "/bin/true", RepoRoots: map[string]string{"test/repo": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("incumbent"), SystemPrompt: "incumbent"},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("candidate"), SystemPrompt: "candidate"},
		},
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{{ID: "case", Input: CaseInput{Prompt: "task"}}}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "does not support prompt experiments") {
		t.Fatalf("error = %v, want executor capability failure", err)
	}
}

func TestDriver_PromptExperimentRequiresMCPAdvertisement(t *testing.T) {
	d, err := NewDriver(Config{
		Advertise: AdvertisePromptHelp, DonmaiBin: "/bin/true",
		RepoRoots: map[string]string{"test/repo": t.TempDir()}, Executor: &promptRecordingExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("incumbent"), SystemPrompt: "incumbent"},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("candidate"), SystemPrompt: "candidate"},
		},
	}
	_, err = d.RunPromptExperiment(context.Background(), []Case{{ID: "case", Input: CaseInput{Prompt: "task"}}}, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "require MCP advertisement") {
		t.Fatalf("error = %v, want MCP advertisement failure", err)
	}
}

func TestDriver_PromptExperimentRejectsTranscriptArmMismatch(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	exec := &promptRecordingExecutor{returnArm: ArmWith}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: filepath.Join(donmaiDir, "donmai"),
		RepoRoots: map[string]string{"test/repo": repoDir}, WorkareaParent: t.TempDir(), Executor: exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("incumbent"), SystemPrompt: "incumbent"},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("candidate"), SystemPrompt: "candidate"},
		},
	}
	cases := []Case{{
		ID: "case", Input: CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Find Target."},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`), Tags: []string{tagSuite, "find-symbol", tagVersion},
	}}
	_, err = d.RunPromptExperiment(context.Background(), cases, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "does not match planned arm") {
		t.Fatalf("error = %v, want transcript attribution failure", err)
	}
}

func TestDriver_PromptExperimentPropagatesBridgeFailure(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>wrong route</html>"))
	}))
	defer server.Close()
	exec := &promptRecordingExecutor{}
	d, err := NewDriver(Config{
		Trials: 1, DonmaiBin: filepath.Join(donmaiDir, "donmai"),
		RepoRoots: map[string]string{"test/repo": repoDir}, WorkareaParent: t.TempDir(),
		Executor: exec, Bridge: NewBridge(server.URL, "", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := experiment.Definition{
		ID: "prompt-v1",
		Arms: []experiment.Arm{
			{ID: "incumbent", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("incumbent"), SystemPrompt: "incumbent"},
			{ID: "candidate", SubjectRef: "agent/base", VariantRef: experiment.SHA256VariantRef("candidate"), SystemPrompt: "candidate"},
		},
	}
	cases := []Case{{
		ID: "case", Input: CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Find Target."},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`), Tags: []string{tagSuite, "find-symbol", tagVersion},
	}}
	_, err = d.RunPromptExperiment(context.Background(), cases, definition, []string{"safety/injection-follow-v1"})
	if err == nil || !strings.Contains(err.Error(), "bridge post") {
		t.Fatalf("error = %v, want fatal bridge failure", err)
	}
	if len(exec.specs) != 1 {
		t.Fatalf("matrix continued after first failed receipt: %d specs", len(exec.specs))
	}
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

// TestDriver_ControlArmMaxTurns_RecordedNotAborted is the driver-seam regression
// for the W6 review blocker: a live-executor arm that ends in error_max_turns
// (empty result, clean exit — the disproportionately-control-arm outcome) must be
// RECORDED as a failed trial, not abort the whole A/B matrix and discard the
// report. It drives the real ClaudeExecutor with a fake spawner emitting the
// error_max_turns fixture on every arm and asserts Run completes with both arms
// recorded pass=false.
func TestDriver_ControlArmMaxTurns_RecordedNotAborted(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")

	c := Case{
		ID:             "codeintel-find-symbol-maxturns-001",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Where is the function Target defined?"},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}

	exec := newClaudeExecutorWithSpawner((&fakeSpawn{stream: readFixture(t, "claude_error_max_turns.jsonl")}).spawn)
	d, err := NewDriver(Config{
		Trials:         1,
		Advertise:      AdvertiseMCP,
		DonmaiBin:      filepath.Join(donmaiDir, "donmai"),
		RepoRoots:      map[string]string{"test/repo": repoDir},
		WorkareaParent: t.TempDir(),
		Executor:       exec,
	})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := d.Run(context.Background(), []Case{c})
	if err != nil {
		t.Fatalf("Run must NOT abort on a control-arm error_max_turns: %v", err)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("both arms must be recorded, got %d", len(rep.Records))
	}
	for _, r := range rep.Records {
		if r.Pass {
			t.Errorf("arm %s: an empty error_max_turns answer must grade pass=false", r.Arm)
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

// TestDriver_UnderpoweredRunIsNotAGAPass proves the efficiency-threshold
// verdict hard-refuses a PASS on an underpowered corpus. One find-symbol case,
// one trial, one repo: WITH passes / WITHOUT fails, WITH cheaper on tokens —
// the raw numbers would clear the Q1v2 efficiency bar — yet this is one task,
// one trial, one repo, one family, far below the locked power preconditions
// (>=8 tasks/family x 2 repos x >=3 trials/arm). Medians over one trial are
// noise masquerading as a verdict; MeetsThreshold must be false.
func TestDriver_UnderpoweredRunIsNotAGAPass(t *testing.T) {
	repoDir, sha := initTempRepo(t)
	donmaiDir := writeFakeBinary(t, "donmai")
	c := Case{
		ID:             "codeintel-find-symbol-test-001",
		Input:          CaseInput{TaskType: TaskFindSymbol, Repo: "test/repo", Ref: sha, Prompt: "Where is the function Target defined?"},
		ExpectedOutput: json.RawMessage(`{"file":"foo.go","lineRange":[1,10]}`),
		Tags:           []string{tagSuite, "find-symbol", tagVersion},
	}
	d, err := NewDriver(Config{
		Trials: 1, Advertise: AdvertiseMCP, DonmaiBin: filepath.Join(donmaiDir, "donmai"),
		RepoRoots: map[string]string{"test/repo": repoDir}, WorkareaParent: t.TempDir(),
		Executor: &recordingExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := d.Run(context.Background(), []Case{c})
	if err != nil {
		t.Fatal(err)
	}
	if r := rep.Aggregate.TokenRatio; r <= 0 || r > 1.0 {
		t.Fatalf("precondition: raw tokenRatio should clear the efficiency bar (<=1.0x), got %.2f", r)
	}
	if len(rep.Aggregate.RegressedFamilies) != 0 {
		t.Fatalf("precondition: no family should regress, got %v", rep.Aggregate.RegressedFamilies)
	}
	if rep.Aggregate.MeetsThreshold {
		t.Error("underpowered run (1 task / 1 trial / 1 repo / 1 family) must NOT meet the efficiency threshold")
	}
	if !rep.Aggregate.Underpowered {
		t.Error("underpowered run must be flagged Underpowered")
	}
	if !strings.Contains(rep.Aggregate.Status, "UNDERPOWERED") {
		t.Errorf("status must call out UNDERPOWERED, got %q", rep.Aggregate.Status)
	}
	if !strings.Contains(rep.Summary(), "UNDERPOWERED") {
		t.Error("summary of an underpowered run must call out UNDERPOWERED")
	}
}

// TestComputeAggregate_PoweredCorpusCanPass proves the power gate is not a
// blanket refusal: a corpus meeting every locked precondition (>=8 tasks/family
// across all four families, >=2 repos, >=3 trials/arm) that clears the Q1v2
// efficiency bar (aggregate tokens <=1.0x, every family <=1.10x, no success
// regression) reports MeetsThreshold=true. The +100pp delta here is
// informational — the verdict rides on the token ratios.
func TestComputeAggregate_PoweredCorpusCanPass(t *testing.T) {
	repos := []string{"acme/webapp", "acme/service"}
	fams := map[TaskType]*FamilyStat{}
	var cases []Case
	var records []RunRecord
	for _, fam := range families {
		f := &FamilyStat{}
		for i := 0; i < minTasksPerFamily; i++ {
			id := fmt.Sprintf("%s-%02d", fam, i)
			repo := repos[i%len(repos)]
			// minTrialsForGA trials/arm/case; WITH passes all, WITHOUT fails all.
			for tr := 0; tr < minTrialsForGA; tr++ {
				f.WithTrials++
				f.WithPasses++
				f.WithoutTrials++
				f.WithTokens = append(f.WithTokens, 100)
				f.WithoutTokens = append(f.WithoutTokens, 110)
				records = append(
					records,
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWith, Pass: true},
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWithout, Pass: false},
				)
			}
			cases = append(cases, Case{ID: id, Input: CaseInput{TaskType: fam, Repo: repo}})
		}
		fams[fam] = f
	}

	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("powered corpus must not be underpowered; shortfalls=%v", agg.PowerShortfalls)
	}
	if agg.DeltaPP < 15 {
		t.Fatalf("precondition: delta should be +100pp, got %.0f", agg.DeltaPP)
	}
	// Every case is a clean +100pp win, so the bootstrap CI is a point mass at
	// 100 — still computed and reported (informational since Q1v2).
	if agg.DeltaCILow < 15 {
		t.Fatalf("precondition: a uniform +100pp corpus must have a tight CI lower bound, got %.1f", agg.DeltaCILow)
	}
	if !agg.MeetsThreshold {
		t.Errorf("powered corpus (tokens 0.91x, no regression) must meet the efficiency threshold; status=%q", agg.Status)
	}
	if !strings.Contains(agg.Status, "GA-PASS") {
		t.Errorf("status should be GA-PASS, got %q", agg.Status)
	}
}

// efficiencyCorpus builds a fully-POWERED corpus (minTasksPerFamily tasks per
// family x all four families x 2 repos x minTrialsForGA trials/arm) where BOTH
// arms pass every trial — DeltaPP is exactly 0, which the retired success bar
// would have refused — and each family's per-trial token totals are fixed at
// tok[fam] = {WITH, WITHOUT}. A family mapped to {0, 0} contributes trials but
// NO token samples (the missing-token-data shape a live run produces when an
// arm's usage capture fails).
func efficiencyCorpus(tok map[TaskType][2]int64) (map[TaskType]*FamilyStat, []RunRecord, []Case) {
	repos := []string{"acme/webapp", "acme/service"}
	fams := map[TaskType]*FamilyStat{}
	var cases []Case
	var records []RunRecord
	for _, fam := range families {
		f := &FamilyStat{}
		fams[fam] = f
		for i := 0; i < minTasksPerFamily; i++ {
			id := fmt.Sprintf("%s-%02d", fam, i)
			repo := repos[i%len(repos)]
			cases = append(cases, Case{ID: id, Input: CaseInput{TaskType: fam, Repo: repo}})
			for tr := 0; tr < minTrialsForGA; tr++ {
				f.WithTrials++
				f.WithPasses++
				f.WithoutTrials++
				f.WithoutPasses++
				if t := tok[fam]; t[0] > 0 || t[1] > 0 {
					f.WithTokens = append(f.WithTokens, t[0])
					f.WithoutTokens = append(f.WithoutTokens, t[1])
				}
				records = append(
					records,
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWith, Pass: true},
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWithout, Pass: true},
				)
			}
		}
	}
	return fams, records, cases
}

// TestComputeAggregate_EfficiencyBar_ZeroDeltaTokenWinPasses is the Q1v2
// red-first core case: a powered run with DeltaPP = 0 (both arms always pass —
// exactly what the 2026-07-06 decision-gate eval showed: control = 100% even on
// grep-resistant probes) but a clean token win (0.95x aggregate, every family
// <=1.10x, no regression) MEETS the threshold. Under the retired >=+15pp bar
// this exact corpus was an automatic FAIL.
func TestComputeAggregate_EfficiencyBar_ZeroDeltaTokenWinPasses(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{
		TaskFindSymbol:          {95, 100},
		TaskLocateUsage:         {95, 100},
		TaskRefactorAcrossFiles: {95, 100},
		TaskDedup:               {95, 100},
	})
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("corpus should be powered; shortfalls=%v", agg.PowerShortfalls)
	}
	if agg.DeltaPP != 0 {
		t.Fatalf("precondition: delta must be exactly 0pp, got %.2f", agg.DeltaPP)
	}
	if math.Abs(agg.TokenRatio-0.95) > 1e-9 {
		t.Fatalf("precondition: aggregate tokenRatio must be 0.95, got %.4f", agg.TokenRatio)
	}
	if !agg.MeetsThreshold {
		t.Errorf("powered 0.95x-token run with zero delta must MEET the efficiency threshold; status=%q", agg.Status)
	}
	if !strings.Contains(agg.Status, "GA-PASS") {
		t.Errorf("status should be GA-PASS, got %q", agg.Status)
	}
}

// TestComputeAggregate_EfficiencyBar_FamilyTokenRegressionFails: one family at
// 1.50x (the pre-WS5 dedup shape) fails the per-family <=1.10x guard even
// though the AGGREGATE median is well under 1.0x — a family-level cost
// regression cannot hide inside a favorable pooled median.
func TestComputeAggregate_EfficiencyBar_FamilyTokenRegressionFails(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{
		TaskFindSymbol:          {80, 100},
		TaskLocateUsage:         {80, 100},
		TaskRefactorAcrossFiles: {80, 100},
		TaskDedup:               {150, 100}, // 1.50x — the dedup cost shape WS5 exists to fix
	})
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("corpus should be powered; shortfalls=%v", agg.PowerShortfalls)
	}
	if agg.TokenRatio > 1.0 {
		t.Fatalf("precondition: aggregate ratio must stay <=1.0x (pooled median hides the dedup cost), got %.2f", agg.TokenRatio)
	}
	if agg.MeetsThreshold {
		t.Errorf("a 1.50x family must fail the per-family <=1.10x guard; status=%q", agg.Status)
	}
	if len(agg.TokenRegressedFamilies) != 1 || agg.TokenRegressedFamilies[0] != TaskDedup {
		t.Errorf("TokenRegressedFamilies = %v, want [dedup]", agg.TokenRegressedFamilies)
	}
	if !strings.Contains(agg.Status, "GA-FAIL") {
		t.Errorf("status should be GA-FAIL, got %q", agg.Status)
	}
}

// TestComputeAggregate_EfficiencyBar_AggregateRatioFails: every family inside
// the +10% per-family allowance (1.05x) but the AGGREGATE over 1.0x fails —
// the overall surface must be net-non-costing, not merely never-terrible.
func TestComputeAggregate_EfficiencyBar_AggregateRatioFails(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{
		TaskFindSymbol:          {105, 100},
		TaskLocateUsage:         {105, 100},
		TaskRefactorAcrossFiles: {105, 100},
		TaskDedup:               {105, 100},
	})
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("corpus should be powered; shortfalls=%v", agg.PowerShortfalls)
	}
	if len(agg.TokenRegressedFamilies) != 0 {
		t.Fatalf("precondition: no family exceeds 1.10x, got %v", agg.TokenRegressedFamilies)
	}
	if agg.MeetsThreshold {
		t.Errorf("aggregate 1.05x > 1.0x must fail the efficiency threshold; status=%q", agg.Status)
	}
	if !strings.Contains(agg.Status, "GA-FAIL") {
		t.Errorf("status should be GA-FAIL, got %q", agg.Status)
	}
}

// TestComputeAggregate_EfficiencyBar_FamilyWithoutTokenDataUnderpowered: a
// family whose trials carry NO token samples (usage capture failed on an arm)
// is still EXCLUDED from the per-family ratio guard (no pass/fail on a
// garbage 0-median), but the missing coverage is a POWER shortfall: the
// efficiency bar is a claim about token cost, and a family with no usable
// token median on either arm cannot back that claim — the run must land
// UNDERPOWERED, never PASS on just the families that have data.
func TestComputeAggregate_EfficiencyBar_FamilyWithoutTokenDataUnderpowered(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{
		TaskFindSymbol:          {95, 100},
		TaskLocateUsage:         {95, 100},
		TaskRefactorAcrossFiles: {95, 100},
		TaskDedup:               {0, 0}, // no token samples on either arm
	})
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if len(agg.TokenRegressedFamilies) != 0 {
		t.Errorf("token-data-less family must be excluded from the ratio guard, got %v", agg.TokenRegressedFamilies)
	}
	if agg.MeetsThreshold {
		t.Errorf("a family with no token data must force UNDERPOWERED, not pass; status=%q", agg.Status)
	}
	if !agg.Underpowered {
		t.Error("missing per-family token coverage must flag the run Underpowered")
	}
	found := false
	for _, s := range agg.PowerShortfalls {
		if strings.Contains(s, string(TaskDedup)) && strings.Contains(s, "token") {
			found = true
		}
	}
	if !found {
		t.Errorf("PowerShortfalls must name the token-coverage gap for %s; got %v", TaskDedup, agg.PowerShortfalls)
	}
	if !strings.Contains(agg.Status, "UNDERPOWERED") {
		t.Errorf("status must call out UNDERPOWERED, got %q", agg.Status)
	}
}

// TestComputeAggregate_EfficiencyBar_SuccessRegressionFails pins the third
// leg of the Q1v2 bar: a powered corpus with every token ratio passing but
// ONE family whose WITH success rate falls below WITHOUT must fail — cheaper
// is not allowed to mean worse.
func TestComputeAggregate_EfficiencyBar_SuccessRegressionFails(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{
		TaskFindSymbol:          {95, 100},
		TaskLocateUsage:         {95, 100},
		TaskRefactorAcrossFiles: {95, 100},
		TaskDedup:               {95, 100},
	})
	// Regress the WITH arm of one family: WithRate drops below WithoutRate.
	fams[TaskLocateUsage].WithPasses -= 5
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("corpus should be powered; shortfalls=%v", agg.PowerShortfalls)
	}
	if len(agg.TokenRegressedFamilies) != 0 {
		t.Fatalf("precondition: every token ratio passes, got %v", agg.TokenRegressedFamilies)
	}
	if agg.MeetsThreshold {
		t.Errorf("a per-family success regression must fail the efficiency bar; status=%q", agg.Status)
	}
	if len(agg.RegressedFamilies) != 1 || agg.RegressedFamilies[0] != TaskLocateUsage {
		t.Errorf("RegressedFamilies = %v, want [%s]", agg.RegressedFamilies, TaskLocateUsage)
	}
	if !strings.Contains(agg.Status, "success regression") {
		t.Errorf("status must call out the success regression, got %q", agg.Status)
	}
	if !strings.Contains(agg.Status, "GA-FAIL") {
		t.Errorf("status should be GA-FAIL, got %q", agg.Status)
	}
}

// TestComputeAggregate_EfficiencyBar_NoTokenDataFails: a powered corpus where
// NO family produced token samples (every arm's usage capture failed) must
// never PASS a bar that is entirely about token cost. With the token-coverage
// power precondition this lands UNDERPOWERED; the aggregate tokenRatio>0
// guard inside the bar remains as defense-in-depth.
func TestComputeAggregate_EfficiencyBar_NoTokenDataFails(t *testing.T) {
	fams, records, cases := efficiencyCorpus(map[TaskType][2]int64{}) // zero-value {0,0} for every family
	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.TokenRatio != 0 {
		t.Fatalf("precondition: aggregate tokenRatio must be 0 with no token data, got %.2f", agg.TokenRatio)
	}
	if agg.MeetsThreshold {
		t.Errorf("a run with NO token data must not pass the efficiency bar; status=%q", agg.Status)
	}
	if !agg.Underpowered {
		t.Error("a run with no token data on any family must be Underpowered (token-coverage precondition)")
	}
	if !strings.Contains(agg.Status, "UNDERPOWERED") {
		t.Errorf("status must call out UNDERPOWERED, got %q", agg.Status)
	}
}

// TestAccumulate_ZeroTokenTrialNotASample pins the U2 capture-failure rule: a
// completed trial whose usage capture failed (TokenCounts.Total()==0) counts
// for success/pass bookkeeping but contributes NO token sample — a fake
// zero-cost sample would bias the aggregate median toward PASS.
func TestAccumulate_ZeroTokenTrialNotASample(t *testing.T) {
	f := &FamilyStat{}
	accumulate(f, RunRecord{Arm: ArmWith, Pass: true})     // zero-value TokenCounts
	accumulate(f, RunRecord{Arm: ArmWithout, Pass: false}) // zero-value TokenCounts
	if f.WithTrials != 1 || f.WithPasses != 1 || f.WithoutTrials != 1 || f.WithoutPasses != 0 {
		t.Errorf("zero-token trials must still count trials/passes: %+v", f)
	}
	if len(f.WithTokens) != 0 || len(f.WithoutTokens) != 0 {
		t.Errorf("zero-token trials must contribute no token samples: with=%v without=%v", f.WithTokens, f.WithoutTokens)
	}

	// Real usage still lands as a sample.
	rec := RunRecord{Arm: ArmWith, Pass: true}
	rec.Envelope.Trace.TokenCounts = TokenCounts{Input: 100, Output: 20}
	accumulate(f, rec)
	if len(f.WithTokens) != 1 || f.WithTokens[0] != 120 {
		t.Errorf("nonzero-token trial must contribute its total: %v", f.WithTokens)
	}
}

// TestComputeAggregate_DeltaCIReportedButNoLongerGates re-aims the old
// finding-7 lower-bound guard at the Q1v2 bar: a fully-POWERED corpus whose
// per-case outcomes are heterogeneous — most cases a tie, a handful big WITH
// wins — has a WIDE 95% delta CI whose lower bound falls below +15pp. Under
// the retired success-delta bar that CI REFUSED the run; under the efficiency
// bar the delta (and its CI) stay computed and reported as INFORMATIONAL, and
// the same corpus PASSES on its token win (0.91x, no regression).
func TestComputeAggregate_DeltaCIReportedButNoLongerGates(t *testing.T) {
	repos := []string{"acme/webapp", "acme/service"}
	fams := map[TaskType]*FamilyStat{}
	var cases []Case
	var records []RunRecord

	// 6 "win" cases (WITH beats WITHOUT +100pp) out of 32; the other 26 are ties
	// (both arms pass). Pooled point estimate ≈ +18.75pp, but the case-level
	// variance is high → the bootstrap 95% lower bound sits well below +15pp.
	winsRemaining := 6
	caseIdx := 0
	for _, fam := range families {
		f := &FamilyStat{}
		fams[fam] = f
		for i := 0; i < minTasksPerFamily; i++ {
			id := fmt.Sprintf("%s-%02d", fam, i)
			repo := repos[caseIdx%len(repos)]
			caseIdx++
			cases = append(cases, Case{ID: id, Input: CaseInput{TaskType: fam, Repo: repo}})
			isWin := winsRemaining > 0
			if isWin {
				winsRemaining--
			}
			for tr := 0; tr < minTrialsForGA; tr++ {
				// WITH always passes; WITHOUT passes on ties, fails on wins.
				f.WithTrials++
				f.WithPasses++
				f.WithTokens = append(f.WithTokens, 100)
				f.WithoutTrials++
				f.WithoutTokens = append(f.WithoutTokens, 110)
				woPass := !isWin
				if woPass {
					f.WithoutPasses++
				}
				records = append(
					records,
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWith, Pass: true},
					RunRecord{CaseID: id, Family: fam, Repo: repo, Arm: ArmWithout, Pass: woPass},
				)
			}
		}
	}

	agg := computeAggregate(fams, records, cases, minTrialsForGA)
	if agg.Underpowered {
		t.Fatalf("corpus should be powered; shortfalls=%v", agg.PowerShortfalls)
	}
	if len(agg.RegressedFamilies) != 0 {
		t.Fatalf("no family should regress (WITH >= WITHOUT everywhere); got %v", agg.RegressedFamilies)
	}
	if agg.DeltaPP < 15 {
		t.Fatalf("precondition: point-estimate delta must clear +15pp, got %.2f", agg.DeltaPP)
	}
	// The CI machinery stays intact and reported: a wide-variance corpus still
	// yields a lower bound below the old +15pp mark, and stddev is nonzero.
	if agg.DeltaCILow >= 15 {
		t.Fatalf("precondition: the 95%% CI lower bound must fall BELOW +15pp for this wide-variance corpus, got %.2f (hi=%.2f, stddev=%.2f)",
			agg.DeltaCILow, agg.DeltaCIHigh, agg.DeltaStdDev)
	}
	if agg.DeltaStdDev <= 0 {
		t.Fatalf("bootstrap stddev must still be computed, got %.4f", agg.DeltaStdDev)
	}
	if !agg.MeetsThreshold {
		t.Errorf("under the Q1v2 efficiency bar the wide delta CI (lower bound %.2f) is informational — the 0.91x token win must PASS; status=%q",
			agg.DeltaCILow, agg.Status)
	}
}

// TestAccumulate_CorrectSkipNotCountedAsAdoption keeps the adoption-rate
// metric honest under the F3 correct-skip policy: a WITH-arm trial graded as a
// correct skip (Pass true, adopted=false) must NOT bump FamilyStat.Adoption,
// while a genuinely adopted trial still does.
func TestAccumulate_CorrectSkipNotCountedAsAdoption(t *testing.T) {
	f := &FamilyStat{}

	correctSkip := RunRecord{
		Arm: ArmWith, Pass: true,
		Grades: []GradeResult{{
			GraderID: GraderToolUse, Score: 1, Pass: true,
			Metadata: map[string]any{"applicable": true, "adopted": false, "correctSkip": true},
		}},
	}
	accumulate(f, correctSkip)
	if f.Adoption != 0 {
		t.Errorf("correct skip counted as adoption: Adoption = %d, want 0", f.Adoption)
	}
	if f.WithTrials != 1 || f.WithPasses != 1 {
		t.Errorf("correct skip must still count the trial/pass: %+v", f)
	}

	adopted := RunRecord{
		Arm: ArmWith, Pass: true,
		Grades: []GradeResult{{
			GraderID: GraderToolUse, Score: 1, Pass: true,
			Metadata: map[string]any{"applicable": true, "adopted": true, "correctTool": true},
		}},
	}
	accumulate(f, adopted)
	if f.Adoption != 1 {
		t.Errorf("real adoption must count: Adoption = %d, want 1", f.Adoption)
	}
}

// TestBuildArmSpec_FamilyToolSubsetPlumbing pins the WS2 family→surface
// plumbing end to end through buildArmSpec: the refactor family's WITH arm
// carries find_type_usages (5 advertised tools) and the MCP server's --tools
// allow-list names exactly the same subset; the find-symbol family carries the
// core four without it.
func TestBuildArmSpec_FamilyToolSubsetPlumbing(t *testing.T) {
	donmaiDir := writeFakeBinary(t, "donmai")
	d, err := NewDriver(Config{
		Trials:         1,
		Advertise:      AdvertiseMCP,
		DonmaiBin:      filepath.Join(donmaiDir, "donmai"),
		WorkareaParent: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	build := func(family TaskType) (ArmSpec, func()) {
		t.Helper()
		c := Case{
			ID:    "sub-" + string(family),
			Input: CaseInput{TaskType: family, Repo: "r", Ref: "s", Prompt: "p"},
		}
		spec, cleanup, err := d.buildArmSpec(context.Background(), c, ArmWith, t.TempDir(), "sess")
		if err != nil {
			t.Fatalf("buildArmSpec(%s): %v", family, err)
		}
		return spec, cleanup
	}

	toolsCSV := func(spec ArmSpec) string {
		t.Helper()
		if len(spec.MCPServers) != 1 {
			t.Fatalf("want one MCP server, got %d", len(spec.MCPServers))
		}
		args := spec.MCPServers[0].Args
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--tools" {
				return args[i+1]
			}
		}
		t.Fatalf("no --tools flag in server args %v", args)
		return ""
	}

	// Refactor family: 5 advertised tools including find_type_usages, and the
	// server --tools list matches the advertised set 1:1 (FQ prefix aside).
	rfSpec, rfCleanup := build(TaskRefactorAcrossFiles)
	defer rfCleanup()
	if len(rfSpec.AdvertisedTools) != 5 {
		t.Fatalf("refactor AdvertisedTools = %v, want 5 entries", rfSpec.AdvertisedTools)
	}
	hasXref := false
	for _, n := range rfSpec.AdvertisedTools {
		if n == fqName("af_code_find_type_usages") {
			hasXref = true
		}
	}
	if !hasXref {
		t.Errorf("refactor arm must advertise af_code_find_type_usages; got %v", rfSpec.AdvertisedTools)
	}
	rfServer := strings.Split(toolsCSV(rfSpec), ",")
	if len(rfServer) != len(rfSpec.AdvertisedTools) {
		t.Fatalf("server --tools (%v) and AdvertisedTools (%v) diverge in size", rfServer, rfSpec.AdvertisedTools)
	}
	for i, unq := range rfServer {
		if fqName(unq) != rfSpec.AdvertisedTools[i] {
			t.Errorf("server tool %d = %q does not match advertised %q", i, unq, rfSpec.AdvertisedTools[i])
		}
	}

	// Find-symbol family: core four, no find_type_usages anywhere.
	fsSpec, fsCleanup := build(TaskFindSymbol)
	defer fsCleanup()
	if len(fsSpec.AdvertisedTools) != 4 {
		t.Fatalf("find-symbol AdvertisedTools = %v, want 4 entries", fsSpec.AdvertisedTools)
	}
	fsJoined := strings.Join(fsSpec.AdvertisedTools, " ") + " " + toolsCSV(fsSpec)
	if strings.Contains(fsJoined, "find_type_usages") {
		t.Errorf("find-symbol arm must not carry find_type_usages: %s", fsJoined)
	}
}
