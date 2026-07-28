package afcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	eval "github.com/RenseiAI/donmai/eval/codeintel"
	"github.com/RenseiAI/donmai/eval/experiment"
)

type evalPromptExperimentOpts struct {
	configPath   string
	casesDir     string
	caseFilter   string
	trials       int
	trialStart   int
	donmaiBin    string
	repoRoots    []string
	platformURL  string
	platformTok  string
	platformPath string
	orgID        string
	projectID    string
	datasetID    string
	receiptFile  string
	maxTurns     int
	maxTokens    int64
	keepWA       bool
	jsonOut      bool
	timeout      time.Duration
}

type promptExperimentOutcome struct {
	ExperimentID string           `json:"experimentId"`
	CaseID       string           `json:"caseId"`
	Arm          experiment.ArmID `json:"arm"`
	SubjectRef   string           `json:"subjectRef"`
	VariantRef   string           `json:"variantRef"`
	TrialIndex   int              `json:"trialIndex"`
	PostedRunID  string           `json:"postedRunId"`
	CostUSD      float64          `json:"costUsd"`
	TurnCount    int              `json:"turnCount"`
	TokenCounts  eval.TokenCounts `json:"tokenCounts"`
}

type promptExperimentSummary struct {
	ExperimentID   string                    `json:"experimentId"`
	TrialsPerArm   int                       `json:"trialsPerArm"`
	ExecutionCount int                       `json:"executionCount"`
	TotalCostUSD   float64                   `json:"totalCostUsd"`
	Outcomes       []promptExperimentOutcome `json:"outcomes"`
}

func newEvalPromptExperimentCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	opts := &evalPromptExperimentOpts{}
	cmd := &cobra.Command{
		Use:    "prompt-experiment",
		Short:  "Run a reviewed prompt experiment with durable platform receipts",
		Hidden: true,
		Long: `Run a reviewed prompt experiment through the real Claude executor. The
config binds each arm to process-local system-prompt files and derives immutable
SHA-256 variant refs. Cases are provisioned at pinned repository refs, every
completed execution is append-written to a sanitized local JSONL journal and
fsynced before platform posting. A successful post appends a second durable event,
so retries skip already-posted trials and fail closed on uncertain paid executions.
The final summary records provider-reported cost without emitting raw prompt text.

This command is an operator harness. It does not promote prompt variants or
mutate production cards. Apply the experiment's external spend authorization
before invoking it.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvalPromptExperiment(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.configPath, "config", "", "Reviewed experiment JSON config")
	f.StringVar(&opts.casesDir, "cases-dir", "", "Directory containing experiment JSONL cases")
	f.StringVar(&opts.caseFilter, "case", "", "Run exactly this case id")
	f.IntVar(&opts.trials, "trials", 3, "Repeated trials per arm")
	f.IntVar(&opts.trialStart, "trial-start", 1, "First one-based trial index")
	f.StringVar(&opts.donmaiBin, "donmai-bin", "", "Path to the donmai binary used by the MCP capability profile")
	f.StringArrayVar(&opts.repoRoots, "repo-root", nil, "Repo clone source as slug=path; repeatable")
	f.StringVar(&opts.platformURL, "platform-url", "", "Platform base URL for durable eval receipts")
	f.StringVar(&opts.platformTok, "platform-token", "", "Bearer token for the platform reporting bridge")
	f.StringVar(&opts.platformPath, "platform-path", eval.DefaultBridgePath, "Platform ingest path")
	f.StringVar(&opts.orgID, "org", "", "Org id for eval receipt rows")
	f.StringVar(&opts.projectID, "project", "", "Project id for eval receipt rows")
	f.StringVar(&opts.datasetID, "dataset-id", "", "Registered dataset id for eval receipt rows")
	f.StringVar(&opts.receiptFile, "receipt-file", "", "Append-only sanitized JSONL execution receipt journal")
	f.IntVar(&opts.maxTurns, "max-turns", 12, "Equal total max-turn budget for every arm")
	f.Int64Var(&opts.maxTokens, "max-tokens", 200000, "Equal max-token budget for every arm")
	f.BoolVar(&opts.keepWA, "keep-workareas", false, "Leave provisioned workareas on disk")
	f.BoolVar(&opts.jsonOut, "json", false, "Emit a sanitized receipt/cost summary as JSON")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Minute, "Overall run timeout")
	_ = cmd.MarkFlagRequired("config")
	_ = cmd.MarkFlagRequired("cases-dir")
	_ = cmd.MarkFlagRequired("platform-url")
	_ = cmd.MarkFlagRequired("platform-token")
	_ = cmd.MarkFlagRequired("org")
	_ = cmd.MarkFlagRequired("project")
	_ = cmd.MarkFlagRequired("dataset-id")
	_ = cmd.MarkFlagRequired("receipt-file")
	_ = cmd.MarkFlagRequired("case")
	_ = cmd.MarkFlagRequired("trials")
	_ = cmd.MarkFlagRequired("max-turns")
	_ = cmd.MarkFlagRequired("max-tokens")
	cmd.Example = bin + " eval prompt-experiment --config experiment.json --cases-dir cases --trial-start 1 --trials 1 --case calibration-001 --repo-root owner/repo=. --platform-url https://... --platform-token rsk_... --org org_... --project proj_... --dataset-id evd_... --receipt-file receipts.jsonl --max-turns 12 --max-tokens 200000 --json"
	return cmd
}

func validatePromptExperimentOpts(opts *evalPromptExperimentOpts) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "--config", value: opts.configPath},
		{name: "--cases-dir", value: opts.casesDir},
		{name: "--case", value: opts.caseFilter},
		{name: "--platform-url", value: opts.platformURL},
		{name: "--platform-token", value: opts.platformTok},
		{name: "--org", value: opts.orgID},
		{name: "--project", value: opts.projectID},
		{name: "--dataset-id", value: opts.datasetID},
		{name: "--receipt-file", value: opts.receiptFile},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must be non-empty", field.name)
		}
	}
	if opts.trials <= 0 {
		return fmt.Errorf("--trials must be positive")
	}
	if opts.trialStart <= 0 {
		return fmt.Errorf("--trial-start must be positive")
	}
	if opts.maxTurns <= 0 {
		return fmt.Errorf("--max-turns must be positive")
	}
	if opts.maxTokens <= 0 {
		return fmt.Errorf("--max-tokens must be positive")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	return nil
}

func selectPromptExperimentCase(cases []eval.Case, caseID string) ([]eval.Case, error) {
	selected := make([]eval.Case, 0, 1)
	for _, c := range cases {
		if c.ID == caseID {
			selected = append(selected, c)
		}
	}
	if len(selected) != 1 {
		return nil, fmt.Errorf("--case must select exactly one case by exact id (case=%q matches=%d)", caseID, len(selected))
	}
	return selected, nil
}

func runEvalPromptExperiment(cmd *cobra.Command, opts *evalPromptExperimentOpts) (runErr error) {
	if err := validatePromptExperimentOpts(opts); err != nil {
		return err
	}
	ledger, err := eval.OpenPromptExperimentReceiptLedger(opts.receiptFile)
	if err != nil {
		return fmt.Errorf("open prompt receipt journal: %w", err)
	}
	defer func() {
		if err := ledger.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close prompt receipt journal: %w", err)
		}
	}()

	loaded, err := experiment.LoadConfigFile(opts.configPath)
	if err != nil {
		return err
	}
	cases, err := eval.LoadPromptExperimentCasesDir(opts.casesDir)
	if err != nil {
		return fmt.Errorf("load prompt-experiment cases: %w", err)
	}
	cases, err = selectPromptExperimentCase(cases, opts.caseFilter)
	if err != nil {
		return err
	}
	repoRoots, err := resolveRepoRoots(opts.repoRoots)
	if err != nil {
		return err
	}
	donmaiBin := opts.donmaiBin
	if donmaiBin == "" {
		donmaiBin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve donmai binary: %w (pass --donmai-bin)", err)
		}
	}
	bridge := eval.NewBridge(opts.platformURL, opts.platformTok, opts.platformPath)
	driver, err := evalNewDriver(eval.Config{
		Trials:               opts.trials,
		TrialStart:           opts.trialStart,
		Advertise:            eval.AdvertiseMCP,
		AdvertiseAllTools:    true,
		DonmaiBin:            donmaiBin,
		RepoRoots:            repoRoots,
		Budget:               eval.Budget{MaxTurns: opts.maxTurns, MaxTokens: opts.maxTokens},
		Executor:             eval.NewClaudeExecutor(),
		Bridge:               bridge,
		PromptReceiptJournal: ledger,
		OrgID:                opts.orgID,
		ProjectID:            opts.projectID,
		DatasetID:            opts.datasetID,
		DatasetName:          loaded.Definition.ID,
		KeepWorkareas:        opts.keepWA,
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[eval] "+format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, opts.timeout)
	defer cancel()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[eval] running experiment=%s cases=%d trial_start=%d trials=%d arms=%d (live claude, durable receipts)\n",
		loaded.Definition.ID, len(cases), opts.trialStart, opts.trials, len(loaded.Definition.Arms))
	report, err := driver.RunPromptExperiment(ctx, cases, loaded.Definition, loaded.GraderIDs)
	if err != nil {
		return fmt.Errorf("prompt experiment: %w", err)
	}
	summary := summarizePromptExperiment(report)
	if opts.jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "experiment=%s executions=%d total_cost_usd=%.6f\n",
		summary.ExperimentID, summary.ExecutionCount, summary.TotalCostUSD)
	return err
}

func summarizePromptExperiment(report experiment.Report[eval.RunRecord]) promptExperimentSummary {
	summary := promptExperimentSummary{
		ExperimentID: report.ExperimentID,
		TrialsPerArm: report.TrialsPerArm,
		Outcomes:     make([]promptExperimentOutcome, 0, len(report.Outcomes)),
	}
	for _, outcome := range report.Outcomes {
		record := outcome.Result
		summary.TotalCostUSD += record.CostUSD
		summary.Outcomes = append(summary.Outcomes, promptExperimentOutcome{
			ExperimentID: report.ExperimentID,
			CaseID:       outcome.Trial.CaseID,
			Arm:          outcome.Trial.Arm.ID,
			SubjectRef:   outcome.Trial.Arm.SubjectRef,
			VariantRef:   outcome.Trial.Arm.VariantRef,
			TrialIndex:   outcome.Trial.TrialIndex,
			PostedRunID:  record.PostedRunID,
			CostUSD:      record.CostUSD,
			TurnCount:    record.Envelope.Trace.TurnCount,
			TokenCounts:  record.Envelope.Trace.TokenCounts,
		})
	}
	summary.ExecutionCount = len(summary.Outcomes)
	return summary
}
