package codesurvival

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// resultPostTimeout bounds the result POST. Mirrors the detached-timeout
// pattern in runner/routing_feedback.go.
const resultPostTimeout = 30 * time.Second

// Workarea is an acquired, isolated clone destination on the box's filesystem.
// The executor clones into Path and calls Release when done; Release(destroy)
// scrubs the credential by removing the worktree (the credential lives only in
// the clone URL / git remote config, never elsewhere).
type Workarea struct {
	// Path is the absolute directory the repo is cloned into.
	Path string
	// Release tears the workarea down (rm -rf), scrubbing the credential. Called
	// best-effort in a defer; must tolerate being called once.
	Release func()
}

// WorkareaProvider acquires an isolated workarea for a scan. The default
// provider uses os.MkdirTemp + os.RemoveAll; tests inject a stub.
type WorkareaProvider func() (Workarea, error)

// defaultWorkareaProvider creates a fresh temp dir under the OS temp root and
// returns a Release that removes it.
func defaultWorkareaProvider() (Workarea, error) {
	dir, err := os.MkdirTemp("", "code-survival-*")
	if err != nil {
		return Workarea{}, fmt.Errorf("codesurvival: acquire workarea: %w", err)
	}
	return Workarea{
		Path:    dir,
		Release: func() { _ = os.RemoveAll(dir) },
	}, nil
}

// Executor runs code-survival scans. Build via NewExecutor. Safe for concurrent
// use across batch items: every method holds only per-call state.
type Executor struct {
	git           gitRunner
	workareas     WorkareaProvider
	httpClient    *http.Client
	logger        *slog.Logger
	workerVersion string
	// poolProviderID stamps ScanExecutorInfo.poolProviderId (e.g. "e2b",
	// "local"). Empty falls back to "unknown".
	poolProviderID string
	// tsRunner runs the baked ts-morph reachability subprocess (RW4). Defaults to
	// execTSRunner; tests inject a golden-fixture runner.
	tsRunner tsRunner
	// wCold is the soft down-weight applied to cold surviving lines (RW4). 0
	// falls back to defaultWCold (0.25). A future batch-item override threads
	// through here.
	wCold float64
	// goReachability lets tests stub the (expensive, filesystem-bound) Go pass.
	// nil falls back to analyzeGoReachability.
	goReachability func(ctx context.Context, log *slog.Logger, repoPath string, survivingByFile map[string][]int) reachabilityResult
	// now is injectable for deterministic tests.
	now func() time.Time
}

// Options configures NewExecutor.
type Options struct {
	// GitRunner overrides the default exec.CommandContext("git", …) runner.
	GitRunner gitRunner
	// WorkareaProvider overrides the default temp-dir provider.
	WorkareaProvider WorkareaProvider
	// HTTPClient overrides http.DefaultClient for the result POST.
	HTTPClient *http.Client
	// Logger overrides slog.Default().
	Logger *slog.Logger
	// WorkerVersion stamps ScanExecutorInfo.workerVersion.
	WorkerVersion string
	// PoolProviderID stamps ScanExecutorInfo.poolProviderId.
	PoolProviderID string
	// TSRunner overrides the default ts-morph subprocess runner (RW4 tests).
	TSRunner tsRunner
	// WCold overrides the default cold-line down-weight (0.25).
	WCold float64
	// GoReachability overrides the default Go reachability pass (RW4 tests).
	GoReachability func(ctx context.Context, log *slog.Logger, repoPath string, survivingByFile map[string][]int) reachabilityResult
}

// NewExecutor constructs an Executor, filling in production defaults for any
// nil collaborator.
func NewExecutor(opts Options) *Executor {
	e := &Executor{
		git:            opts.GitRunner,
		workareas:      opts.WorkareaProvider,
		httpClient:     opts.HTTPClient,
		logger:         opts.Logger,
		workerVersion:  opts.WorkerVersion,
		poolProviderID: opts.PoolProviderID,
		tsRunner:       opts.TSRunner,
		wCold:          opts.WCold,
		goReachability: opts.GoReachability,
		now:            time.Now,
	}
	if e.git == nil {
		e.git = execGitRunner{}
	}
	if e.workareas == nil {
		e.workareas = defaultWorkareaProvider
	}
	if e.httpClient == nil {
		e.httpClient = http.DefaultClient
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.workerVersion == "" {
		e.workerVersion = "dev"
	}
	if e.poolProviderID == "" {
		e.poolProviderID = "unknown"
	}
	if e.tsRunner == nil {
		e.tsRunner = execTSRunner{}
	}
	if e.wCold <= 0 {
		e.wCold = defaultWCold
	}
	if e.goReachability == nil {
		e.goReachability = analyzeGoReachability
	}
	return e
}

// Handle is the batch-handler entry point for a code-survival-scan item. It is
// best-effort by contract: every failure logs + reports a status:"failed" (or
// "skipped") result and returns nil so the poll loop NEVER crashes and NEVER
// touches the agent path. It returns a non-nil error only when the work item is
// rejected before any work begins (org-claim mismatch / unknown contract
// version) so the poll loop can log the rejection — the loop still continues.
func (e *Executor) Handle(ctx context.Context, item BatchWorkItem) error {
	log := e.logger.With(
		"batchJobId", item.BatchJobID,
		"workType", item.WorkType,
		"attributionId", item.AttributionID,
		"checkpoint", item.Checkpoint,
		"prRepo", item.PRRepo,
		"prNumber", item.PRNumber,
	)

	// Reject an unknown contract major before doing any work.
	if item.ContractVersion != CodeSurvivalContractVersion {
		log.Warn("code-survival: contract version mismatch; rejecting",
			"itemVersion", item.ContractVersion, "workerVersion", CodeSurvivalContractVersion)
		return fmt.Errorf("codesurvival: unsupported contract version %d (worker speaks %d)",
			item.ContractVersion, CodeSurvivalContractVersion)
	}

	// (a) Re-verify the JWT org claim (cross-tenant guard). Reject + audit on
	// mismatch; never clone.
	if err := verifyOrgClaim(item.ResultAuth, item.OrgID); err != nil {
		log.Warn("code-survival: org claim re-verification failed; rejecting (cross-tenant guard)",
			"orgId", item.OrgID, "err", err)
		return fmt.Errorf("codesurvival: org claim rejected: %w", err)
	}

	res := e.scan(ctx, log, item)

	// (e) POST the result. Best-effort — a POST failure does not crash the loop.
	if err := e.postResult(ctx, log, item, res); err != nil {
		log.Warn("code-survival: result POST failed (non-fatal)", "err", err)
	}
	return nil
}

// scan acquires a workarea, clones to the merge SHA, runs the survival blame
// pass, and builds the result. It never returns an error — any failure is
// folded into a status:"failed"/"skipped" result so the caller can always POST
// something. The credential is scrubbed via the workarea Release defer.
func (e *Executor) scan(ctx context.Context, log *slog.Logger, item BatchWorkItem) CodeSurvivalScanResult {
	base := e.baseResult(item)

	// (b) Acquire workarea + clone the repo deep enough to reach mergeSha.
	wa, err := e.workareas()
	if err != nil {
		log.Warn("code-survival: workarea acquire failed", "err", err)
		base.Status = StatusFailed
		return base
	}
	// (f) Scrub credential + release workarea on every exit path.
	defer wa.Release()

	if item.GitCredential.CloneURL == "" {
		log.Warn("code-survival: missing clone url; skipping", "skipReason", SkipRepoGone)
		base.Status = StatusSkipped
		base.SkipReason = ptr(SkipRepoGone)
		return base
	}

	if err := e.clone(ctx, wa.Path, item.GitCredential.CloneURL); err != nil {
		// Clone failure: repo deleted / access revoked / token expired.
		log.Warn("code-survival: clone failed; skipping", "skipReason", SkipRepoGone, "err", scrub(err.Error(), item.GitCredential))
		base.Status = StatusSkipped
		base.SkipReason = ptr(SkipRepoGone)
		return base
	}

	// Scrub the credential from the cloned remote so it never lingers on disk
	// for the duration of the (potentially slow) blame pass. The injected token
	// lives in the remote URL written by `git clone`; rewrite it to the bare
	// HTTPS URL. Best-effort — Release() still rm -rf's the whole tree.
	e.scrubRemoteCredential(ctx, wa.Path, item.PRRepo)

	// Verify the clone can actually reach mergeSha. A force-push / rewrite /
	// too-shallow clone trips shallow_history.
	if _, err := e.git.run(ctx, wa.Path, "cat-file", "-e", item.MergeSha+"^{commit}"); err != nil {
		log.Warn("code-survival: merge sha unreachable; skipping",
			"skipReason", SkipShallowHistory, "mergeSha", item.MergeSha)
		base.Status = StatusSkipped
		base.SkipReason = ptr(SkipShallowHistory)
		return base
	}

	// Record HEAD for the result audit.
	if head, err := e.git.run(ctx, wa.Path, "rev-parse", "HEAD"); err == nil {
		base.HeadSha = strings.TrimSpace(head)
	}

	// (c) diff-tree + blame survival pass.
	scan := scanPrSurvival(ctx, e.git, wa.Path, item.MergeSha)
	if scan.diffTreeFailed {
		// diff-tree could not list the merge's files even though cat-file said
		// the commit exists — treat as shallow/rewritten history.
		log.Warn("code-survival: diff-tree failed; skipping",
			"skipReason", SkipShallowHistory, "err", scan.errorMessage)
		base.Status = StatusSkipped
		base.SkipReason = ptr(SkipShallowHistory)
		return base
	}

	// (d) compute survival rate via the ported computeSurvivalRate.
	rate := computeSurvivalRate(survivalRateInput{
		linesTotalAtMerge: scan.linesTotalAtMerge,
		linesSurviving:    scan.linesSurviving,
	})
	base.Survival = ScanSurvival{
		LinesTotalAtMerge: rate.linesTotalAtMerge,
		LinesSurviving:    rate.linesSurviving,
		SurvivalRatePct:   rate.survivalRatePct,
	}
	base.Status = scan.status

	// (RW4) Reachability / hot-path weighting. Runs ON TOP of survival, over the
	// SAME clone. NEVER hard-fails survival: any degradation collapses to
	// status:partial + hotWeighted=null with survival preserved intact.
	e.applyReachability(ctx, log, &base, wa.Path, scan.survivingByFile)

	log.Info("code-survival: scan complete",
		"status", base.Status,
		"linesTotalAtMerge", base.Survival.LinesTotalAtMerge,
		"linesSurviving", base.Survival.LinesSurviving,
		"filesScanned", len(scan.filesScanned),
		"filesMissing", len(scan.filesMissing),
		"hotWeighted", base.HotWeighted != nil,
		"perSymbol", len(base.PerSymbol),
	)
	return base
}

// applyReachability runs the per-language reachability passes over the clone,
// classifies the surviving lines, and populates base.HotWeighted + base.PerSymbol.
//
// Mixed-language PRs (both .go and .ts present) run BOTH passes; classification
// unions them per file. Graceful degradation contract:
//   - If survival already produced no surviving lines, there is nothing to weight
//     — leave hotWeighted=null/perSymbol=[] and do not downgrade an OK survival.
//   - If ANY relevant pass degraded (toolchain absent, timeout, crash, parse
//     fail, partial), the result is status:partial + hotWeighted=null +
//     perSymbol=[]. Survival counts are untouched.
//   - Reachability NEVER sets status:failed; survival owns hard-fail (RW3).
func (e *Executor) applyReachability(ctx context.Context, log *slog.Logger, base *CodeSurvivalScanResult, repoPath string, survivingByFile map[string][]int) {
	// Stamp toolchain versions (audit + drift detection) regardless of outcome.
	base.Executor.Toolchains.Go = goToolchainVersion()
	base.Executor.Toolchains.Node = nodeToolchainVersion(ctx)

	// Only the survival-skip/fail paths reach here without surviving lines, but
	// guard anyway: nothing to weight ⇒ leave reachability null.
	if len(survivingByFile) == 0 {
		return
	}

	goTargets := goFiles(survivingByFile)
	tsTargets := tsFiles(survivingByFile)
	if len(goTargets) == 0 && len(tsTargets) == 0 {
		// Surviving lines exist but in languages we don't analyse (e.g. .py, .rs).
		// Cannot classify ⇒ degrade to partial, hotWeighted=null (no down-weight).
		log.Info("code-survival: reachability skipped (unsupported languages only); partial")
		base.Status = StatusPartial
		return
	}

	var passes []reachabilityResult
	degraded := false

	if len(goTargets) > 0 {
		gr := e.goReachability(ctx, log, repoPath, survivingByFile)
		degraded = degraded || gr.partial
		passes = append(passes, gr)
	}
	if len(tsTargets) > 0 {
		tr := analyzeTSReachability(ctx, log, e.tsRunner, repoPath, survivingByFile)
		degraded = degraded || tr.partial
		passes = append(passes, tr)
	}

	if degraded {
		// Survival preserved; reachability is best-effort and degraded.
		log.Info("code-survival: reachability degraded; reporting partial with survival intact")
		base.Status = StatusPartial
		base.HotWeighted = nil
		base.PerSymbol = []ScanSymbolBreakdown{}
		return
	}

	cls := classifySurvivingLines(survivingByFile, passes...)
	hw := computeHotWeighted(cls, base.Survival.LinesTotalAtMerge, e.wCold)
	base.HotWeighted = &hw
	base.PerSymbol = cls.perSymbol
	if base.PerSymbol == nil {
		base.PerSymbol = []ScanSymbolBreakdown{}
	}
	// Reachability succeeded: a pure-survival OK stays OK (status already set by
	// the survival pass; reachability success never downgrades it).
}

// baseResult builds the result skeleton shared by every exit path: the contract
// version, echoed identifiers, RW3-null reachability fields, an empty (never
// nil) perSymbol slice, and executor info.
func (e *Executor) baseResult(item BatchWorkItem) CodeSurvivalScanResult {
	gitVer := ""
	if v, err := e.git.run(context.Background(), "", "--version"); err == nil {
		gitVer = strings.TrimSpace(strings.TrimPrefix(v, "git version "))
	}
	return CodeSurvivalScanResult{
		ContractVersion: CodeSurvivalContractVersion,
		AttributionID:   item.AttributionID,
		Checkpoint:      item.Checkpoint,
		MergeSha:        item.MergeSha,
		HeadSha:         "",
		Status:          StatusFailed, // overwritten on success/skip
		SkipReason:      nil,
		Survival:        ScanSurvival{},
		HotWeighted:     nil,                     // populated by RW4 applyReachability on success
		PerSymbol:       []ScanSymbolBreakdown{}, // never nil → serializes as []
		Provider:        nil,                     // not yet attributed
		WorkType:        nil,                     // not yet attributed
		Executor: ScanExecutorInfo{
			PoolProviderID: e.poolProviderID,
			WorkerVersion:  e.workerVersion,
			Toolchains:     ScanToolchains{Git: gitVer},
		},
	}
}

// clone clones cloneURL into dst deep enough to reach the merge SHA. It does
// NOT use --depth 1 (the merge SHA must be reachable; the merge parent's blame
// must resolve). Mirrors the in-box clone path: the credential is
// already injected into cloneURL by the platform at dispatch.
func (e *Executor) clone(ctx context.Context, dst, cloneURL string) error {
	// Full-history clone (no --depth) so `git blame <mergeSha>` and the merge's
	// ancestry resolve. A bandwidth optimisation (--shallow-since / partial
	// clone) is a future tuning knob; correctness first.
	if _, err := e.git.run(ctx, "", "clone", cloneURL, dst); err != nil {
		return err
	}
	return nil
}

// scrubRemoteCredential rewrites the origin remote URL to the credential-free
// HTTPS form so the injected token does not linger in .git/config during the
// blame pass. Best-effort — the workarea Release() rm -rf is the hard scrub.
func (e *Executor) scrubRemoteCredential(ctx context.Context, repoPath, prRepo string) {
	if prRepo == "" {
		return
	}
	bare := "https://github.com/" + prRepo + ".git"
	if _, err := e.git.run(ctx, repoPath, "remote", "set-url", "origin", bare); err != nil {
		e.logger.Debug("code-survival: remote credential scrub failed (non-fatal)", "err", err)
	}
}

// postResult POSTs the CodeSurvivalScanResult to item.ResultEndpoint with
// item.ResultAuth as the bearer token. Mirrors the detached-timeout, best-effort
// POST in runner/routing_feedback.go.
func (e *Executor) postResult(parentCtx context.Context, log *slog.Logger, item BatchWorkItem, res CodeSurvivalScanResult) error {
	if item.ResultEndpoint == "" {
		return errors.New("codesurvival: no result endpoint")
	}
	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), resultPostTimeout)
	defer cancel()
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, item.ResultEndpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(item.ResultAuth, "Bearer "))

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("non-2xx: %d", resp.StatusCode)
	}
	log.Debug("code-survival: result posted", "status", string(res.Status), "endpoint", item.ResultEndpoint)
	return nil
}

// ptr returns a pointer to v. Used for the nullable contract fields.
func ptr[T any](v T) *T { return &v }

// scrub removes the injected token from a log string so a clone error message
// never leaks the credential.
func scrub(s string, cred BatchWorkGitCredential) string {
	if cred.Token != "" {
		s = strings.ReplaceAll(s, cred.Token, "***")
	}
	if cred.CloneURL != "" {
		s = strings.ReplaceAll(s, cred.CloneURL, "***")
	}
	return s
}
