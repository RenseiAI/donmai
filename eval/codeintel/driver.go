package codeintel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	return &Driver{cfg: cfg, wm: wm, ad: NewAdvertisement(cfg.Advertise)}, nil
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

// AggregateStat is the whole-benchmark rollup + the founder-threshold verdict.
type AggregateStat struct {
	DeltaPP           float64    `json:"deltaPP"`
	TokenRatio        float64    `json:"tokenRatio"`
	AdoptionRate      float64    `json:"adoptionRate"`
	RegressedFamilies []TaskType `json:"regressedFamilies"`
	// MeetsThreshold reflects the LOCKED founder bar: >=+15pp aggregate delta,
	// no per-family regression, median tokens <=+10%. Only meaningful on a full
	// statistically-powered run (>=3 trials); a plumbing run reports it but it is
	// not a GA claim.
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
	rep.Aggregate = computeAggregate(rep.Families)
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
	if arm == ArmWithout {
		// Control: strip donmai from PATH (mandatory guard).
		spec.Env, _ = ScrubBinaryFromEnv(base, "donmai")
		return spec, nil, nil
	}
	// Treatment: make donmai reachable (prepend its dir) and attach the advertisement.
	withEnv := PrependPath(base, filepath.Dir(d.cfg.DonmaiBin))
	spec.Env = withEnv
	servers, suffix, err := d.ad.Apply(ctx, d.cfg.DonmaiBin, wa, c.Input.RepoPath, withEnv)
	if err != nil {
		return ArmSpec{}, nil, fmt.Errorf("advertise: %w", err)
	}
	spec.MCPServers = servers
	spec.PromptSuffix = suffix
	spec.AdvertisedTools = d.ad.AdvertisedToolNames()
	var cleanup func()
	if len(servers) > 0 {
		path, werr := clijsonl.WriteMCPConfig(servers)
		if werr != nil {
			return ArmSpec{}, nil, fmt.Errorf("write mcp config: %w", werr)
		}
		spec.MCPConfigPath = path
		cleanup = func() { _ = clijsonl.RemoveMCPConfig(path) }
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

func computeAggregate(fams map[TaskType]*FamilyStat) AggregateStat {
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
	agg := AggregateStat{
		DeltaPP:           deltaPP,
		TokenRatio:        tokenRatio,
		AdoptionRate:      rate(withPassesAdopt(fams), withT),
		RegressedFamilies: regressed,
	}
	agg.MeetsThreshold = deltaPP >= 15.0 && len(regressed) == 0 && tokenRatio > 0 && tokenRatio <= 1.10
	return agg
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
	fmt.Fprintf(&b, "founder-threshold (>=+15pp, no regression, tokens<=+10%%): %v\n", a.MeetsThreshold)
	if r.PostedCount > 0 || r.PostErrors > 0 {
		fmt.Fprintf(&b, "bridge: posted=%d errors=%d\n", r.PostedCount, r.PostErrors)
	}
	return b.String()
}
