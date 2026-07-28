package afcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	eval "github.com/RenseiAI/donmai/eval/codeintel"
	"github.com/RenseiAI/donmai/eval/experiment"
)

// writeMinimalBenchmarkDir writes a one-case benchmark JSONL dir that passes
// eval.LoadCasesDir validation, so the command reaches driver construction.
func writeMinimalBenchmarkDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	line := `{"id":"codeintel-find-symbol-x-001","input":{"taskType":"find-symbol","repo":"r","ref":"deadbeef","prompt":"Where is X defined?"},"expectedOutput":{"file":"a.go","lineRange":[1,2]},"tags":["codeintel-eval","find-symbol","v1"]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "find-symbol.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEvalCodeIntel_AdvertiseAllToolsPropagatesToDriverConfig proves the
// --advertise-all-tools flag reaches eval.Config.AdvertiseAllTools (and that
// the default leaves it false), via the evalNewDriver test seam — the same
// hook pattern the github/linear commands use.
func writePromptExperimentFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"incumbent.txt": "incumbent prompt\n",
		"candidate.txt": "candidate prompt\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := `{"id":"delegation-v1","arms":[{"id":"incumbent","subjectRef":"agent/development","systemPromptFile":"incumbent.txt"},{"id":"candidate","subjectRef":"agent/development","systemPromptFile":"candidate.txt"}],"graderIds":["behavior/delegation-fit-v1"]}`
	configPath := filepath.Join(dir, "experiment.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	casesDir := filepath.Join(dir, "cases")
	if err := os.Mkdir(casesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"id":"e2-simple-001","input":{"taskType":"development-simple","repo":"r","ref":"deadbeef","prompt":"Complete the fixture."},"expectedOutput":{"delegationProbe":{"requiredOutputIncludes":["fixture complete"],"delegationPolicy":"forbid","subagentToolNames":["Task"]}}}` + "\n"
	if err := os.WriteFile(filepath.Join(casesDir, "cases.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, casesDir
}

func TestEvalPromptExperimentCommandIsHidden(t *testing.T) {
	if cmd := newEvalPromptExperimentCmd(Config{}); !cmd.Hidden {
		t.Fatal("prompt-experiment operator command must remain hidden")
	}
}

func TestEvalPromptExperimentRequiresExplicitPaidRunScopeFlags(t *testing.T) {
	cmd := newEvalPromptExperimentCmd(Config{})
	trialStart := cmd.Flags().Lookup("trial-start")
	if trialStart == nil || trialStart.DefValue != "1" {
		t.Fatalf("--trial-start = %#v, want optional flag defaulting to 1", trialStart)
	}
	for _, name := range []string{"receipt-file", "case", "trials", "max-turns", "max-tokens"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("prompt-experiment must expose --%s", name)
		}
		if required := flag.Annotations[cobra.BashCompOneRequiredFlag]; len(required) == 0 || required[0] != "true" {
			t.Fatalf("--%s required annotation = %#v", name, required)
		}
	}
	if !strings.Contains(cmd.Long, "append") || !strings.Contains(cmd.Long, "fsync") {
		t.Fatalf("command help must explain append+fsync durability, got:\n%s", cmd.Long)
	}
}

func TestSelectPromptExperimentCaseRequiresExactSingleMatch(t *testing.T) {
	cases := []eval.Case{{ID: "case-1"}, {ID: "case-10"}}
	selected, err := selectPromptExperimentCase(cases, "case-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].ID != "case-1" {
		t.Fatalf("exact selection = %+v", selected)
	}
	if _, err := selectPromptExperimentCase(cases, "case"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("substring selection error = %v", err)
	}
	if _, err := selectPromptExperimentCase([]eval.Case{{ID: "duplicate"}, {ID: "duplicate"}}, "duplicate"); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("duplicate selection error = %v", err)
	}
}

func TestEvalPromptExperimentRejectsNonPositiveTrialStart(t *testing.T) {
	opts := &evalPromptExperimentOpts{
		configPath: "config.json", casesDir: "cases", caseFilter: "case-1", trials: 1, trialStart: 0,
		platformURL: "https://platform.invalid", platformTok: "test-token",
		orgID: "org_test", projectID: "proj_test", datasetID: "evd_test",
		receiptFile: "receipts.jsonl", maxTurns: 1, maxTokens: 1, timeout: time.Second,
	}
	if err := validatePromptExperimentOpts(opts); err == nil || !strings.Contains(err.Error(), "--trial-start must be positive") {
		t.Fatalf("error = %v, want positive trial-start error", err)
	}
}

func TestEvalPromptExperimentReceiptOpenFailureStopsBeforeDriver(t *testing.T) {
	called := false
	orig := evalNewDriver
	evalNewDriver = func(cfg eval.Config) (*eval.Driver, error) {
		called = true
		return orig(cfg)
	}
	defer func() { evalNewDriver = orig }()

	opts := &evalPromptExperimentOpts{
		configPath: "must-not-be-read.json", casesDir: "must-not-be-read", caseFilter: "case-1", trials: 1, trialStart: 1,
		platformURL: "https://platform.invalid", platformTok: "test-token",
		orgID: "org_test", projectID: "proj_test", datasetID: "evd_test",
		receiptFile: t.TempDir(), maxTurns: 1, maxTokens: 1, timeout: time.Second,
	}
	err := runEvalPromptExperiment(&cobra.Command{}, opts)
	if err == nil || !strings.Contains(err.Error(), "open prompt receipt journal") {
		t.Fatalf("error = %v, want fail-closed receipt open error", err)
	}
	if called {
		t.Fatal("driver was created after receipt journal open failure")
	}
}

func TestSummarizePromptExperimentIncludesVariantIdentityWithoutRawPrompt(t *testing.T) {
	const secret = "raw candidate prompt bytes must never be serialized"
	arm := experiment.Arm{
		ID: "candidate", SubjectRef: "agent/development",
		VariantRef: experiment.SHA256VariantRef(secret), SystemPrompt: secret,
	}
	report := experiment.Report[eval.RunRecord]{
		ExperimentID: "delegation-v1",
		TrialsPerArm: 1,
		Outcomes: []experiment.Outcome[eval.RunRecord]{
			{
				Trial: experiment.Trial{CaseID: "case-1", Arm: arm, TrialIndex: 1},
				Result: eval.RunRecord{
					PostedRunID: "run-1", CostUSD: 0.125, CostReported: true,
					Envelope: eval.ReportEnvelope{Trace: eval.TraceInsert{TurnCount: 3, TokenCounts: eval.TokenCounts{Input: 10, Output: 2}}},
				},
			},
		},
	}
	summary := summarizePromptExperiment(report)
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	encoded := string(body)
	if strings.Contains(encoded, secret) || strings.Contains(encoded, "SystemPrompt") || strings.Contains(encoded, "systemPrompt") {
		t.Fatalf("sanitized summary leaked raw prompt bytes: %s", encoded)
	}
	if !strings.Contains(encoded, arm.SubjectRef) || !strings.Contains(encoded, arm.VariantRef) {
		t.Fatalf("sanitized summary omitted safe variant identity: %s", encoded)
	}
	if summary.ExecutionCount != 1 || summary.TotalCostUSD != 0.125 ||
		summary.Outcomes[0].ExperimentID != report.ExperimentID || summary.Outcomes[0].PostedRunID != "run-1" {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestEvalPromptExperimentBuildsLiveReceiptDriverConfig(t *testing.T) {
	configPath, casesDir := writePromptExperimentFixture(t)
	receiptPath := filepath.Join(t.TempDir(), "receipts.jsonl")

	var captured eval.Config
	sentinel := errors.New("driver config captured")
	orig := evalNewDriver
	evalNewDriver = func(cfg eval.Config) (*eval.Driver, error) {
		captured = cfg
		return nil, sentinel
	}
	defer func() { evalNewDriver = orig }()

	root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newEvalCmd(Config{}))
	root.SetArgs([]string{
		"eval", "prompt-experiment",
		"--config", configPath,
		"--cases-dir", casesDir,
		"--donmai-bin", "/bin/echo",
		"--repo-root", "r=/tmp/repo",
		"--platform-url", "https://platform.invalid",
		"--platform-token", "test-token",
		"--org", "org_test",
		"--project", "proj_test",
		"--dataset-id", "evd_test",
		"--receipt-file", receiptPath,
		"--case", "e2-simple-001",
		"--trial-start", "2",
		"--trials", "1",
		"--max-turns", "12",
		"--max-tokens", "200000",
	})
	if err := root.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("expected captured-config sentinel, got %v", err)
	}
	if captured.Executor == nil || captured.Executor.Name() != "claude" {
		t.Fatalf("executor = %v, want claude", captured.Executor)
	}
	if captured.Advertise != eval.AdvertiseMCP || !captured.AdvertiseAllTools {
		t.Fatalf("advertisement config = %q all=%v", captured.Advertise, captured.AdvertiseAllTools)
	}
	if captured.Bridge == nil || !captured.Bridge.Enabled() || captured.PromptReceiptJournal == nil || captured.Trials != 1 || captured.TrialStart != 2 {
		t.Fatalf("receipt config = bridge:%v journal:%v trials:%d trial-start:%d", captured.Bridge, captured.PromptReceiptJournal, captured.Trials, captured.TrialStart)
	}
}

func TestEvalCodeIntel_AdvertiseAllToolsPropagatesToDriverConfig(t *testing.T) {
	benchDir := writeMinimalBenchmarkDir(t)

	var captured []eval.Config
	sentinel := errors.New("driver config captured")
	orig := evalNewDriver
	evalNewDriver = func(cfg eval.Config) (*eval.Driver, error) {
		captured = append(captured, cfg)
		return nil, sentinel
	}
	defer func() { evalNewDriver = orig }()

	run := func(extra ...string) {
		t.Helper()
		root := &cobra.Command{Use: "donmai", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(newEvalCmd(Config{}))
		args := append([]string{
			"eval", "codeintel",
			"--benchmark-dir", benchDir,
			"--donmai-bin", "/bin/echo",
		}, extra...)
		root.SetArgs(args)
		if err := root.Execute(); !errors.Is(err, sentinel) {
			t.Fatalf("expected the captured-config sentinel error, got %v", err)
		}
	}

	run("--advertise-all-tools")
	run()

	if len(captured) != 2 {
		t.Fatalf("captured %d configs, want 2", len(captured))
	}
	if !captured[0].AdvertiseAllTools {
		t.Errorf("--advertise-all-tools must set Config.AdvertiseAllTools; got %+v", captured[0].AdvertiseAllTools)
	}
	if captured[1].AdvertiseAllTools {
		t.Errorf("default run must leave Config.AdvertiseAllTools false")
	}
	// Sanity on adjacent wiring: the default advertise mode reaches the Config.
	if captured[0].Advertise != eval.AdvertiseMCP {
		t.Errorf("Config.Advertise = %q, want %q", captured[0].Advertise, eval.AdvertiseMCP)
	}
}
