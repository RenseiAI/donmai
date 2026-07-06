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
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

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
	// Reporting context for the eval_runs rows.
	OrgID, ProjectID, DatasetID, DatasetName string
	// KeepWorkareas leaves provisioned workareas on disk (debugging).
	KeepWorkareas bool
	// Logf receives progress lines (nil → discard).
	Logf func(string, ...any)
}

// Driver is the two-arm A/B eval orchestrator.
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
	CaseID   string         `json:"caseId"`
	Family   TaskType       `json:"family"`
	Repo     string         `json:"repo"`
	Arm      Arm            `json:"arm"`
	Trial    int            `json:"trial"`
	Pass     bool           `json:"pass"`
	Grades   []GradeResult  `json:"grades"`
	Envelope ReportEnvelope `json:"envelope"`
	Posted   bool           `json:"posted"`
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

// Locked founder-threshold power preconditions (brief 06 §4.5): the >=+15pp
// verdict is only a GA claim over >=8-12 tasks/family x 2 repos x >=3 trials/arm.
// An underpowered or --task-filtered run must never report a PASS.
const (
	// minTasksPerFamily is the floor of the brief's 8-12 tasks/family band.
	minTasksPerFamily = 8
	// minReposCovered is the brief's "x 2 repos" (platform TS + donmai Go).
	minReposCovered = 2
	// minTrialsForGA is the brief's ">=3 trials/arm" to average out LLM nondeterminism.
	minTrialsForGA = 3
)

// AggregateStat is the whole-benchmark rollup + the founder-threshold verdict.
type AggregateStat struct {
	DeltaPP           float64    `json:"deltaPP"`
	TokenRatio        float64    `json:"tokenRatio"`
	AdoptionRate      float64    `json:"adoptionRate"`
	RegressedFamilies []TaskType `json:"regressedFamilies"`
	// Trials / FamilyCounts / RepoCounts capture the corpus power actually run,
	// computed from the executed cases — not assumed.
	Trials       int              `json:"trials"`
	FamilyCounts map[TaskType]int `json:"familyCounts"`
	RepoCounts   map[string]int   `json:"repoCounts"`
	// Underpowered is true when the run does not meet the locked power
	// preconditions (>=8 tasks/family across all four families, >=2 repos, >=3
	// trials). PowerShortfalls enumerates exactly which preconditions failed.
	Underpowered    bool     `json:"underpowered"`
	PowerShortfalls []string `json:"powerShortfalls,omitempty"`
	// DeltaCILow/High are the 95% confidence interval of the aggregate delta from
	// a CASE-CLUSTERED bootstrap (resampling whole cases, not trials, since the
	// >=3 trials of one case@sha are strongly correlated — pooling them as
	// independent Bernoulli understates variance). DeltaStdDev is the bootstrap
	// standard deviation. The founder verdict gates on DeltaCILow (the lower
	// bound), NOT the point estimate, so a borderline +15pp with a wide interval
	// cannot clear a bar meant to justify a permanent badge removal.
	DeltaCILow  float64 `json:"deltaCiLow"`
	DeltaCIHigh float64 `json:"deltaCiHigh"`
	DeltaStdDev float64 `json:"deltaStdDev"`
	// Status is the human-legible verdict category: "UNDERPOWERED — not a GA
	// verdict", "GA-PASS …", or "GA-FAIL …".
	Status string `json:"status"`
	// MeetsThreshold reflects the LOCKED founder bar: >=+15pp aggregate delta,
	// no per-family regression, median tokens <=+10% — AND the power
	// preconditions above. It can only be true on a statistically-powered run;
	// an underpowered run is forced to false regardless of the point estimate.
	MeetsThreshold bool `json:"meetsThreshold"`
}

// Run executes the full A/B matrix over cases and returns the Report.
func (d *Driver) Run(ctx context.Context, cases []Case) (Report, error) {
	rep := Report{Trials: d.cfg.Trials, Advertise: d.cfg.Advertise, Families: map[TaskType]*FamilyStat{}}
	for _, c := range cases {
		fam := rep.Families[c.Family()]
		if fam == nil {
			fam = &FamilyStat{}
			rep.Families[c.Family()] = fam
		}
		for trial := 1; trial <= d.cfg.Trials; trial++ {
			for _, arm := range []Arm{ArmWithout, ArmWith} {
				rec, err := d.runOne(ctx, c, arm, trial)
				if err != nil {
					return rep, fmt.Errorf("case %s arm %s trial %d: %w", c.ID, arm, trial, err)
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
		}
	}
	rep.Aggregate = computeAggregate(rep.Families, rep.Records, cases, d.cfg.Trials)
	return rep, nil
}

// runOne provisions one workarea, executes one arm, grades, posts, and tears down.
func (d *Driver) runOne(ctx context.Context, c Case, arm Arm, trial int) (RunRecord, error) {
	wa, sessionID, err := d.provision(ctx, c, arm, trial)
	if err != nil {
		return RunRecord{}, err
	}
	if !d.cfg.KeepWorkareas {
		defer func() { _ = d.wm.Teardown(context.Background(), sessionID) }()
	}
	d.cfg.Logf("provisioned %s arm=%s trial=%d at %s", c.ID, arm, trial, wa)

	spec, cleanup, err := d.buildArmSpec(ctx, c, arm, wa, sessionID)
	if err != nil {
		return RunRecord{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	tr, err := d.cfg.Executor.Execute(ctx, spec)
	if err != nil {
		return RunRecord{}, fmt.Errorf("execute: %w", err)
	}

	grades := d.grade(ctx, c, tr)
	pass := taskSuccessPass(grades)

	meta := ReportMeta{
		CaseID: c.ID, Arm: arm, Family: string(c.Family()), Repo: c.Input.Repo, Ref: c.Input.Ref,
		Trial: trial, Advertisement: string(d.cfg.Advertise), DatasetName: d.cfg.DatasetName,
	}
	env, err := BuildEnvelope(newID("run"), newID("trace"), sessionID, d.cfg.OrgID, d.cfg.ProjectID, d.cfg.DatasetID, c, tr, grades, meta)
	if err != nil {
		return RunRecord{}, err
	}

	rec := RunRecord{CaseID: c.ID, Family: c.Family(), Repo: c.Input.Repo, Arm: arm, Trial: trial, Pass: pass, Grades: grades, Envelope: env}
	if d.cfg.Bridge != nil {
		// The wire contract is the platform's flat per-trial /api/evals/ingest
		// body (the route runs the registered graders inline). The local
		// ReportEnvelope above stays the harness's canonical eval_runs+eval_traces
		// capture (dumped by --dry and used for the offline record).
		ingest := BuildIngestRequest(c, tr, trial, sessionID, d.cfg.DatasetID, d.cfg.ProjectID)
		resp, perr := d.cfg.Bridge.Post(ctx, ingest)
		if perr != nil {
			rec.PostErr = perr.Error()
			d.cfg.Logf("bridge post failed for %s/%s: %v", c.ID, arm, perr)
		}
		if resp != nil {
			rec.Posted = true
			rec.PostedRunID = resp.RunID
			d.cfg.Logf("posted %s/%s → eval_run %s (platform graders: %v)", c.ID, arm, resp.RunID, resp.GradersRun)
		}
	}
	return rec, nil
}

// buildArmSpec assembles the per-arm env + advertisement + MCP config.
func (d *Driver) buildArmSpec(ctx context.Context, c Case, arm Arm, wa, _ string) (ArmSpec, func(), error) {
	base := os.Environ()
	spec := ArmSpec{
		Arm: arm, Case: c, Workarea: wa, DonmaiBin: d.cfg.DonmaiBin,
		Budget: d.cfg.Budget, AdvertiseMode: d.cfg.Advertise, SnapshotID: workareaLeaf(wa),
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
	if arm == ArmWithout {
		// Control: the neutral base — same tools as WITH, minus donmai.
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
	spec.PromptSuffix = suffix
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

// grade runs the task-success grader (+ tool-use grader on the WITH arm).
func (d *Driver) grade(ctx context.Context, c Case, tr Transcript) []GradeResult {
	var grades []GradeResult
	if c.Family() == TaskRefactorAcrossFiles {
		grades = append(grades, NewRubricGrader(d.cfg.Judge).Grade(ctx, c, tr))
	} else if g := TaskGraderFor(c.Family()); g != nil {
		grades = append(grades, g.Grade(ctx, c, tr))
	}
	if tr.Arm == ArmWith {
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
	total := rec.Envelope.Trace.TokenCounts.Total()
	if rec.Arm == ArmWith {
		f.WithTrials++
		f.WithTokens = append(f.WithTokens, total)
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
		f.WithoutTokens = append(f.WithoutTokens, total)
		if rec.Pass {
			f.WithoutPasses++
		}
	}
}

func computeAggregate(fams map[TaskType]*FamilyStat, records []RunRecord, cases []Case, trials int) AggregateStat {
	var withP, withT, woP, woT int
	var withTok, woTok []int64
	var regressed []TaskType
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
		if f.WithTrials > 0 && f.WithoutTrials > 0 && f.WithRate() < f.WithoutRate() {
			regressed = append(regressed, fam)
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
	ciLow, ciHigh, stddev := bootstrapDeltaCI(caseAggsFromRecords(records))
	agg := AggregateStat{
		DeltaPP:           deltaPP,
		TokenRatio:        tokenRatio,
		AdoptionRate:      rate(withPassesAdopt(fams), withT),
		RegressedFamilies: regressed,
		Trials:            trials,
		FamilyCounts:      famCounts,
		RepoCounts:        repoCounts,
		Underpowered:      len(shortfalls) > 0,
		PowerShortfalls:   shortfalls,
		DeltaCILow:        ciLow,
		DeltaCIHigh:       ciHigh,
		DeltaStdDev:       stddev,
	}
	// Gate on the CI LOWER BOUND (not the point estimate): a borderline +15pp
	// whose interval dips below +15pp is noise, not a GA-grade positive delta.
	statBar := ciLow >= 15.0 && len(regressed) == 0 && tokenRatio > 0 && tokenRatio <= 1.10
	// The power preconditions HARD-GATE the verdict: a null/underpowered result
	// can never be reported as a GA pass, no matter how large the point estimate.
	agg.MeetsThreshold = statBar && !agg.Underpowered
	switch {
	case agg.Underpowered:
		agg.Status = "UNDERPOWERED — not a GA verdict (" + strings.Join(shortfalls, "; ") + ")"
	case agg.MeetsThreshold:
		agg.Status = "GA-PASS (95% CI lower bound >=+15pp, no regression, tokens<=+10%)"
	default:
		agg.Status = "GA-FAIL (powered, but did not clear the founder bar)"
	}
	return agg
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
		fmt.Fprintf(&b, "regressed families: %v\n", a.RegressedFamilies)
	}
	fmt.Fprintf(&b, "aggregate delta 95%% CI: [%+.1f, %+.1f]pp (stddev %.1f, case-clustered bootstrap)\n",
		a.DeltaCILow, a.DeltaCIHigh, a.DeltaStdDev)
	fmt.Fprintf(&b, "power: %d trial(s)/arm, families=%v, repos=%d (need >=%d tasks/family, >=%d repos, >=%d trials)\n",
		a.Trials, a.FamilyCounts, len(a.RepoCounts), minTasksPerFamily, minReposCovered, minTrialsForGA)
	fmt.Fprintf(&b, "founder-threshold (95%% CI lower bound >=+15pp, no regression, tokens<=+10%%): %v\n", a.MeetsThreshold)
	fmt.Fprintf(&b, "verdict: %s\n", a.Status)
	if r.PostedCount > 0 || r.PostErrors > 0 {
		fmt.Fprintf(&b, "bridge: posted=%d errors=%d\n", r.PostedCount, r.PostErrors)
	}
	return b.String()
}
