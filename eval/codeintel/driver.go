package codeintel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/eval/experiment"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

var graderIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:/[a-z][a-z0-9._-]*)+$`)

// Config configures a Driver run.
type Config struct {
	// Trials is the repeated-trial count per arm (brief 06 §4.3.4; default 3,
	// allow 1 for a fast plumbing run).
	Trials int
	// Advertise selects the WITH-arm advertisement mechanism (default MCP).
	Advertise AdvertiseMode
	// AdvertiseAllTools disables the WS2 core-subset rule and advertises all
	// six af_code_* tools to every WITH arm (the pre-WS2 surface) — kept as an
	// A/B escape hatch for measuring the subset's own effect.
	AdvertiseAllTools bool
	// DonmaiBin is the absolute path to the donmai binary the WITH arm uses.
	DonmaiBin string
	// RepoRoots maps a case repo slug (e.g. "RenseiAI/donmai") to a local clone
	// source (path or URL) the driver provisions from.
	RepoRoots map[string]string
	// WorkareaParent is the parent dir under which per-arm workareas are created.
	WorkareaParent string
	// Budget is the equal per-arm turn/token cap.
	Budget Budget
	// Executor runs each arm (plumbing by default; a live-LLM executor when wired).
	Executor Executor
	// Judge scores refactor tasks (nil → refactor tasks are recorded but never pass).
	Judge Judge
	// Bridge posts results to the platform (nil / disabled → offline).
	Bridge *Bridge
	// PromptReceiptJournal durably records paid prompt-experiment executions
	// before bridge posting and indexes them to prevent duplicate retry spend.
	// Nil preserves ordinary code-intel and offline behavior.
	PromptReceiptJournal PromptExperimentReceiptJournal
	// Reporting context for the eval_runs rows.
	OrgID, ProjectID, DatasetID, DatasetName string
	// KeepWorkareas leaves provisioned workareas on disk (debugging).
	KeepWorkareas bool
	// Logf receives progress lines (nil → discard).
	Logf func(string, ...any)
}

// Driver is the code-intel A/B orchestrator and prompt-experiment execution adapter.
type Driver struct {
	cfg Config
	wm  *worktree.Manager
	ad  Advertisement
}

// NewDriver validates cfg and builds the worktree manager.
func NewDriver(cfg Config) (*Driver, error) {
	if cfg.Trials <= 0 {
		cfg.Trials = 3
	}
	if cfg.Advertise == "" {
		cfg.Advertise = AdvertiseMCP
	}
	if cfg.Executor == nil {
		cfg.Executor = NewPlumbingExecutor()
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if strings.TrimSpace(cfg.DonmaiBin) == "" {
		return nil, fmt.Errorf("driver: DonmaiBin is required")
	}
	if cfg.WorkareaParent == "" {
		cfg.WorkareaParent = filepath.Join(os.TempDir(), "codeintel-eval-workareas")
	}
	wm, err := worktree.NewManager(worktree.Options{ParentDir: cfg.WorkareaParent})
	if err != nil {
		return nil, fmt.Errorf("driver: worktree manager: %w", err)
	}
	return &Driver{cfg: cfg, wm: wm, ad: NewAdvertisement(cfg.Advertise, cfg.AdvertiseAllTools)}, nil
}

// RunRecord is one (case, arm, trial) outcome.
type RunRecord struct {
	CaseID       string         `json:"caseId"`
	Family       TaskType       `json:"family"`
	Repo         string         `json:"repo"`
	Arm          Arm            `json:"arm"`
	Trial        int            `json:"trial"`
	Pass         bool           `json:"pass"`
	CostUSD      float64        `json:"costUsd"`
	CostReported bool           `json:"costReported"`
	CostComplete bool           `json:"costComplete"`
	Grades       []GradeResult  `json:"grades"`
	Envelope     ReportEnvelope `json:"envelope"`
	Posted       bool           `json:"posted"`
	// PostedRunID is the platform-assigned eval_runs id returned by the ingest
	// route (empty when offline or on a post error) — the handle an operator uses
	// to find this trial in /admin/evals.
	PostedRunID string `json:"postedRunId,omitempty"`
	PostErr     string `json:"postError,omitempty"`
}

// Report is the aggregate outcome of a Run.
type Report struct {
	Trials      int                      `json:"trials"`
	Advertise   AdvertiseMode            `json:"advertise"`
	Records     []RunRecord              `json:"records"`
	Families    map[TaskType]*FamilyStat `json:"families"`
	Aggregate   AggregateStat            `json:"aggregate"`
	PostedCount int                      `json:"postedCount"`
	PostErrors  int                      `json:"postErrors"`
}

// FamilyStat is the per-family A/B rollup.
type FamilyStat struct {
	WithPasses    int     `json:"withPasses"`
	WithTrials    int     `json:"withTrials"`
	WithoutPasses int     `json:"withoutPasses"`
	WithoutTrials int     `json:"withoutTrials"`
	Adoption      int     `json:"adoptionCount"` // WITH-arm trials that invoked a code-intel tool
	WithTokens    []int64 `json:"-"`
	WithoutTokens []int64 `json:"-"`
}

// WithRate is the WITH-arm task-success rate.
func (f *FamilyStat) WithRate() float64 { return rate(f.WithPasses, f.WithTrials) }

// WithoutRate is the WITHOUT-arm task-success rate.
func (f *FamilyStat) WithoutRate() float64 { return rate(f.WithoutPasses, f.WithoutTrials) }

// DeltaPP is the WITH−WITHOUT success delta in percentage points.
func (f *FamilyStat) DeltaPP() float64 { return (f.WithRate() - f.WithoutRate()) * 100 }

// TokenRatio is median(WITH tokens) / median(WITHOUT tokens).
func (f *FamilyStat) TokenRatio() float64 {
	w, wo := medianI64(f.WithTokens), medianI64(f.WithoutTokens)
	if wo == 0 {
		return 0
	}
	return float64(w) / float64(wo)
}

// AdoptionRate is the fraction of WITH-arm trials that used a code-intel tool.
func (f *FamilyStat) AdoptionRate() float64 { return rate(f.Adoption, f.WithTrials) }

// Locked power preconditions (brief 06 §4.5): ANY GA verdict — the retired
// success-delta bar or the current efficiency bar — is only a claim over
// >=8-12 tasks/family x 2 repos x >=3 trials/arm; token-ratio medians need
// sample size just as much as success rates did. An underpowered or
// --task-filtered run must never report a PASS.
const (
	// minTasksPerFamily is the floor of the brief's 8-12 tasks/family band.
	minTasksPerFamily = 8
	// minReposCovered is the brief's "x 2 repos" (platform TS + donmai Go).
	minReposCovered = 2
	// minTrialsForGA is the brief's ">=3 trials/arm" to average out LLM nondeterminism.
	minTrialsForGA = 3
)

// Q1v2 efficiency bar (founder decision, 2026-07-06). The ORIGINAL GA bar —
// aggregate success delta >=+15pp on the 95% CI lower bound — was RETIRED
// after the 2026-07-06 decision-gate eval proved it unreachable against
// frontier agents: the control (WITHOUT) arm scored 100% even on
// grep-resistant probes, so no tool surface can buy +15 percentage points of
// success. The GA claim is now an EFFICIENCY claim: the code-intel surface
// must make the agent CHEAPER without making it worse. On a powered run the
// verdict requires all three of:
//
//  1. aggregate tokenRatio <= maxAggregateTokenRatio — WITH is no more
//     expensive than WITHOUT overall;
//  2. every family WITH token data holds tokenRatio <= maxFamilyTokenRatio —
//     no family pays more than +10% for the surface (families lacking token
//     data on either arm are excluded, same convention as the regression
//     check);
//  3. no per-family SUCCESS regression — WithRate >= WithoutRate wherever
//     both arms ran.
//
// The success delta (DeltaPP + case-clustered bootstrap CI) stays COMPUTED
// AND REPORTED — informational now, not gating.
const (
	// maxAggregateTokenRatio is the whole-benchmark median-token ceiling.
	maxAggregateTokenRatio = 1.0
	// maxFamilyTokenRatio is the per-family ceiling (+10% worst case).
	maxFamilyTokenRatio = 1.10
)

// AggregateStat is the whole-benchmark rollup + the efficiency-threshold verdict.
type AggregateStat struct {
	DeltaPP           float64    `json:"deltaPP"`
	TokenRatio        float64    `json:"tokenRatio"`
	AdoptionRate      float64    `json:"adoptionRate"`
	RegressedFamilies []TaskType `json:"regressedFamilies"`
	// TokenRegressedFamilies lists families (with token data on both arms)
	// whose median-token ratio exceeds maxFamilyTokenRatio — each one is an
	// independent efficiency-bar failure.
	TokenRegressedFamilies []TaskType `json:"tokenRegressedFamilies,omitempty"`
	// Trials / FamilyCounts / RepoCounts capture the corpus power actually run,
	// computed from the executed cases — not assumed.
	Trials       int              `json:"trials"`
	FamilyCounts map[TaskType]int `json:"familyCounts"`
	RepoCounts   map[string]int   `json:"repoCounts"`
	// Underpowered is true when the run does not meet the locked power
	// preconditions (>=8 tasks/family across all four families, >=2 repos, >=3
	// trials) or the token-coverage precondition (every executed family must
	// carry a nonzero token median on BOTH arms — the efficiency bar is a
	// token-cost claim). PowerShortfalls enumerates exactly which
	// preconditions failed.
	Underpowered    bool     `json:"underpowered"`
	PowerShortfalls []string `json:"powerShortfalls,omitempty"`
	// DeltaCILow/High are the 95% confidence interval of the aggregate delta from
	// a CASE-CLUSTERED bootstrap (resampling whole cases, not trials, since the
	// >=3 trials of one case@sha are strongly correlated — pooling them as
	// independent Bernoulli understates variance). DeltaStdDev is the bootstrap
	// standard deviation. INFORMATIONAL since the Q1v2 decision (2026-07-06):
	// the retired success-delta bar gated on DeltaCILow; the efficiency bar
	// does not, but the interval stays computed and reported so a run's delta
	// claim is always variance-qualified.
	DeltaCILow  float64 `json:"deltaCiLow"`
	DeltaCIHigh float64 `json:"deltaCiHigh"`
	DeltaStdDev float64 `json:"deltaStdDev"`
	// Status is the human-legible verdict category: "UNDERPOWERED — not a GA
	// verdict", "GA-PASS …", or "GA-FAIL …".
	Status string `json:"status"`
	// MeetsThreshold reflects the Q1v2 EFFICIENCY bar (see the constant block
	// above): aggregate tokenRatio <= 1.0, every family with data <= 1.10,
	// no per-family success regression — AND the power preconditions. It can
	// only be true on a statistically-powered run; an underpowered run is
	// forced to false regardless of the ratios.
	MeetsThreshold bool `json:"meetsThreshold"`
}

// Run executes the full code-intel A/B matrix over cases and returns the Report.
// Trial enumeration is delegated to the provider-neutral experiment package;
// this package remains the concrete provisioning/execution/grading consumer.
func (d *Driver) Run(ctx context.Context, cases []Case) (Report, error) {
	rep := Report{Trials: d.cfg.Trials, Advertise: d.cfg.Advertise, Families: map[TaskType]*FamilyStat{}}
	matrixCases, caseByID := experimentCases(cases)
	matrix, err := experiment.Run(ctx, experiment.Definition{Arms: []experiment.Arm{{ID: ArmWithout}, {ID: ArmWith}}}, matrixCases, d.cfg.Trials,
		func(ctx context.Context, trial experiment.Trial) (RunRecord, error) {
			return d.runPlannedOne(ctx, caseByID[trial.CaseID], trial, trial.Arm.ID == ArmWith, nil, false)
		})
	if err != nil {
		return rep, err
	}
	for _, outcome := range matrix.Outcomes {
		rec := outcome.Result
		fam := rep.Families[rec.Family]
		if fam == nil {
			fam = &FamilyStat{}
			rep.Families[rec.Family] = fam
		}
		accumulate(fam, rec)
		if rec.Posted {
			rep.PostedCount++
		}
		if rec.PostErr != "" {
			rep.PostErrors++
		}
		rep.Records = append(rep.Records, rec)
	}
	rep.Aggregate = computeAggregate(rep.Families, rep.Records, cases, d.cfg.Trials)
	return rep, nil
}

// RunPromptExperiment executes arbitrary prompt arms through the same pinned
// workarea, real executor, transcript capture, and bridge path as code-intel.
// Generic experiments require explicit platform graders and MCP advertisement;
// local code-intel grades remain diagnostic only.
func (d *Driver) RunPromptExperiment(
	ctx context.Context,
	cases []Case,
	definition experiment.Definition,
	graderIDs []string,
) (experiment.Report[RunRecord], error) {
	if definition.ID == "" {
		return experiment.Report[RunRecord]{}, fmt.Errorf("prompt experiment id is required")
	}
	if d.cfg.Advertise != AdvertiseMCP {
		return experiment.Report[RunRecord]{}, fmt.Errorf("prompt experiments require MCP advertisement so the variant hash binds the complete appended system prompt")
	}
	if err := validateGraderIDs(graderIDs); err != nil {
		return experiment.Report[RunRecord]{}, err
	}
	capable, ok := d.cfg.Executor.(promptExperimentExecutor)
	if !ok || !capable.SupportsPromptExperiments() {
		return experiment.Report[RunRecord]{}, fmt.Errorf("executor %q does not support prompt experiments", d.cfg.Executor.Name())
	}
	matrixCases, caseByID := experimentCases(cases)
	return experiment.Run(ctx, definition, matrixCases, d.cfg.Trials,
		func(ctx context.Context, trial experiment.Trial) (RunRecord, error) {
			return d.runPlannedOne(ctx, caseByID[trial.CaseID], trial, true, graderIDs, true)
		})
}

func validateGraderIDs(graderIDs []string) error {
	if len(graderIDs) == 0 {
		return fmt.Errorf("prompt experiments require at least one explicit grader id")
	}
	seen := make(map[string]struct{}, len(graderIDs))
	for _, graderID := range graderIDs {
		if len(graderID) > 160 || !graderIDPattern.MatchString(graderID) {
			return fmt.Errorf("grader id %q must be a concrete registry path", graderID)
		}
		if _, ok := seen[graderID]; ok {
			return fmt.Errorf("duplicate grader id %q", graderID)
		}
		seen[graderID] = struct{}{}
	}
	return nil
}

func experimentCases(cases []Case) ([]experiment.Case, map[string]Case) {
	matrixCases := make([]experiment.Case, 0, len(cases))
	caseByID := make(map[string]Case, len(cases))
	for _, c := range cases {
		matrixCases = append(matrixCases, experiment.Case{ID: c.ID, Prompt: c.Input.Prompt})
		caseByID[c.ID] = c
	}
	return matrixCases, caseByID
}

// runPlannedOne provisions one workarea, executes one arm, grades, posts, and tears down.
func (d *Driver) runPlannedOne(
	ctx context.Context,
	c Case,
	trial experiment.Trial,
	useCodeIntel bool,
	graderIDs []string,
	failOnPostError bool,
) (RunRecord, error) {
	arm := Arm(trial.Arm.ID)
	isPromptExperiment := trial.ExperimentID != ""
	var receiptIdentity PromptExperimentReceiptIdentity
	if isPromptExperiment && d.cfg.PromptReceiptJournal != nil {
		var err error
		receiptIdentity, err = promptExperimentReceiptIdentity(d.cfg, c, trial, graderIDs)
		if err != nil {
			return RunRecord{}, err
		}
		state, err := d.cfg.PromptReceiptJournal.Lookup(receiptIdentity)
		if err != nil {
			return RunRecord{}, fmt.Errorf("lookup prompt receipt: %w", err)
		}
		switch state.Status {
		case PromptExperimentReceiptMissing:
			// First attempt: proceed to provider execution.
		case PromptExperimentReceiptPlatformPosted:
			d.cfg.Logf("skipping already-posted prompt trial %s/%s/%d receipt=%s", c.ID, arm, trial.TrialIndex, receiptIdentity.ReceiptID)
			return reconstructPostedPromptRun(c, arm, trial.TrialIndex, state.Receipt), nil
		case PromptExperimentReceiptExecutionCompleted:
			return RunRecord{}, fmt.Errorf("prompt receipt %s is execution_completed but not platform_posted; refusing duplicate provider execution", receiptIdentity.ReceiptID)
		default:
			return RunRecord{}, fmt.Errorf("prompt receipt %s has unknown state %q", receiptIdentity.ReceiptID, state.Status)
		}
	}

	wa, sessionID, err := d.provision(ctx, c, arm, trial.TrialIndex)
	if err != nil {
		return RunRecord{}, err
	}
	if !d.cfg.KeepWorkareas {
		defer func() { _ = d.wm.Teardown(context.Background(), sessionID) }()
	}
	d.cfg.Logf("provisioned %s arm=%s trial=%d at %s", c.ID, arm, trial.TrialIndex, wa)

	spec, cleanup, err := d.buildPlannedArmSpec(ctx, c, arm, wa, sessionID, trial.Prompt, useCodeIntel, !isPromptExperiment)
	if err != nil {
		return RunRecord{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	tr, executeErr := d.cfg.Executor.Execute(ctx, spec)
	var promptReceipt PromptExperimentReceipt
	if isPromptExperiment && d.cfg.PromptReceiptJournal != nil {
		disposition := PromptExperimentDispositionCompleted
		if executeErr != nil {
			disposition = PromptExperimentDispositionExecutionError
		}
		promptReceipt = buildPromptExperimentReceipt(receiptIdentity, tr, disposition)
		if err := d.cfg.PromptReceiptJournal.RecordExecutionCompleted(promptReceipt); err != nil {
			return RunRecord{}, fmt.Errorf("write execution_completed receipt: %w", err)
		}
	}
	if executeErr != nil {
		return RunRecord{}, fmt.Errorf("execute: %w", executeErr)
	}
	if tr.Arm != arm {
		return RunRecord{}, fmt.Errorf("execute: transcript arm %q does not match planned arm %q", tr.Arm, arm)
	}
	if isPromptExperiment && (!tr.CostReported || !tr.CostComplete) {
		return RunRecord{}, fmt.Errorf("execute: provider did not report complete provider cost for prompt experiment trial")
	}

	grades := d.grade(ctx, c, tr, useCodeIntel)
	pass := taskSuccessPass(grades)

	meta := ReportMeta{
		CaseID: c.ID, Arm: arm, Family: string(c.Family()), Repo: c.Input.Repo, Ref: c.Input.Ref,
		Trial: trial.TrialIndex, Advertisement: string(d.cfg.Advertise), DatasetName: d.cfg.DatasetName,
	}
	env, err := BuildEnvelope(newID("run"), newID("trace"), sessionID, d.cfg.OrgID, d.cfg.ProjectID, d.cfg.DatasetID, c, tr, grades, meta)
	if err != nil {
		return RunRecord{}, err
	}

	rec := RunRecord{
		CaseID: c.ID, Family: c.Family(), Repo: c.Input.Repo, Arm: arm,
		Trial: trial.TrialIndex, Pass: pass, CostUSD: tr.CostUSD,
		CostReported: tr.CostReported, CostComplete: tr.CostComplete,
		Grades: grades, Envelope: env,
	}
	if d.cfg.Bridge != nil {
		// The wire contract is the platform's flat per-trial /api/evals/ingest
		// body (the route runs the registered graders inline). The local
		// ReportEnvelope above stays the harness's canonical eval_runs+eval_traces
		// capture (dumped by --dry and used for the offline record).
		ingest := BuildIngestRequest(c, tr, trial.TrialIndex, sessionID, d.cfg.DatasetID, d.cfg.ProjectID)
		if isPromptExperiment {
			ingest.GraderIDs = append([]string(nil), graderIDs...)
			ingest.Experiment = &ExperimentReceipt{
				ExperimentID: trial.ExperimentID,
				SubjectRef:   trial.Arm.SubjectRef,
				VariantRef:   trial.Arm.VariantRef,
			}
		}
		resp, perr := d.cfg.Bridge.Post(ctx, ingest)
		if perr != nil {
			rec.PostErr = perr.Error()
			d.cfg.Logf("bridge post failed for %s/%s: %v", c.ID, arm, perr)
			if failOnPostError {
				return rec, fmt.Errorf("bridge post: %w", perr)
			}
		}
		if resp != nil {
			if isPromptExperiment && d.cfg.PromptReceiptJournal != nil {
				postedReceipt := promptReceipt
				postedReceipt.PostedRunID = resp.RunID
				if err := d.cfg.PromptReceiptJournal.RecordPlatformPosted(postedReceipt); err != nil {
					return rec, fmt.Errorf("write platform_posted receipt: %w", err)
				}
			}
			rec.Posted = true
			rec.PostedRunID = resp.RunID
			d.cfg.Logf("posted %s/%s → eval_run %s (platform graders: %v)", c.ID, arm, resp.RunID, resp.GradersRun)
		}
	}
	return rec, nil
}

func buildPromptExperimentReceipt(identity PromptExperimentReceiptIdentity, tr Transcript, disposition string) PromptExperimentReceipt {
	knownCostUSD := tr.CostUSD
	if !tr.CostReported {
		knownCostUSD = 0
	}
	return PromptExperimentReceipt{
		ExperimentID:          identity.ExperimentID,
		CaseID:                identity.CaseID,
		Arm:                   identity.Arm,
		SubjectRef:            identity.SubjectRef,
		VariantRef:            identity.VariantRef,
		TrialIndex:            identity.TrialIndex,
		InvocationScopeDigest: identity.InvocationScopeDigest,
		ReceiptID:             identity.ReceiptID,
		Disposition:           disposition,
		CostUSD:               knownCostUSD,
		CostCompleteness:      promptExperimentCostCompleteness(tr),
		TurnCount:             tr.TurnCount,
		TokenCounts:           tr.TokenCounts,
	}
}

func promptExperimentCostCompleteness(tr Transcript) PromptExperimentCostCompleteness {
	if tr.CostComplete {
		return PromptExperimentCostComplete
	}
	if tr.CostReported {
		return PromptExperimentCostPartial
	}
	return PromptExperimentCostMissing
}

func reconstructPostedPromptRun(c Case, arm Arm, trialIndex int, receipt PromptExperimentReceipt) RunRecord {
	return RunRecord{
		CaseID:       c.ID,
		Family:       c.Family(),
		Repo:         c.Input.Repo,
		Arm:          arm,
		Trial:        trialIndex,
		CostUSD:      receipt.CostUSD,
		CostReported: receipt.CostCompleteness != PromptExperimentCostMissing,
		CostComplete: receipt.CostCompleteness == PromptExperimentCostComplete,
		Envelope: ReportEnvelope{Trace: TraceInsert{
			TurnCount:   receipt.TurnCount,
			TokenCounts: receipt.TokenCounts,
		}},
		Posted:      true,
		PostedRunID: receipt.PostedRunID,
	}
}

// buildArmSpec assembles the legacy per-arm env + advertisement + MCP config.
func (d *Driver) buildArmSpec(ctx context.Context, c Case, arm Arm, wa, sessionID string) (ArmSpec, func(), error) {
	return d.buildPlannedArmSpec(ctx, c, arm, wa, sessionID, experiment.PromptPlan{UserPrompt: c.Input.Prompt}, arm == ArmWith, true)
}

func (d *Driver) buildPlannedArmSpec(
	ctx context.Context,
	c Case,
	arm Arm,
	wa, _ string,
	plan experiment.PromptPlan,
	useCodeIntel bool,
	includePromptSuffix bool,
) (ArmSpec, func(), error) {
	base := runtimeenv.FilterRunnerOnly(os.Environ())
	spec := ArmSpec{
		Arm: arm, Case: c, Workarea: wa, DonmaiBin: d.cfg.DonmaiBin,
		Budget: d.cfg.Budget, AdvertiseMode: d.cfg.Advertise, UseCodeIntel: useCodeIntel, SnapshotID: workareaLeaf(wa),
		Prompt: plan.UserPrompt, VariantSystemPrompt: plan.SystemPrompt, ContextReset: plan.ContextReset,
	}
	// Neutralize donmai on the SHARED base so BOTH arms resolve an identical set
	// of non-donmai tools (rg/gh/git). Dropping the whole directory that holds
	// donmai (the old ScrubBinaryFromEnv approach) strips those baseline tools
	// from the control only when donmai is co-installed alongside them (e.g.
	// /opt/homebrew/bin on the dogfooding host), asymmetrically inflating the A/B
	// delta. Name-granular masking removes donmai and nothing else.
	neutralBase, neutralCleanup, err := NeutralizeBinaryInEnv(base, "donmai")
	if err != nil {
		return ArmSpec{}, nil, fmt.Errorf("neutralize control PATH: %w", err)
	}
	if !useCodeIntel {
		// Control: the neutral base — same tools as treatment, minus donmai.
		spec.Env = neutralBase
		return spec, neutralCleanup, nil
	}
	// Treatment: neutral base + a dedicated donmai-ONLY dir (re-adding donmai
	// cannot re-introduce any sibling tool the control lacks) and the advertisement.
	donmaiDir, donmaiCleanup, err := StageBinaryOnlyDir(d.cfg.DonmaiBin, "donmai")
	if err != nil {
		neutralCleanup()
		return ArmSpec{}, nil, fmt.Errorf("stage donmai dir: %w", err)
	}
	withEnv := PrependPath(neutralBase, donmaiDir)
	spec.Env = withEnv
	cleanup := func() { donmaiCleanup(); neutralCleanup() }
	servers, suffix, err := d.ad.Apply(ctx, d.cfg.DonmaiBin, wa, c.Input.RepoPath, c.Family(), withEnv)
	if err != nil {
		cleanup()
		return ArmSpec{}, nil, fmt.Errorf("advertise: %w", err)
	}
	spec.MCPServers = servers
	if includePromptSuffix {
		spec.PromptSuffix = suffix
	}
	// Prompt experiments use MCP discovery without a separate shared system
	// suffix, so VariantRef hashes the complete appended system-prompt bytes.
	spec.AdvertisedTools = d.ad.AdvertisedToolNames(c.Family())
	if len(servers) > 0 {
		path, werr := clijsonl.WriteMCPConfig(servers)
		if werr != nil {
			cleanup()
			return ArmSpec{}, nil, fmt.Errorf("write mcp config: %w", werr)
		}
		spec.MCPConfigPath = path
		prev := cleanup
		cleanup = func() { _ = clijsonl.RemoveMCPConfig(path); prev() }
	}
	return spec, cleanup, nil
}

// grade runs the task-success grader plus the tool-use grader for every arm
// that received the code-intel capability profile.
func (d *Driver) grade(ctx context.Context, c Case, tr Transcript, useCodeIntel bool) []GradeResult {
	var grades []GradeResult
	if c.Family() == TaskRefactorAcrossFiles {
		grades = append(grades, NewRubricGrader(d.cfg.Judge).Grade(ctx, c, tr))
	} else if g := TaskGraderFor(c.Family()); g != nil {
		grades = append(grades, g.Grade(ctx, c, tr))
	}
	if useCodeIntel {
		grades = append(grades, NewToolUseGrader().Grade(ctx, c, tr))
	}
	return grades
}

// provision clones the case repo and checks out its pinned ref into a fresh
// workarea via the existing worktree.Manager (no new provisioning invented).
func (d *Driver) provision(ctx context.Context, c Case, arm Arm, trial int) (string, string, error) {
	src := d.cfg.RepoRoots[c.Input.Repo]
	if strings.TrimSpace(src) == "" {
		return "", "", fmt.Errorf("no RepoRoots entry for repo %q (configure --repo-root %s=<path>)", c.Input.Repo, c.Input.Repo)
	}
	sessionID := fmt.Sprintf("%s-%s-t%d-%s", sanitizeLeaf(c.ID), arm, trial, newID("wa")[:6])
	wa, err := d.wm.Provision(ctx, worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: src, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		return "", "", fmt.Errorf("provision clone: %w", err)
	}
	if err := checkoutRef(ctx, wa, c.Input.Ref); err != nil {
		_ = d.wm.Teardown(context.Background(), sessionID)
		return "", "", fmt.Errorf("checkout %s: %w", c.Input.Ref, err)
	}
	return wa, sessionID, nil
}

// checkoutRef detaches the workarea at the pinned ref so the ground truth is
// evaluated at exactly the SHA it was derived from.
func checkoutRef(ctx context.Context, dir, ref string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "--detach", "--quiet", ref) // nolint:gosec // ref is a pinned SHA from the validated benchmark.
	cmd.Env = runtimeenv.FilterRunnerOnly(cmd.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── aggregation ──────────────────────────────────────────────────────────────

// taskSuccessPass returns the pass of the task-success grader (the first
// non-tool-use grader), which is what the A/B delta is computed over.
func taskSuccessPass(grades []GradeResult) bool {
	for _, g := range grades {
		if g.GraderID != GraderToolUse {
			return g.Pass
		}
	}
	return false
}

func accumulate(f *FamilyStat, rec RunRecord) {
	// A zero token total means the trial's usage capture FAILED (no real LLM
	// trial costs zero tokens) — record NO token sample rather than a fake
	// zero-cost one: the per-family guard excludes a 0 median, but the pooled
	// aggregate median would count it as a genuinely cheap trial and bias the
	// efficiency bar toward PASS. Success/pass counting is unaffected; the
	// token-coverage power precondition (tokenCoverageShortfalls) surfaces
	// families whose samples all went missing.
	total := rec.Envelope.Trace.TokenCounts.Total()
	if rec.Arm == ArmWith {
		f.WithTrials++
		if total > 0 {
			f.WithTokens = append(f.WithTokens, total)
		}
		if rec.Pass {
			f.WithPasses++
		}
		for _, g := range rec.Grades {
			if g.GraderID == GraderToolUse {
				if adopted, _ := g.Metadata["adopted"].(bool); adopted {
					f.Adoption++
				}
			}
		}
	} else {
		f.WithoutTrials++
		if total > 0 {
			f.WithoutTokens = append(f.WithoutTokens, total)
		}
		if rec.Pass {
			f.WithoutPasses++
		}
	}
}

func computeAggregate(fams map[TaskType]*FamilyStat, records []RunRecord, cases []Case, trials int) AggregateStat {
	var withP, withT, woP, woT int
	var withTok, woTok []int64
	var regressed, tokenRegressed []TaskType
	for _, fam := range families {
		f := fams[fam]
		if f == nil {
			continue
		}
		withP += f.WithPasses
		withT += f.WithTrials
		woP += f.WithoutPasses
		woT += f.WithoutTrials
		withTok = append(withTok, f.WithTokens...)
		woTok = append(woTok, f.WithoutTokens...)
		if f.WithTrials > 0 && f.WithoutTrials > 0 {
			if f.WithRate() < f.WithoutRate() {
				regressed = append(regressed, fam)
			}
			// Per-family token guard: a TokenRatio of 0 means no WITHOUT-arm
			// token data — exclude the family rather than pass/fail on garbage
			// (same missing-data convention as the regression check above).
			if r := f.TokenRatio(); r > 0 && r > maxFamilyTokenRatio {
				tokenRegressed = append(tokenRegressed, fam)
			}
		}
	}
	deltaPP := (rate(withP, withT) - rate(woP, woT)) * 100
	tokenRatio := 0.0
	if m := medianI64(woTok); m > 0 {
		tokenRatio = float64(medianI64(withTok)) / float64(m)
	}
	famCounts := CountByFamily(cases)
	repoCounts := CountByRepo(cases)
	shortfalls := powerShortfalls(famCounts, repoCounts, trials)
	shortfalls = append(shortfalls, tokenCoverageShortfalls(fams)...)
	ciLow, ciHigh, stddev := bootstrapDeltaCI(caseAggsFromRecords(records))
	agg := AggregateStat{
		DeltaPP:                deltaPP,
		TokenRatio:             tokenRatio,
		AdoptionRate:           rate(withPassesAdopt(fams), withT),
		RegressedFamilies:      regressed,
		TokenRegressedFamilies: tokenRegressed,
		Trials:                 trials,
		FamilyCounts:           famCounts,
		RepoCounts:             repoCounts,
		Underpowered:           len(shortfalls) > 0,
		PowerShortfalls:        shortfalls,
		DeltaCILow:             ciLow,
		DeltaCIHigh:            ciHigh,
		DeltaStdDev:            stddev,
	}
	// The Q1v2 efficiency bar (see the constant block above): aggregate token
	// win, no family paying >+10%, no success regression. A tokenRatio of 0
	// means NO token data — that cannot clear a bar about token cost (kept as
	// defense-in-depth even though the token-coverage precondition already
	// forces such a run UNDERPOWERED). The success delta (and its bootstrap
	// CI) is informational, not gating.
	effBar := tokenRatio > 0 && tokenRatio <= maxAggregateTokenRatio &&
		len(tokenRegressed) == 0 && len(regressed) == 0
	// The power preconditions HARD-GATE the verdict: a null/underpowered result
	// can never be reported as a GA pass, no matter how favorable the ratios.
	agg.MeetsThreshold = effBar && !agg.Underpowered
	switch {
	case agg.Underpowered:
		agg.Status = "UNDERPOWERED — not a GA verdict (" + strings.Join(shortfalls, "; ") + ")"
	case agg.MeetsThreshold:
		agg.Status = "GA-PASS (efficiency bar: aggregate tokens<=1.0x, per-family <=1.10x, no success regression)"
	default:
		agg.Status = "GA-FAIL (powered, but did not clear the efficiency bar: " + strings.Join(efficiencyFailures(tokenRatio, tokenRegressed, regressed), "; ") + ")"
	}
	return agg
}

// efficiencyFailures enumerates which efficiency-bar conditions a powered run
// missed, for the GA-FAIL status line.
func efficiencyFailures(tokenRatio float64, tokenRegressed, regressed []TaskType) []string {
	var out []string
	switch {
	case tokenRatio <= 0:
		out = append(out, "no token data")
	case tokenRatio > maxAggregateTokenRatio:
		out = append(out, fmt.Sprintf("aggregate tokenRatio %.2fx > %.2fx", tokenRatio, maxAggregateTokenRatio))
	}
	if len(tokenRegressed) > 0 {
		out = append(out, fmt.Sprintf("family tokenRatio > %.2fx: %v", maxFamilyTokenRatio, tokenRegressed))
	}
	if len(regressed) > 0 {
		out = append(out, fmt.Sprintf("success regression: %v", regressed))
	}
	return out
}

// caseAgg is one benchmark case's pooled A/B tally (summed across its trials) —
// the resampling unit for the case-clustered bootstrap.
type caseAgg struct {
	withPasses, withTrials int
	woPasses, woTrials     int
}

// caseAggsFromRecords folds the per-(case,arm,trial) records into per-case
// tallies, preserving first-seen order.
func caseAggsFromRecords(records []RunRecord) []caseAgg {
	idx := map[string]int{}
	var out []caseAgg
	for _, r := range records {
		i, ok := idx[r.CaseID]
		if !ok {
			i = len(out)
			idx[r.CaseID] = i
			out = append(out, caseAgg{})
		}
		if r.Arm == ArmWith {
			out[i].withTrials++
			if r.Pass {
				out[i].withPasses++
			}
		} else {
			out[i].woTrials++
			if r.Pass {
				out[i].woPasses++
			}
		}
	}
	return out
}

const (
	// bootstrapIters is the resample count for the delta CI — enough for a stable
	// 95% interval, cheap (no LLM/IO in the loop).
	bootstrapIters = 2000
	// bootstrapSeed fixes the RNG so a given corpus yields a REPRODUCIBLE CI — a
	// GA verdict must not wobble between runs of the same data.
	bootstrapSeed = 0x5EED5
)

// bootstrapDeltaCI returns the 95% confidence interval [lo, hi] and standard
// deviation of the aggregate WITH−WITHOUT success delta (percentage points) by
// resampling CASES with replacement (not trials). Clustering on cases is the
// correct unit: the >=3 trials of one case@sha are strongly correlated, so
// pooling them as independent Bernoulli understates the true variance.
func bootstrapDeltaCI(cases []caseAgg) (lo, hi, stddev float64) {
	n := len(cases)
	if n == 0 {
		return 0, 0, 0
	}
	rng := mrand.New(mrand.NewSource(bootstrapSeed)) // nolint:gosec // deterministic non-crypto RNG for a reproducible CI, not a security context.
	deltas := make([]float64, 0, bootstrapIters)
	var sum float64
	for it := 0; it < bootstrapIters; it++ {
		var wp, wt, op, ot int
		for k := 0; k < n; k++ {
			c := cases[rng.Intn(n)]
			wp += c.withPasses
			wt += c.withTrials
			op += c.woPasses
			ot += c.woTrials
		}
		d := (rate(wp, wt) - rate(op, ot)) * 100
		deltas = append(deltas, d)
		sum += d
	}
	sort.Float64s(deltas)
	lo = percentile(deltas, 2.5)
	hi = percentile(deltas, 97.5)
	mean := sum / float64(len(deltas))
	var ss float64
	for _, d := range deltas {
		ss += (d - mean) * (d - mean)
	}
	stddev = math.Sqrt(ss / float64(len(deltas)))
	return lo, hi, stddev
}

// percentile returns the p-th percentile (0-100) of a pre-sorted slice via
// nearest-rank.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p/100*float64(len(sorted)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// powerShortfalls enumerates which locked power preconditions the run misses:
// every family must carry >=minTasksPerFamily cases, >=minReposCovered repos
// must be present, and >=minTrialsForGA trials/arm must have run. An empty slice
// means the corpus is powered enough for a GA verdict.
func powerShortfalls(famCounts map[TaskType]int, repoCounts map[string]int, trials int) []string {
	var out []string
	for _, fam := range families {
		if n := famCounts[fam]; n < minTasksPerFamily {
			out = append(out, fmt.Sprintf("family %s has %d task(s), need >=%d", fam, n, minTasksPerFamily))
		}
	}
	if len(repoCounts) < minReposCovered {
		out = append(out, fmt.Sprintf("%d repo(s) covered, need >=%d", len(repoCounts), minReposCovered))
	}
	if trials < minTrialsForGA {
		out = append(out, fmt.Sprintf("%d trial(s)/arm, need >=%d", trials, minTrialsForGA))
	}
	return out
}

// tokenCoverageShortfalls enumerates executed families whose token medians
// are missing/zero on either arm — the token-coverage power precondition. The
// efficiency bar is a claim about token cost; accumulate drops zero-token
// (capture-failure) trials, so a family whose captures failed would otherwise
// silently shrink the claim to the families that happened to report usage. A
// family that ran trials but has no nonzero token median on an arm makes the
// run UNDERPOWERED — it can never PASS.
func tokenCoverageShortfalls(fams map[TaskType]*FamilyStat) []string {
	var out []string
	for _, fam := range families {
		f := fams[fam]
		if f == nil || (f.WithTrials == 0 && f.WithoutTrials == 0) {
			continue // family not executed at all — corpus power covers it
		}
		if medianI64(f.WithTokens) <= 0 {
			out = append(out, fmt.Sprintf("family %s has no token data on the WITH arm", fam))
		}
		if medianI64(f.WithoutTokens) <= 0 {
			out = append(out, fmt.Sprintf("family %s has no token data on the WITHOUT arm", fam))
		}
	}
	return out
}

func withPassesAdopt(fams map[TaskType]*FamilyStat) int {
	n := 0
	for _, f := range fams {
		n += f.Adoption
	}
	return n
}

// ── small helpers ────────────────────────────────────────────────────────────

func rate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func medianI64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}

// newID mints a short random id with a prefix (run-/trace-/wa-).
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// sanitizeLeaf makes a case id safe as a directory leaf.
func sanitizeLeaf(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r == ':' {
			return '-'
		}
		return r
	}, s)
}

// Summary renders a human-readable A/B report for the CLI.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "code-intel A/B eval — %d trial(s)/arm, advertise=%s\n", r.Trials, r.Advertise)
	fmt.Fprintf(&b, "%-22s %8s %8s %8s %10s %9s\n", "family", "WITH", "WITHOUT", "delta", "tokRatio", "adoption")
	for _, fam := range families {
		f := r.Families[fam]
		if f == nil {
			continue
		}
		fmt.Fprintf(&b, "%-22s %7.0f%% %7.0f%% %+7.0fpp %9.2fx %8.0f%%\n",
			fam, f.WithRate()*100, f.WithoutRate()*100, f.DeltaPP(), f.TokenRatio(), f.AdoptionRate()*100)
	}
	a := r.Aggregate
	fmt.Fprintf(&b, "%-22s %8s %8s %+7.0fpp %9.2fx %8.0f%%\n", "AGGREGATE", "", "", a.DeltaPP, a.TokenRatio, a.AdoptionRate*100)
	if len(a.RegressedFamilies) > 0 {
		fmt.Fprintf(&b, "regressed families (success): %v\n", a.RegressedFamilies)
	}
	if len(a.TokenRegressedFamilies) > 0 {
		fmt.Fprintf(&b, "regressed families (tokens > %.2fx): %v\n", maxFamilyTokenRatio, a.TokenRegressedFamilies)
	}
	fmt.Fprintf(&b, "aggregate delta 95%% CI: [%+.1f, %+.1f]pp (stddev %.1f, case-clustered bootstrap; informational — success-delta bar retired 2026-07-06)\n",
		a.DeltaCILow, a.DeltaCIHigh, a.DeltaStdDev)
	fmt.Fprintf(&b, "power: %d trial(s)/arm, families=%v, repos=%d (need >=%d tasks/family, >=%d repos, >=%d trials)\n",
		a.Trials, a.FamilyCounts, len(a.RepoCounts), minTasksPerFamily, minReposCovered, minTrialsForGA)
	fmt.Fprintf(&b, "efficiency-threshold (aggregate tokens<=%.1fx, per-family <=%.2fx, no success regression): %v\n",
		maxAggregateTokenRatio, maxFamilyTokenRatio, a.MeetsThreshold)
	fmt.Fprintf(&b, "verdict: %s\n", a.Status)
	if r.PostedCount > 0 || r.PostErrors > 0 {
		fmt.Fprintf(&b, "bridge: posted=%d errors=%d\n", r.PostedCount, r.PostErrors)
	}
	return b.String()
}
