package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/internal/providerretry"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/activity"
	"github.com/RenseiAI/donmai/runtime/executionevent"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// kitLoadSkills is the seam used for Kit skill loading in the runner
// loop. It delegates to internal/kit.LoadSkills; isolated here so tests
// can verify integration without a real KitRegistry on disk.
var kitLoadSkills = kit.LoadSkills

// termCastPath returns the on-disk asciinema-v2 cast location for a
// session's workarea (spec § 16), next to events.jsonl in the
// state.AgentDirName convention. Single source of truth shared by the
// interactive spec builder below (which populates
// agent.InteractiveSpec.RecordPath when recording is allowed) and the
// end-of-session cleanup in interactive_loop.go's dispatchInteractive
// (which removes the file at this same path once the session reaches a
// terminal state).
func termCastPath(wpath string) string {
	return filepath.Join(wpath, state.AgentDirName, "term.cast")
}

func worktreeProvisionStrategy(qw QueuedWork) worktree.CloneStrategy {
	if qw.Repository == "" && qw.RepositoryDeclaration == nil && qw.CacheSeedID == "" && qw.WorkareaMode != worktree.ModeShared {
		return worktree.StrategyEmpty
	}
	return worktree.StrategyClone
}

// runLoop drives the per-session orchestration steps in F.1.1 §4
// order. Returns the in-progress Result (always non-nil) plus a
// terminal err the caller may surface.
//
// Step ordering matches the design doc verbatim:
//
//  1. Resolve provider
//  2. Provision worktree
//  3. Compose env (after credential injection — runner-side cred
//     resolution is a daemon responsibility today; this step takes
//     QueuedWork.AuthToken + ResolvedProfile.CredentialID as opaque)
//  4. Build MCP config
//  5. Render prompt
//  6. Translate to agent.Spec
//  7. Spawn provider
//  8. Start heartbeat pulser
//  9. Stream events
//  10. Wait for terminal event
//  11. Tail recovery (steering → backstop)
//     11b. Linear state transition — parse WORK_RESULT,
//     resolve target status from sdlc.go, post update via the
//     issue-tracker proxy. Failures recorded as PostSessionWarnings;
//     never fatal.
//  12. Build Result envelope
//
// The orchestration loop is long by design — splitting it further hides
// the step ordering that is the package's primary contract.
//
//nolint:gocyclo,funlen // intentional — see comment above.
func (r *Runner) runLoop(ctx context.Context, qw QueuedWork, startedAt int64, admission *HarnessAdmission) (*Result, error) {
	res := &Result{
		SessionID:       qw.SessionID,
		IssueIdentifier: qw.IssueIdentifier,
		StartedAt:       startedAt,
	}

	// 1. Resolve exactly one harness/provider pair before any posterior network
	// request, worktree creation, credential delivery, or provider spawn.
	selection, err := r.admittedHarnessSelection(ctx, qw, admission)
	if err != nil {
		err = attachDeniedHarnessReceipt(qw, err, r.now())
		res.Status = "failed"
		res.FailureMode = FailureProviderResolve
		res.Error = err.Error()
		var admissionErr *HarnessAdmissionError
		if errors.As(err, &admissionErr) {
			value := admissionErr.Receipt.Value()
			res.AdmissionReceipt = &value
			res.ResolverDecisions = append(res.ResolverDecisions, admissionErr.Decisions...)
		}
		return res, err
	}
	qw.runtimeTransport = selection.runtimeTransport
	if err := r.registry.validateRuntimeTransport(qw); err != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
		return res, err
	}
	provider := selection.Provider
	// Register the session's quota reporter before any harness side
	// effect: live quota updates stream from the first turn, and the
	// reporter must already be held so the consecutive-update dedup
	// sees every loop of the session. Released at the end of the run.
	r.registerQuotaReporter(qw.SessionID, provider)
	defer r.releaseQuotaReporter(qw.SessionID)
	// Refuse a stamped execution-security level this exact harness cannot
	// render in the session's mode before any workarea, credential or
	// provider side effect. The same function re-checks the final spec
	// before spawn (step 6b) and records the report.
	if _, err = executionSecurityReport(agent.Spec{ExecutionSecurity: qw.ExecutionSecurity, PromptMode: sessionPromptMode(qw, selection.effectiveCell)}, provider, nil); err != nil {
		refuseForExecutionSecurity(res, err)
		return res, err
	}
	repositoryDeclaration, provisionDeclaration, executorWorkareaCapabilities, workareaErr := resolveRepositoryWorkareaDeclaration(qw, provider)
	if workareaErr != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, workareaErr.Error()
		return res, workareaErr
	}
	var preparedPlan *agent.PreparedHarness
	var preparedSource agent.Spec
	codeIntelDelivery := codeIntelDeliverySelection{Route: codeIntelDeliveryLegacy}
	if len(selection.receipt.Bytes()) > 0 {
		preparedPlan, err = preparedHarnessFromWork(qw)
		if err != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
			return res, err
		}
		preparedSource, _, err = buildPreparedSourceSpec(qw, selection, r.additionalExtensionDecorator, r.preparedCapabilities)
		if err != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
			return res, err
		}
		preparedSource = ReconcileRepositorySandbox(preparedSource, repositoryDeclaration)
		preparedSource.PreparedHarness = preparedPlan
		codeIntelDelivery, err = codeIntelRouteFromSpec(qw.CodeIntel, preparedSource)
		if err != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
			return res, err
		}
		harness, ok := provider.(agent.HarnessProvider)
		if !ok {
			err = errors.New("runner: selected provider has no exact harness manifest")
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
			return res, err
		}
		if _, err = agent.ApplyPreparedHarness(preparedSource, harness.Manifest()); err != nil {
			if agent.ExecutionSecurityErrorCode(err) != "" {
				refuseForExecutionSecurity(res, err)
				return res, err
			}
			// A drift error names exactly which authority-projection fields
			// disagreed (never their values — see agent.AuthorityDriftError);
			// log it as a structured line so a production refusal is
			// diagnosable from the daemon/runner log alone, instead of a
			// bare 64-hex digest inequality nobody could triage.
			var driftErr *agent.AuthorityDriftError
			if errors.As(err, &driftErr) {
				r.logger.Error("prepared harness authority drift",
					"sessionId", qw.SessionID,
					"harness", selection.Harness.ID,
					"provider", provider.Name(),
					"driftFields", driftErr.Fields,
				)
			}
			// The AuthorityDigest projection agreeing does not mean the
			// recomputed ToolLifecycleReceipt does — see
			// agent.ToolLifecycleDriftError's doc comment (a Spec field the
			// tool/lifecycle compiler consumes but the projection does not
			// cover, e.g. Spec.AdditionalExtensions, changed after
			// preflight). Same doctrine as the AuthorityDriftError log
			// above: name exactly which receipt field(s) disagreed so this
			// self-check's own failure is diagnosable from the log alone,
			// not just the eventual spawn-time refusal
			// (runner.ReconcileAdditionalExtensions's doc comment records
			// the production incident this self-check exists to catch
			// early).
			var toolDriftErr *agent.ToolLifecycleDriftError
			if errors.As(err, &toolDriftErr) {
				r.logger.Error("prepared harness tool-lifecycle drift",
					"sessionId", qw.SessionID,
					"harness", selection.Harness.ID,
					"provider", provider.Name(),
					"driftFields", toolDriftErr.Fields,
				)
			}
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
			return res, err
		}
	}
	res.ProviderName = provider.Name()
	harnessRef := selection.Harness
	res.HarnessRef = &harnessRef
	res.ResolverDecisions = append(res.ResolverDecisions, selection.Decisions...)
	caps := provider.Capabilities()
	// The DECLARED notice-delivery mechanism for this harness, read off the
	// live manifest — never inferred from the harness's name, and never
	// assumed. A provider with no manifest leaves it empty, which every
	// consumer treats as "undeclared" and therefore as "do not deliver".
	noticeDelivery := agent.NoticeDelivery("")
	if hp, ok := provider.(agent.HarnessProvider); ok {
		noticeDelivery = hp.Manifest().Caps.NoticeDelivery
	}
	r.logger.Info("provider resolved",
		"sessionId", qw.SessionID,
		"harness", selection.Harness.ID,
		"provider", provider.Name(),
		"injection", caps.SupportsMessageInjection,
		"resume", caps.SupportsSessionResume,
		"noticeDelivery", declaredOrUndeclared(noticeDelivery),
	)

	// Log which dispatch path is in use
	// so operators can grep one session end-to-end through the
	// stage-vs-legacy fork. `mode=stage` means the platform's new
	// `agent.dispatch_stage` action queued this work and the runner is
	// using qw.StagePrompt verbatim; `mode=legacy` means the work came
	// in via `agent.dispatch_to_queue` and the embedded
	// per-work-type template is rendering the user prompt.
	stageMode := "legacy"
	if strings.TrimSpace(qw.StagePrompt) != "" {
		stageMode = "stage"
	}
	r.logger.Info("[runner-stage]",
		"sid", qw.SessionID,
		"stageId", qw.StageID,
		"mode", stageMode,
	)

	// Budget enforcement and the session's usage meter. The enforcer is
	// always constructed; when qw.StageBudget is nil (legacy path) it
	// enforces no cap, but still meters every turn for Result.Cost.
	enforcer := NewBudgetEnforcer(qw.StageBudget, time.UnixMilli(startedAt))
	enforcer.midTurnWrapUp = caps.SupportsMessageInjection && takesMidTurnWrapUp(noticeDelivery)
	if enforcer.Enabled() {
		subAgents := "unlimited"
		if qw.StageBudget.MaxSubAgents != nil {
			subAgents = fmt.Sprintf("%d", *qw.StageBudget.MaxSubAgents)
		}
		r.logger.Info("[runner-stage]",
			"sid", qw.SessionID,
			"stageId", qw.StageID,
			"event", "budget.enforce",
			"maxDurationSeconds", qw.StageBudget.MaxDurationSeconds,
			"maxSubAgents", subAgents,
			"maxTokens", qw.StageBudget.MaxTokens,
		)
	}

	// 2. Provision worktree. We clone at the remote default branch
	// (typically main) and create the per-session work branch on
	// top inside the worktree afterward — passing a non-existent
	// branch to `git clone --branch` fails because the upstream
	// reference does not yet exist.
	// Amend-existing-branch contract: when dispatch carries a base Ref, provision the
	// worktree AT that ref and retain it as the working/push branch. Validation
	// is strict (no empty, no ".." , no absolute) so a malformed ref cannot
	// escape the worktree. Trimmed Ref empty preserves legacy behaviour exactly.
	refBranch := ""
	if ref := trimRef(qw.Ref); ref != "" {
		if err := validateRef(ref); err != nil {
			res.Status = "failed"
			res.FailureMode = FailureWorktreeProvision
			res.Error = err.Error()
			return res, err
		}
		refBranch = ref
	}
	// Continue-mode contract: when the spec names a pull request to
	// continue, the run checks out the pull request's head branch at the
	// dispatched head commit — never a fresh agent/<session> branch. The
	// head branch is also the push target and the run's own pull request.
	if err := validateContinuePullRequest(qw.ContinuePullRequest); err != nil {
		res.Status = "failed"
		res.FailureMode = FailureWorktreeProvision
		res.Error = err.Error()
		return res, err
	}
	continueMode := qw.ContinuePullRequest != nil
	// The platform keeps sending the ref pin (older runners rely on it)
	// alongside the continued record, so a ref that names the continued
	// head branch is accepted; only a ref naming a DIFFERENT branch is a
	// typed refusal.
	if continueMode && refBranch != "" && !strings.EqualFold(refBranch, strings.TrimSpace(qw.ContinuePullRequest.HeadRef)) {
		err := errors.New("runner: continued pull request head ref differs from the dispatched amend ref")
		res.Status = "failed"
		res.FailureMode = FailureWorktreeProvision
		res.Error = err.Error()
		return res, err
	}
	if continueMode && (qw.RepositoryDeclaration != nil || qw.PullRequest != nil || qw.BaseRef != "") {
		err := errors.New("runner: continued pull request is mutually exclusive with repository declarations, dispatched pull requests, and base branches")
		res.Status = "failed"
		res.FailureMode = FailureWorktreeProvision
		res.Error = err.Error()
		return res, err
	}
	branch := qw.Branch
	switch {
	case continueMode:
		branch = continuePullRequestBranch(qw.ContinuePullRequest)
	case refBranch != "":
		branch = refBranch
	case branch == "":
		branch = "agent/" + qw.SessionID
	}
	provisionStrategy := worktreeProvisionStrategy(qw)
	repositoryFree := provisionStrategy == worktree.StrategyEmpty
	provisionBranch := refBranch
	provisionSourceRef := qw.Ref
	if qw.BaseRef != "" {
		provisionBranch = qw.BaseRef
		provisionSourceRef = qw.BaseRef
	}
	if repositoryFree {
		provisionBranch = ""
		provisionSourceRef = ""
	}
	wpath, err := r.wt.Provision(ctx, worktree.ProvisionSpec{
		SessionID:             qw.SessionID,
		RepoURL:               qw.Repository,
		Branch:                provisionBranch,
		BaseRef:               qw.BaseRef,
		RequireBranchBase:     qw.BaseRef != "",
		SourceRef:             provisionSourceRef,
		Strategy:              provisionStrategy,
		RepositoryDeclaration: provisionDeclaration,
		ExecutorCapabilities:  executorWorkareaCapabilities,
		Mode:                  qw.WorkareaMode,
		ParentWorkareaID:      qw.ParentWorkareaID,
		RepositoryFilter:      qw.RepositoryFilter,
		CacheSeedID:           qw.CacheSeedID,
		PullRequest:           qw.PullRequest,
	})
	if err != nil {
		res.Status = "failed"
		res.FailureMode = classifyWorktreeErr(err)
		res.Error = err.Error()
		return res, err
	}
	res.WorktreePath = wpath
	layout, layoutErr := r.wt.Layout(qw.SessionID)
	if layoutErr != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, layoutErr.Error()
		return res, layoutErr
	}
	res.WorkareaRoot = layout.Root.String()
	r.logger.Debug("worktree provisioned", "sessionId", qw.SessionID, "path", wpath, "workareaRoot", res.WorkareaRoot, "nested", layout.IsNested())
	var declaredRepositoryPaths map[string]string
	var skippedRepositories map[string]struct{}
	if repositoryDeclaration != nil {
		declaredRepositoryPaths, err = r.wt.RepositoryPaths(qw.SessionID)
		if err != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, err.Error()
			return res, err
		}
		// A declared read-only context repository whose clone failed is
		// skipped with a warning instead of failing the session: record
		// each skip on the result so it is visible wherever
		// PostSessionWarnings surface. The skipped set is also the only
		// thing that excuses a declared repository without a path when the
		// sandbox authority policy is built below.
		skipped, skippedErr := r.wt.SkippedRepositories(qw.SessionID)
		if skippedErr != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, skippedErr.Error()
			return res, skippedErr
		}
		skippedRepositories = make(map[string]struct{}, len(skipped))
		for _, name := range skipped {
			skippedRepositories[name] = struct{}{}
			warning := fmt.Sprintf("declared read-only context repository %q failed to clone; continuing without it", name)
			r.logger.Warn("declared repository skipped", "sessionId", qw.SessionID, "repository", name)
			res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
		}
	}
	runnerStatePath := wpath
	selectedRepositoryReadOnly := repositoryDeclaration != nil && repositoryDeclaration.Selected.Authority == workarea.RepositoryReadOnly
	if selectedRepositoryReadOnly {
		runnerStatePath = filepath.Join(res.WorkareaRoot, workarea.DeclarationDirName, "runner")
		root, err := os.OpenRoot(res.WorkareaRoot)
		if err != nil {
			return res, fmt.Errorf("open workarea root for runner state: %w", err)
		}
		if err := root.MkdirAll(filepath.Join(workarea.DeclarationDirName, "runner"), 0o700); err != nil {
			_ = root.Close()
			return res, fmt.Errorf("create root-owned runner state: %w", err)
		}
		stateInfo, err := root.Lstat(filepath.Join(workarea.DeclarationDirName, "runner"))
		_ = root.Close()
		if err != nil || !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 {
			return res, fmt.Errorf("root-owned runner state is unsafe")
		}
	}

	// Check out the continued pull request's head branch at the dispatched
	// head commit before the agent starts. The branch may exist only on the
	// remote, so this fetches it and checks it out at the pinned commit;
	// anything else fails the session rather than running on a moved head.
	if continueMode && !repositoryFree {
		if err := checkoutContinuePullRequest(ctx, wpath, qw.ContinuePullRequest); err != nil {
			res.Status = "failed"
			res.FailureMode = FailureWorktreeProvision
			res.Error = err.Error()
			return res, err
		}
		r.logger.Info("checked out continued pull request head", "branch", branch, "sessionId", qw.SessionID)
	}
	// Create the per-session work branch in the worktree (skipped when
	// provisioning at an existing ref — that ref IS the working branch).
	selectedRepositoryMutable := !repositoryFree && !selectedRepositoryReadOnly
	switch {
	case continueMode:
		r.logger.Info("continuing pull request head branch", "branch", branch, "sessionId", qw.SessionID)
	case repositoryFree:
		r.logger.Info("repository-free workarea provisioned without a git branch", "sessionId", qw.SessionID)
	case repositoryDeclaration != nil && refBranch == "":
		for _, repository := range repositoryDeclaration.Repositories {
			if repository.Authority != workarea.RepositoryMutable {
				continue
			}
			repositoryPath := declaredRepositoryPaths[repository.Name]
			if repositoryPath == "" {
				continue
			}
			if _, gitErr := runGit(ctx, repositoryPath, gitIdentity{}, "checkout", "-b", branch); gitErr != nil {
				r.logger.Debug("create declared repository work branch failed (may already exist)",
					"repository", repository.Name, "branch", branch, "err", gitErr)
			}
		}
	case refBranch == "" && selectedRepositoryMutable:
		if _, gerr := runGit(ctx, wpath, gitIdentity{}, "checkout", "-b", branch); gerr != nil {
			r.logger.Debug("create work branch failed (may already exist)",
				"branch", branch, "err", gerr)
		}
	case refBranch != "":
		r.logger.Info("provisioned at existing ref branch", "branch", branch, "sessionId", qw.SessionID)
	default:
		r.logger.Info("selected repository is read-only; runner branch creation omitted",
			"sessionId", qw.SessionID, "repository", repositoryDeclaration.Selected.Name)
	}

	// Record every mutable checkout with the commit it starts at, so teardown
	// can tell the session's own unpublished work from the base and preserve
	// it before deleting the workarea (Run → preserveUnpublishedWork).
	switch {
	case repositoryFree:
	case repositoryDeclaration != nil:
		targets := make([]rescueTarget, 0, len(repositoryDeclaration.Repositories))
		for _, repository := range repositoryDeclaration.Repositories {
			if repository.Authority == workarea.RepositoryMutable {
				if path := declaredRepositoryPaths[repository.Name]; path != "" {
					targets = append(targets, rescueTarget{name: repository.Name, path: path})
				}
			}
		}
		recordRescueTargets(ctx, res, targets)
	case selectedRepositoryMutable:
		recordRescueTargets(ctx, res, []rescueTarget{{path: wpath}})
	}

	if qw.BaseRef != "" {
		if err := verifyNewWorkBranch(ctx, wpath, branch); err != nil {
			res.Status = "failed"
			res.FailureMode = FailureWorktreeProvision
			res.Error = err.Error()
			return res, err
		}
	}

	// 2a-bis. Sibling context repositories (DONMAI_SIBLING_REPOS; see
	// siblings.go). An executor that can hold a declared read-only leaf got
	// each entry as a context leaf under the session root above; any other
	// executor keeps the original placement beside the session worktree. A
	// work item that declares its own repositories is authoritative, and the
	// variable is ignored for it. Never fatal.
	switch {
	case qw.RepositoryDeclaration != nil && siblingReposSpec(qw) != "":
		warning := siblingReposEnv + " is ignored: the work item declares its own repositories"
		r.logger.Warn(warning, "sessionId", qw.SessionID)
		res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
	case !repositoryFree && repositoryDeclaration == nil:
		r.provisionSiblings(ctx, qw, wpath)
	}

	// 2b. Provision kit toolchain into the worktree (Seam 2 / 006).
	//
	// Cloud sandboxes boot bare; the kit's [provide.toolchain_install.<os>]
	// scripts + the post_acquire hook must run against the acquired
	// worktree BEFORE the agent spawns ("Kit provide() runs against
	// acquired workarea; toolchain is pre-set"). A non-zero exit aborts the
	// session here — the agent never starts (005:357 "failure of any
	// aborts"). pre_release runs best-effort on teardown via defer.
	//
	// Demand resolution (OD-1: explicit-overrides-detection, KITS PIVOT #3):
	//   1. qw.Kits — the platform-resolved kit toolchain demand threaded on
	//      the work item (composed from the agent composition's KitRef[]).
	//      When non-nil and non-empty it is authoritative and detection is
	//      skipped — the platform already chose the kits for this session.
	//   2. r.kitDetector — fallback: detect kits from the cloned worktree's
	//      files when the platform sent no demand. Requires KitDetector to
	//      be wired (set at runner construction in afcli/agent_run.go).
	// Zero-kit sessions (no platform demand AND no detector / no match) skip
	// this entirely (additive — pre-K1 behaviour preserved).
	var demand *kit.ToolchainDemand
	if len(selection.receipt.Bytes()) == 0 || qw.Kits != nil {
		demand = r.resolveKitDemand(qw, wpath, res)
	}
	if demand != nil {
		if res.Status == "failed" {
			// resolveKitDemand classified a detect/compose error onto res.
			return res, fmt.Errorf("kit demand: %s", res.Error)
		}
		if !demand.IsEmpty() {
			if selectedRepositoryReadOnly {
				err := fmt.Errorf("selected read-only repository cannot receive runner-owned toolchain provisioning")
				res.Status, res.FailureMode, res.Error = "failed", FailureKitProvision, err.Error()
				return res, err
			}
			r.logger.Info("kit toolchain provision starting",
				"sessionId", qw.SessionID,
				"os", demand.OS,
				"kits", demand.Kits,
				"installSteps", len(demand.ToolchainInstall),
				"postAcquireSteps", len(demand.PostAcquire),
			)
			execer := shellExecer{baseEnv: cappedSessionEnv(qw)}
			provisioner := kit.NewProvisioner(r.logger)
			if provErr := provisioner.Provision(ctx, execer, wpath, demand); provErr != nil {
				res.Status = "failed"
				res.FailureMode = FailureKitProvision
				res.Error = provErr.Error()
				return res, provErr // Seam 2: agent never spawns
			}
			// pre_release on teardown — best-effort, never fatal (005:218).
			defer provisioner.Release(context.Background(), execer, wpath, demand)
		}
	}

	// 2b-bis. Repository dependency install. After the kit toolchain
	// (step 2b) and before spawn: a pnpm lockfile triggers
	// `pnpm install --prefer-offline --frozen-lockfile`, a go.mod
	// triggers `go mod download`. Bounded by one timeout; a failure is
	// logged and the run continues. Skipped when the kit hook already
	// installed (node_modules/.bin present) and when the selected
	// repository is read-only.
	r.installSessionDependencies(ctx, qw, wpath, selectedRepositoryReadOnly)

	// 2c. Post-clone kit skill + prompt-fragment re-detection.
	//
	// The daemon pre-computed KitSkillSources at runner construction time
	// using its CWD, but the real repo has now been cloned to wpath so
	// detection against the actual repo contents may differ (e.g. a framework
	// manifest declares files=[pom.xml] and the daemon's CWD has no pom.xml).
	// When KitSkillDetector is wired, replace the pre-computed sources with a
	// fresh scan against wpath. Additive: nil KitSkillDetector keeps the
	// existing KitSkillSources (pre-K1-bootstrap behaviour).
	var kitSkillSources []kit.KitSkillSource
	if len(selection.receipt.Bytes()) == 0 {
		kitSkillSources = r.kitSkillSources // default: daemon-CWD pre-compute
	}
	if len(selection.receipt.Bytes()) == 0 && r.kitSkillDetector != nil {
		targetOS := r.kitTargetOS
		detected, detectErr := r.kitSkillDetector(wpath, targetOS)
		if detectErr != nil {
			r.logger.Warn("kit skill detector (post-clone) failed; falling back to pre-computed sources",
				"sessionId", qw.SessionID,
				"err", detectErr,
			)
		} else {
			kitSkillSources = detected // nil is fine → step 5a skips injection
		}
	}

	// Detect prompt-fragment sources from the cloned worktree when the detector
	// is wired. nil = no fragment injection (additive).
	var kitPromptFragSources []kit.KitPromptFragmentSource
	if len(selection.receipt.Bytes()) == 0 && r.kitPromptFragDetector != nil {
		targetOS := r.kitTargetOS
		frags, fragErr := r.kitPromptFragDetector(wpath, targetOS)
		if fragErr != nil {
			r.logger.Warn("kit prompt-fragment detector (post-clone) failed; skipping fragment injection",
				"sessionId", qw.SessionID,
				"err", fragErr,
			)
		} else {
			kitPromptFragSources = frags
		}
	}

	// 3. Compose env. Daemon is expected to inject the resolved
	// credential into qw.AuthToken's matching env var via Spec.Env;
	// we forward whatever the caller set plus the standard session
	// metadata.
	// Per-seat budget: overlay the cooperative worker caps so the tools
	// the harness fans out size themselves to the seat share. The daemon
	// already applies the same caps to the worker environment; re-applying
	// here covers standalone runs and guarantees the harness child — the
	// process that actually spawns the fan-out — carries them. Explicit
	// values win; a disabled budget changes nothing.
	specEnv := cappedSessionEnv(qw)
	effectiveMCPBearerFile, err := prepareSessionMCPBearerEnv(
		qw,
		specEnv,
		os.Getenv(mcpGatewayTokenFileEnv),
	)
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailureSpawn
		res.Error = err.Error()
		return res, err
	}
	defer effectiveMCPBearerFile.Cleanup()

	// 4. Build MCP config. The exact harness adapter applies or denies the
	// resulting set before spawn; nothing here is silently dropped.
	//
	// The platform per-session HTTP gate leads whenever it is emitted at all:
	// it is the A2A capability-bundle + tool-call allow-list enforcement
	// point, so it must never be shadowed. It is emitted only for a harness
	// that declares MCP delivery for this session mode — see
	// defaultMCPServersForHarness. The agent card's MCP servers (qw.McpServers,
	// WS5) are APPENDED after it, unfiltered: they are caller-requested, so an
	// undeliverable one must deny loudly. Dedup is by server name with the
	// default winning on collision.
	mcpDefaults := defaultMCPServersForHarness(qw, wpath, provider, sessionPromptMode(qw, selection.effectiveCell), codeIntelDelivery.Route)
	// Surface a degraded mint: a platform-connected session with no
	// session-scoped bearer omits the gateway below, so name that omission
	// here or it reads as an ordinary standalone session. Strictly a signal —
	// the spawn proceeds with whatever defaults were emitted, and the worker
	// bearer is never substituted for the missing session bearer.
	logMCPGatewayDegradedMint(r.logger, qw)
	res.PostSessionWarnings = appendDegradedMintWarning(res.PostSessionWarnings, qw)
	// Advisory only — see logMCPGatewayBearerExpiry. The bearer below is
	// written into a config file nothing rewrites, so this line is the only
	// warning an operator gets that the session's tools have a horizon.
	v2Applies := r.protectedRuntimeMCPV2Applies(qw, selection)
	if !v2Applies {
		logMCPGatewayBearerExpiry(r.logger, qw, mcpDefaults, time.Now())
	}
	mcpServers := mergeMCPServers(mcpDefaults, qw.McpServers)
	if v2Applies {
		mcpServers, err = applyProtectedRuntimeMCPV2(qw, selection, r.capabilityRealizations, r.protectedRuntimeMCPV2Selector, mcpServers, effectiveMCPBearerFile.Path)
	} else {
		err = validateProtectedRuntimeMCPMaterialization(qw, selection, r.capabilityRealizations, r.protectedRuntimeMCPSelector, mcpServers)
	}
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailureSpawn
		res.Error = err.Error()
		return res, err
	}
	mcpResult, err := buildMCPConfigPath(r.mcpb, mcpServers)
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailureSpawn
		res.Error = fmt.Sprintf("mcp config build: %v", err)
		return res, err
	}
	defer mcpResult.Cleanup()

	// 5. Render prompt.
	//
	// 5a. Collect Kit [provide.skills] + [provide.prompt_fragments]
	// contributions and inject them into the prompt builder before rendering.
	//
	// Skills (from kitSkillSources — post-clone-detected or daemon-CWD
	// pre-computed): loaded in kit-priority order (higher priority → earlier
	// position); unreadable files are skipped with a warning so a broken kit
	// does not abort the session. Tool disallow rules scraped from SKILL.md
	// frontmatter are carried forward to step 6 for application to the
	// agent.Spec.
	//
	// Prompt fragments (from kitPromptFragSources — post-clone-detected):
	// filtered by qw.WorkType, then their file bodies are appended AFTER
	// the skill block. Fragments with an empty [when] list match all
	// workTypes (no filter). Additive: nil sources = no fragment injection.
	// SkillAppend is per-run scratch state. Keep it on a fresh builder so
	// concurrent sessions on one long-lived Runner cannot overwrite each
	// other's skill composition. SystemAppend and Registry are immutable
	// construction inputs and remain shared by value/pointer respectively.
	promptBuilder := &prompt.Builder{
		SystemAppend: r.promptBuilder.SystemAppend,
		Registry:     r.promptBuilder.Registry,
	}

	var kitDisallowedTools []string
	if len(kitSkillSources) > 0 {
		loaded, skillErr := kitLoadSkills(kitSkillSources)
		if skillErr != nil {
			r.logger.Warn("kit skill loader: partial load (some skill files skipped)",
				"sessionId", qw.SessionID,
				"err", skillErr,
			)
		}
		promptBuilder.SkillAppend = loaded.SystemAppend
		kitDisallowedTools = loaded.DisallowedTools
		if loaded.SystemAppend != "" {
			r.logger.Info("kit skills injected into system prompt",
				"sessionId", qw.SessionID,
				"skillBytes", len(loaded.SystemAppend),
				"disallowCount", len(kitDisallowedTools),
			)
		}
	}

	// Fold the agent card's INLINE skills (WS5) into the prompt builder AFTER
	// the kit (file-sourced) skills, and union their disallowedTools into the
	// kit-derived disallowed set. Inline skills carry their body verbatim on
	// the wire (no SKILL.md on disk). Additive: no card skills → no change.
	if len(qw.Skills) > 0 {
		newAppend, inlineDisallow, injected := foldInlineSkills(promptBuilder.SkillAppend, qw.Skills)
		promptBuilder.SkillAppend = newAppend
		kitDisallowedTools = append(kitDisallowedTools, inlineDisallow...)
		if injected > 0 {
			r.logger.Info("agent-card inline skills injected into system prompt",
				"sessionId", qw.SessionID,
				"skillCount", injected,
				"skillBytes", len(newAppend),
			)
		}
	}
	// Inject workType-filtered prompt fragments into the prompt builder.
	// Fragment bodies are appended after the skill block so the kit's skills
	// always precede its work-type-specific guidance.
	if len(kitPromptFragSources) > 0 {
		loadedFrags, fragErr := kit.LoadPromptFragments(kitPromptFragSources, qw.WorkType)
		if fragErr != nil {
			r.logger.Warn("kit prompt-fragment loader: partial load (some fragment files skipped)",
				"sessionId", qw.SessionID,
				"err", fragErr,
			)
		}
		if loadedFrags.SystemAppend != "" {
			// Append to any skill text already set above.
			existing := promptBuilder.SkillAppend
			if existing != "" {
				promptBuilder.SkillAppend = existing + "\n\n" + loadedFrags.SystemAppend
			} else {
				promptBuilder.SkillAppend = loadedFrags.SystemAppend
			}
			r.logger.Info("kit prompt fragments injected into system prompt",
				"sessionId", qw.SessionID,
				"workType", qw.WorkType,
				"fragBytes", len(loadedFrags.SystemAppend),
			)
		}
	}

	// Interview mode: prepend the hardened interview persona to
	// the upstream-supplied system-prompt override BEFORE rendering so the
	// prompt builder emits it immediately after the runner-owned content-safety
	// preamble. The persona pins the agent into one-question-per-turn /
	// thinking-only behaviour and survives a cloned-repo CLAUDE.md (a live
	// sandbox run proves the hostile-CLAUDE.md case in a live sandbox). Headless
	// runs are untouched — this only fires when qw.Mode == interview.
	if qw.isInterview() {
		qw.SystemPromptOverride = buildInterviewSystemPrompt(
			qw.SystemPromptOverride, interview.InterviewCompleteSentinel)
	}

	// Render source-addressed prompt authorities. The exact harness profile,
	// not a coarse provider capability, decides whether memory/context rides a
	// native system surface or the first turn.
	composition, err := promptBuilder.BuildComposition(qw.QueuedWork)
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailurePromptRender
		res.Error = err.Error()
		return res, err
	}

	// 5b. Code-intel usage partial. When (and only when) the CodeIntel
	// capability block is present, append a compact usage partial to the
	// composed system prompt — FQ MCP tool names for MCP-capable providers,
	// Bash-CLI fallback guidance for providers that ignore MCP specs. Strict
	// no-op when the block is absent (byte-identical prompt to today).
	composition.HarnessProtocol = injectCodeIntelPartialForDelivery(composition.HarnessProtocol, caps, qw.CodeIntel, codeIntelDelivery)
	composition.HarnessProtocol = injectWorkareaProtocolPartial(composition.HarnessProtocol, repositoryDeclaration != nil)
	systemPrompt := composition.SystemPrompt()
	userPrompt := composition.UserPrompt
	if qw.isInteractive() {
		// Interactive rendering deliberately suppresses batch task scaffolding,
		// but InitialPrompt is still the caller's required first user task. Put
		// that authority into the pre-spawn plan so the exact harness profile
		// must deliver or deny it and the receipt covers the real input. The
		// provider owns native delivery; dispatchInteractive must never replay
		// these bytes after spawn.
		userPrompt = qw.InitialPrompt
		if limit := interactiveInitialPromptLimitForProvider(provider); len(userPrompt) > limit {
			err = fmt.Errorf(
				"interactive initial prompt is %d UTF-8 bytes; limit is %d bytes",
				len(userPrompt),
				limit,
			)
			res.Status = "failed"
			res.FailureMode = FailureInteractiveInput
			res.Error = err.Error()
			return res, err
		}
	}
	promptPlan := &agent.PromptPlan{
		ContractVersion:  agent.PromptContractVersion,
		BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
		UserPrompt:       agent.PromptContent{ID: "runner-user-task", Text: userPrompt, Required: userPrompt != ""},
	}
	if provider.Name() != agent.ProviderShell {
		// Model-driving harnesses receive the runner-owned operating protocol
		// and its legacy policy-authorized user-turn fallbacks. A bare shell is
		// intentionally excluded: its user surface executes commands, so no
		// non-user authority may be projected onto shell_pty_seed.
		promptPlan.HarnessProtocol = &agent.PromptContent{ID: "runner-harness-protocol", Text: composition.HarnessProtocol, Required: true}
		promptPlan.AuthorizedDowngrades = []agent.PromptDowngradeAuthorization{
			{ID: "runner-authorizes-protocol-to-user", Channel: agent.PromptChannelHarnessProtocol, To: agent.PromptChannelUserPrompt},
			{ID: "runner-authorizes-role-to-user", Channel: agent.PromptChannelRoleIntent, To: agent.PromptChannelUserPrompt},
			{ID: "runner-authorizes-context-to-user", Channel: agent.PromptChannelInitialContext, To: agent.PromptChannelUserPrompt},
		}
		if composition.RoleIntent != "" {
			promptPlan.RoleIntent = &agent.PromptContent{ID: "agent-card-role-intent", Text: composition.RoleIntent, Required: true}
		}
		if composition.InitialContext != "" {
			promptPlan.InitialContext = []agent.PromptContent{{ID: "agent-memory-context", Text: composition.InitialContext, Required: true}}
		}
	}

	// 6. Translate to agent.Spec.
	composedEnv := envToMap(r.envc.Compose(hostEnv(), agent.Spec{Env: specEnv}))
	spec, err := translateSpecForCodeIntelDelivery(qw, caps, SpecInputs{
		Cwd:                wpath,
		Prompt:             userPrompt,
		SystemPromptAppend: systemPrompt,
		PromptPlan:         promptPlan,
		InitialContext:     composition.InitialContext,
		MCPServers:         mcpServers,
		Env:                composedEnv,
		Autonomous:         true,
		Logger:             r.logger,
		ProviderName:       string(provider.Name()),
	}, codeIntelDelivery, kitDisallowedTools)
	if err != nil {
		res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
		return res, err
	}

	// Interactive mode: request spawn-under-PTY with a live interactive
	// session surface (interactive-attach-v1). Spec.Interactive is
	// capability-gated by the harness manifest — only PTY-transport
	// harnesses honor it; any other harness ignores it (the same rule as
	// every other Spec field), which the runner catches after Spawn by
	// type-asserting the handle to agent.InteractiveCapable.
	//
	// Geometry is intentionally left at zero so ptyhost falls back to its
	// 80×24 default (agent.InteractiveSpec § Cols/Rows): QueuedWork carries
	// no viewport hint today, and the relay resizes the PTY authoritatively
	// once the first viewer joins (spec § 8, applied verbatim), so a fixed
	// initial geometry is not load-bearing.
	//
	// RecordPath (the asciinema-v2 cast destination) is populated only when
	// host-side recording is allowed by policy: QueuedWork.RecordingEnabled
	// nil (no platform decision — standalone, or a platform predating the
	// field) or true both default to allowed; explicit false leaves
	// RecordPath empty, which ptyhost's newRecorder treats as a valid no-op
	// (no cast file is ever created). Spec.Interactive itself is still built
	// unconditionally — the PTY surface is needed regardless of recording
	// policy; only the parallel recording is gated. When a cast IS written it
	// lands in the session workarea next to events.jsonl, matching the
	// workarea convention (state.AgentDirName) — see termCastPath.
	if qw.isInteractive() {
		spec.Interactive = &agent.InteractiveSpec{}
		if qw.RecordingEnabled == nil || *qw.RecordingEnabled {
			spec.Interactive.RecordPath = termCastPath(runnerStatePath)
		}
	}
	if len(selection.receipt.Bytes()) > 0 {
		if codeIntelDelivery.Route == codeIntelDeliveryNative {
			if err := requirePreparedNativeCodeIntelPolicy(spec, preparedSource); err != nil {
				res.Status, res.FailureMode, res.Error = "failed", FailureProviderResolve, err.Error()
				return res, err
			}
		}
		spec = applyPreparedSourceAuthority(spec, preparedSource, preparedPlan)
	} else {
		spec.OnPromptAdapted = func(receipt agent.PromptDeliveryReceipt) error {
			_, err := r.store.Update(runnerStatePath, func(s *state.State) error {
				s.PromptReceipt = &receipt
				return nil
			})
			return err
		}
		spec.OnToolLifecycleAdapted = func(receipt agent.ToolLifecycleReceipt) error {
			_, err := r.store.Update(runnerStatePath, func(s *state.State) error {
				s.AppendToolLifecycleReceipt(receipt)
				return nil
			})
			return err
		}
	}
	if repositoryDeclaration != nil {
		policy, policyErr := repositoryAuthorityPolicy(*repositoryDeclaration, declaredRepositoryPaths, skippedRepositories,
			res.WorkareaRoot, wpath, string(executorWorkareaCapabilities.RepositoryAuthorityEnforcement))
		if policyErr != nil {
			res.Status, res.FailureMode, res.Error = "failed", FailureWorktreeProvision, policyErr.Error()
			return res, policyErr
		}
		// The authority declaration is stronger than a caller's autonomous
		// full-access preference. Only declared mutable paths may be writable.
		// ReconcileRepositorySandbox (repository_sandbox_reconcile.go) is the
		// SAME function the daemon preflight compiler applies
		// (provider_view.go's PreflightExecution, via compilePreparedHarness)
		// — a receipt-bearing session with a declared repository must derive
		// this exact mutation identically on both sides, or the host-compiled
		// authority digest can never agree with what this spawn lane
		// materializes even though nothing genuinely changed.
		spec = ReconcileRepositorySandbox(spec, repositoryDeclaration)
		spec.RepositoryAuthority = policy
	}

	// 6b. Execution security: refuse a stamped level this exact harness and
	// session mode cannot render, and record what a stamped run achieves,
	// before any state, credential or provider side effect. Receipt-bearing
	// work reports the host-compiled plan's report once it meets the stamp
	// (the provider's PrepareHarness also re-derives it byte-for-byte);
	// everything else renders here with the same function.
	report, securityErr := executionSecurityReport(spec, provider, preparedPlan)
	if securityErr != nil {
		refuseForExecutionSecurity(res, securityErr)
		return res, securityErr
	}
	res.ExecutionSecurity = report

	// 7. Initialise the per-session state.json so a crash mid-spawn
	// is recoverable.
	var journalBoundaryErr error
	if _, err := r.store.Update(runnerStatePath, func(s *state.State) error {
		journalBoundaryErr = stampRunJournalBoundary(s, qw.SessionID, startedAt, runnerStatePath)
		s.IssueIdentifier = qw.IssueIdentifier
		s.IssueTitle = qw.Title
		s.IssueID = qw.IssueID
		s.SessionID = qw.SessionID
		s.ProviderName = provider.Name()
		s.Harness = selection.Harness.ID
		s.AgentCardID = qw.AgentCardID
		s.AgentCardName = qw.AgentCardName
		s.Model = qw.ResolvedProfile.Model
		// Display axes come from the final admitted binding, never from the
		// harness name or a model-id prefix. Missing bindings stay unknown.
		s.ModelProvider, s.ModelAuthor, s.EndpointOperator, s.Protocol = "", "", "", ""
		if spec.Endpoint != nil {
			s.ModelProvider = string(spec.Endpoint.Company)
			s.ModelAuthor = spec.Endpoint.ModelAuthor
			s.EndpointOperator = spec.Endpoint.EndpointOperator
			s.Protocol = string(spec.Endpoint.Protocol)
		}
		s.WorkType = qw.WorkType
		s.WorkerID = qw.WorkerID
		s.CurrentStep = "spawning"
		s.AttemptCount++
		s.ExecutionSecurity = report
		return nil
	}); err != nil {
		// state.json is best-effort — log and continue.
		r.logger.Warn("state init failed", "sessionId", qw.SessionID, "err", err)
	} else if journalBoundaryErr != nil {
		r.logger.Warn("event journal boundary unavailable", "sessionId", qw.SessionID)
	}

	// 8. Spawn provider.
	handle, err := provider.Spawn(ctx, spec)
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailureSpawn
		res.Error = err.Error()
		return res, err
	}
	defer func() {
		// Best-effort stop on exit. Stop is idempotent.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = handle.Stop(stopCtx)
	}()

	// 8b. Wave 3 runtime memory-inject (v2) channel + per-Run dedup.
	//
	// injectCh carries platform-queued memory blocks delivered via the
	// heartbeat lock-refresh transport (see step 9 OnInject wiring). It is
	// drained between turns at the post-terminal seam (step 11) so claude's
	// single-in-flight Inject contract is respected on the single runner
	// goroutine. seenInject dedupes by DeliveryID within this Run (the
	// platform also acks to stop re-sending, but a re-delivery in the
	// heartbeat-interval window before the ack lands must not double-inject).
	//
	// Whether any block is actually delivered is decided ENTIRELY by the
	// platform (it only returns an `inject` on the lock-refresh response when
	// the project's memory config has runtime-inject enabled) — so the worker
	// needs no env var or local config.
	//
	// The rail is wired when a CONSUMER EXISTS, which is not the same question
	// as "does the provider support message injection":
	//
	//   - Headless and interview runs deliver through Handle.Inject, so they
	//     genuinely need caps.SupportsMessageInjection. Providers without it
	//     rely on the dispatch-time fold (v1).
	//   - An INTERACTIVE run never calls Handle.Inject at all — its delivery
	//     surface is whatever the harness DECLARES (agent.NoticeDelivery), so
	//     the provider's message-injection capability is simply the wrong
	//     question here. The rail is wired for every interactive run, including
	//     the ones whose declared channel this build cannot drive, because the
	//     consumer's job is not only to deliver: it is also to say truthfully
	//     that it could not, by dead-lettering with a reason instead of letting
	//     the payload rot in a buffer nobody reads. Nothing on that path acks.
	injectCh := make(chan heartbeat.InjectPayload, 8)
	seenInject := map[string]struct{}{}
	runtimeInjectEnabled := caps.SupportsMessageInjection || qw.isInteractive()

	// 9. Start heartbeat pulser (in a goroutine — Pulser.Start fires
	// the first tick synchronously then runs the loop in its own
	// goroutine).
	var hbCredentialProvider heartbeat.CredentialProvider
	if r.credentialProvider != nil {
		hbCredentialProvider = func(ctx context.Context) (heartbeat.RuntimeCredentials, error) {
			creds, err := r.credentialProvider(ctx)
			return heartbeat.RuntimeCredentials{
				WorkerID:  creds.WorkerID,
				AuthToken: creds.AuthToken,
			}, err
		}
	}
	// Wave 3 runtime memory-inject: wire OnInject only when a consumer exists.
	// See newInjectAcceptor for the dedup + ack-or-requeue contract, and for
	// why the interactive mode acks on DELIVERY rather than on buffer.
	var onInject func(heartbeat.InjectPayload) bool
	if runtimeInjectEnabled {
		onInject = newInjectAcceptor(
			injectCh, seenInject, r.logger, qw.SessionID, !qw.isInteractive(),
		)
	}
	pulser, err := heartbeat.NewWithAckObserver(heartbeat.Config{
		SessionID: qw.SessionID,
		WorkerID:  qw.WorkerID,
		// IssueID is the Linear issue UUID — the platform's
		// /lock-refresh handler keys the lock on issue:lock:{id}
		// and rejects the request with 400 when this is empty.
		// Sourced from prompt.QueuedWork.IssueID (camelCase
		// "issueId" on the wire).
		IssueID:   qw.IssueID,
		BaseURL:   qw.PlatformURL,
		AuthToken: qw.AuthToken,
		// SessionClass stamps every lock-refresh body with the runtime
		// session class so the platform's activity-stall reaper exempts an
		// interactive session during human think-time (W4 amendment 4 —
		// the named cross-repo dependency W3 reads). "interactive" for the
		// PTY-hosted interactive dispatch; empty for every other mode
		// (omitempty keeps the wire byte-identical for headless/interview).
		SessionClass:       interactiveSessionClass(qw),
		CredentialProvider: hbCredentialProvider,
		// Persist the actual successful session-heartbeat acknowledgement
		// into state.State.LastHeartbeat (the field host-watch reads),
		// fenced to this exact session and run. Failed refreshes, replay
		// ingestion and output-only activity never fire this callback.
		Interval:   r.hbInterval,
		HTTPClient: r.httpClient,
		Logger:     r.logger,
		OnInject:   onInject,
	}, r.heartbeatAckObserver(runnerStatePath, qw.SessionID, startedAt))
	if err != nil {
		// Heartbeat is non-fatal at construction time only when
		// PlatformURL is missing; that's caught by validateQueuedWork.
		r.logger.Warn("heartbeat construct failed", "err", err)
	} else if startErr := pulser.Start(ctx); startErr != nil {
		r.logger.Warn("heartbeat start failed", "err", startErr)
	} else {
		defer func() { _ = pulser.Stop() }()
	}

	// 9b. Start the activity poster (mirrors the heartbeat pulser's
	// per-session lifecycle). Pushes every runner-observed agent.Event
	// to /api/sessions/<id>/activity asynchronously so the platform
	// activity buffer + topology view stay populated. Best-effort: a
	// construction or start error is logged and the loop falls back to
	// the noop sink so the rest of the run is unaffected.
	var sink activitySink = noopSink{}
	var actCredentialProvider activity.CredentialProvider
	if r.credentialProvider != nil {
		actCredentialProvider = func(ctx context.Context) (activity.RuntimeCredentials, error) {
			creds, err := r.credentialProvider(ctx)
			return activity.RuntimeCredentials{
				WorkerID:  creds.WorkerID,
				AuthToken: creds.AuthToken,
			}, err
		}
	}
	actPoster, actErr := activity.New(activity.Config{
		SessionID:          qw.SessionID,
		WorkerID:           qw.WorkerID,
		BaseURL:            qw.PlatformURL,
		AuthToken:          qw.AuthToken,
		CredentialProvider: actCredentialProvider,
		HTTPClient:         r.httpClient,
		Logger:             r.logger,
		// ProviderName flows onto the wire payload so the platform's
		// hook-bus bridge can build a faithful ProviderRef for the
		// reconstructed Layer 6 hook events. Resolved earlier (the
		// registry.Resolve call at line 93 used the same value).
		ProviderName: string(provider.Name()),
	})
	if actErr != nil {
		r.logger.Warn("activity poster construct failed", "err", actErr)
	} else if startErr := actPoster.Start(ctx); startErr != nil {
		r.logger.Warn("activity poster start failed", "err", startErr)
	} else {
		sink = actPoster
		defer func() { _ = actPoster.Stop() }()
	}

	// 9b.1 Start normalized execution-event capture only when the target
	// explicitly advertises its ingest route. The uploader owns its durable
	// journal and never extends the activity poster's best-effort contract.
	var eventSink *executionEventSink
	eventSink, eventErr := newExecutionEventSinkForWork(qw, sink, r.logger, func() (*executionevent.Uploader, error) {
		var eventCredentialProvider executionevent.CredentialProvider
		if r.credentialProvider != nil {
			eventCredentialProvider = func(ctx context.Context) (executionevent.RuntimeCredentials, error) {
				creds, credErr := r.credentialProvider(ctx)
				return executionevent.RuntimeCredentials{WorkerID: creds.WorkerID, AuthToken: creds.AuthToken}, credErr
			}
		}
		return executionevent.New(executionevent.Config{
			SessionID:          qw.SessionID,
			BaseURL:            qw.PlatformURL,
			AuthToken:          qw.AuthToken,
			CredentialProvider: eventCredentialProvider,
			HTTPClient:         r.httpClient,
			Logger:             r.logger,
		})
	})
	if eventErr != nil {
		r.logger.Warn("execution-event uploader construct failed", "err", eventErr)
	} else if eventSink != nil {
		sink = eventSink
		defer func() { eventSink.Close(res) }()
	}

	// 9c. Start the additive per-call span pipeline when explicitly enabled by
	// the binary/operator or when the dispatch advertises a compatible ingest
	// route. The capability gate is the mixed-version seam: an older server
	// receives no unknown requests, while DONMAI_OTEL_TRACES can opt an OSS
	// embedder into a configured endpoint. Both poster and processor are
	// best-effort; construction failure leaves the activity stream untouched.
	var traceProcessor spanEventProcessor = noopSpanProcessor{}
	if r.spanEmissionEnabled || qw.hasCapability(CapabilitySpanIngest) {
		var spanCredentialProvider spanruntime.CredentialProvider
		if r.credentialProvider != nil {
			spanCredentialProvider = func(ctx context.Context) (spanruntime.RuntimeCredentials, error) {
				creds, credErr := r.credentialProvider(ctx)
				return spanruntime.RuntimeCredentials{AuthToken: creds.AuthToken}, credErr
			}
		}
		spanPoster, spanErr := spanruntime.NewPoster(spanruntime.PosterConfig{
			BaseURL:            qw.PlatformURL,
			EndpointPath:       r.spanEndpointPath,
			AuthToken:          qw.AuthToken,
			CredentialProvider: spanCredentialProvider,
			HTTPClient:         r.httpClient,
			Logger:             r.logger,
		})
		if spanErr != nil {
			r.logger.Warn("span poster construct failed; per-call tracing disabled", "err", spanErr)
		} else if startErr := spanPoster.Start(ctx); startErr != nil {
			r.logger.Warn("span poster start failed; per-call tracing disabled", "err", startErr)
		} else {
			processor, processorErr := spanruntime.NewProcessor(spanruntime.ProcessorConfig{
				SessionID:        qw.SessionID,
				OrgID:            qw.OrganizationID,
				WorkspaceID:      qw.OrganizationID,
				WorkType:         qw.WorkType,
				System:           spanruntime.ProviderSystem(provider.Name()),
				Model:            qw.ResolvedProfile.Model,
				Traceparent:      qw.Traceparent,
				Tracestate:       qw.Tracestate,
				SessionStorageID: qw.SessionStorageID,
				SessionPublicID:  qw.SessionPublicID,
				TrackerSessionID: qw.TrackerSessionID,
				Sender:           spanPoster,
				Now:              r.now,
			})
			if processorErr != nil {
				r.logger.Warn("span processor construct failed; per-call tracing disabled", "err", processorErr)
				_ = spanPoster.Stop()
			} else {
				traceProcessor = processor
				defer func() {
					processor.Finish(res.Status, res.Error)
					_ = spanPoster.Stop()
				}()
			}
		}
	}

	// 9d. Start the step-heartbeat emitter (mirrors the heartbeat pulser +
	// activity poster per-session lifecycle). Every 15s it POSTs a
	// decoupled step-liveness beat to /api/sessions/<id>/step-heartbeat so
	// the platform can stamp agent_sessions.last_step_heartbeat +
	// last_progress_at and refresh the Redis session:heartbeat pointer —
	// closing governor Class-1 stale detection for the worker-alive/
	// session-wedged case (a runner still holding its ownership lock but
	// producing no genuine tool/token events for minutes). Best-effort: a
	// construction/start error is logged and skipped, and a POST failure
	// (including a 404 from a platform build without the companion route)
	// is swallowed inside the emitter — a step-heartbeat outage must never
	// fail the run. Wave 3 item 1.
	//
	// The usage behind the beat depends on the lane: headless sessions read
	// the budget enforcer's meter, while interactive sessions read the
	// transcript-tail totals the interactive supervisor accumulates (an
	// interactive session never constructs an enforcer reading). The totals
	// live on the Runner for the session so both the emitter wiring here
	// and the supervisor below share them; the heartbeat carries the
	// running totals, clamped non-decreasing by the emitter.
	var stepCredentialProvider stepheartbeat.CredentialProvider
	if r.credentialProvider != nil {
		stepCredentialProvider = func(ctx context.Context) (stepheartbeat.RuntimeCredentials, error) {
			creds, err := r.credentialProvider(ctx)
			return stepheartbeat.RuntimeCredentials{
				WorkerID:  creds.WorkerID,
				AuthToken: creds.AuthToken,
			}, err
		}
	}
	stepEmitter, stepErr := stepheartbeat.New(stepheartbeat.Config{
		SessionID:          qw.SessionID,
		WorkerID:           qw.WorkerID,
		BaseURL:            qw.PlatformURL,
		AuthToken:          qw.AuthToken,
		CredentialProvider: stepCredentialProvider,
		UsageProvider:      r.stepHeartbeatUsage(qw, enforcer),
		HTTPClient:         r.httpClient,
		Logger:             r.logger,
		Interval:           r.stepHeartbeatInterval,
		// Interval is zero in production, keeping the 15s default —
		// calibrated against the platform's 60s SESSION_STALE_THRESHOLD_MS.
	})
	if stepErr != nil {
		r.logger.Warn("step-heartbeat construct failed", "err", stepErr)
	} else if startErr := stepEmitter.Start(ctx); startErr != nil {
		r.logger.Warn("step-heartbeat start failed", "err", startErr)
	} else {
		defer func() { _ = stepEmitter.Stop() }()
	}

	// 9e. Record the reasoning effort requested from the harness — once, on
	// the platform activity stream and in state.json — before any turn
	// output, for every run mode (headless, interview, interactive). A
	// session's record then states the level it ran at, or that none was
	// configured and the harness or model default applied.
	r.recordReasoningEffort(ctx, runnerStatePath, qw.SessionID, spec.Effort, sink)

	// ── Interview run-mode branch ─────────────────────────────
	//
	// When qw.Mode == "interview" the runner drives the non-terminating
	// park-and-inject loop instead of the one-shot consumeEvents → drain →
	// steering → backstop → runPostSession path below. Everything above
	// this point (spawn, env, worktree, kit, state.json, heartbeat,
	// activity) is shared and has already run; the interview loop owns its
	// own event consumption + token-delta production + turn-taking and
	// returns the terminal Result directly. Steering / backstop /
	// post-session are intentionally SKIPPED — an interview produces no PR
	// and drives no Linear state transition (the SDLC handoff happens via
	// the platform's /complete CloudEvent gate, not here).
	if qw.isInterview() {
		return r.dispatchInterview(ctx, handle, runnerStatePath, qw, res, sink, traceProcessor, injectCh)
	}

	// ── Interactive run-mode branch ───────────────────────────
	//
	// When qw.Mode == "interactive" the runner drives the PTY-hosted
	// interactive session: attach the spawned InteractiveSession's live
	// byte stream outbound to the relay (env-provided ATTACH_URL/
	// ATTACH_TOKEN) and run until the child exits, ctx cancel, budget cap,
	// or operator stop. Everything above (spawn, env, worktree, kit,
	// state.json, heartbeat with the sessionClass stamp, activity) is shared
	// and has already run. Steering / backstop / post-session are SKIPPED
	// for the same reason interviews skip them: an interactive session
	// produces no PR and drives no issue-tracker state transition — the
	// lifecycle is owned by the human at the terminal, not the runner.
	//
	// injectCh is handed over the same way dispatchInterview receives it: an
	// interactive session is a LIVE consumer of the runtime-inject rail, not
	// a session that happens to have one wired. Without this argument every
	// buffered payload is accepted by OnInject, acked to the producer, and
	// then never read by anyone — silent loss that reports success.
	//
	// noticeDelivery rides along because the consumer must know what the
	// harness DECLARED before it writes anything: a PTY write is the correct
	// primitive only where no agent sits behind the terminal.
	if qw.isInteractive() {
		return r.dispatchInteractive(ctx, handle, runnerStatePath, qw, res, sink, pulser, injectCh, noticeDelivery,
			interactiveInitialPromptLimitForProvider(provider))
	}

	// 10. Stream events; wait for terminal.
	// Budget duration cap rides on top of the stream ctx — when it
	// fires the consumer sees ctx.Err() == context.DeadlineExceeded
	// and we classify as FailureBudgetExceeded (CapDuration) below.
	budgetCtx, budgetCancel := enforcer.WithDurationCap(ctx)
	defer budgetCancel()
	streamCtx, streamCancel := context.WithCancel(budgetCtx)
	defer streamCancel()

	// Heartbeat lost-ownership shortcut: cancel streamCtx and
	// surface FailureLostOwnership on the result.
	lostOwnership := make(chan struct{})
	if pulser != nil {
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-pulser.LostOwnership():
				close(lostOwnership)
				streamCancel()
			}
		}()
	}

	stallFollowUps := r.newTurnFollowUps()
	stallFollowUps.limit = r.stallRetryLimit()
	streamRes, streamErr := r.consumeEventsWithStallRetries(ctx, streamCtx, provider, &handle, runnerStatePath, qw, res, enforcer, sink, traceProcessor, spec, stallFollowUps)
	// budgetStop is the budget cap that ended the session, once one did: the
	// provider is stopped and no further turn starts. The turn it ended is
	// still resolved below (its manifest, verdict and pull request) so the
	// session can end completed when that work was already delivered.
	stopped, budgetStop, err := r.classifyStreamStop(qw, res, handle, enforcer, pulser, lostOwnership, streamRes, streamErr)
	if stopped {
		res.Cost = enforcer.cost()
		return res, err
	}

	// Apply event-stream observations onto the result envelope.
	streamRes.applyTo(res, provider.Name())

	// 10·M. Turn-result manifest resolution (W3 — deterministic turn outcome).
	// Resolution order for the verdict: the agent-written
	// `.agent/turn-result.json` manifest FIRST, then the WORK_RESULT marker the
	// stream observation already scraped (streamRes.applyTo above), then the
	// deterministic backstop further down. The manifest WINS when present —
	// a structured file the agent wrote is more reliable than a marker scraped
	// out of free-form prose. Best-effort: a missing manifest is the common
	// case (ErrNoManifest) and a no-op; a malformed one logs + falls through
	// to the scraped marker. A manifest verdict of "blocked" feeds the same
	// streamRes.blocked signal the marker scan produces, so the blocked
	// classification fork below treats both channels identically.
	r.applyTurnManifest(runnerStatePath, qw, res, &streamRes)
	// Stamp the manifest file before any follow-up turn so step 11·M can tell
	// a manifest the follow-up wrote from one it merely left in place.
	manifestBeforeFollowUp := stampManifest(runnerStatePath)

	// 10·PR. A pull request URL in the conversation is only a candidate. For
	// work that owes a pull request, accept one only when it exists on the
	// session's own repository with the session's branch or commit as its
	// head (see pull_request_verify.go); otherwise the envelope carries no
	// pull request, so steering and the backstop still run and a quoted
	// example URL can never mark the run complete. Re-run after every
	// follow-up turn. Lookups outlive a cancelled run context (each is
	// individually bounded) so a late verification is not lost to it.
	// A rework run's pull request existed before the run: its head at run
	// start (recorded before the agent's first turn) is what a delivered
	// rework must have moved past.
	verifyCtx := context.WithoutCancel(ctx)
	var prVerifier *sessionPullRequestVerifier
	if RequiresPRURL(qw.WorkType) {
		prVerifier = r.newSessionPullRequestVerifier(verifyCtx, qw, repositoryDeclaration, wpath, branch, reworkStartHead(qw, res, wpath), repositoryFree)
	}
	r.acceptSessionPullRequest(verifyCtx, prVerifier, qw, res, &streamRes, streamRes)

	// Continue-mode delivery: the run's pull request is the continued
	// one. Seed it on the envelope BEFORE the first tail-recovery pass
	// so the no-new-commit and draft continuation rules treat the
	// continued pull request exactly like the rework pull requests they
	// already pin: a noop continue run keeps turning instead of ending
	// completed against an unchanged head.
	if qw.ContinuePullRequest != nil && !repositoryFree && res.PullRequestURL == "" && RequiresPRURL(qw.WorkType) {
		if continuedURL := continuePullRequestURL(verifyCtx, qw, repositoryDeclaration, wpath); continuedURL != "" {
			r.seedContinuedPullRequest(prVerifier, continuedURL, res, &streamRes)
		}
	}

	// 10a. Structural blocked-agent classification. When the agent
	// announced a deliberate decline (scanBlocked picked up a
	// "WORK_RESULT:blocked" / "AGENT_BLOCKED: …" marker) and did not also
	// produce a PR, fork to FailureAgentBlocked. This is a reasoned
	// refusal, not a crash — so we suppress steering + backstop below
	// (there is nothing to recover) and surface a distinct outcome the
	// platform can route to a needs-clarification path instead of
	// re-dispatching the identical context. A PR-producing session is
	// never treated as blocked even if the text mentions a blocker.
	if classifyBlocked(res, streamRes) {
		r.logger.Info("agent blocked: deliberate decline detected",
			"sessionId", qw.SessionID,
			"reason", streamRes.blockedReason,
		)
	}

	// 10b. Wave 3 runtime memory-inject drain (v2). At the post-terminal
	// seam — the turn has drained to a ResultEvent — deliver any memory
	// blocks the heartbeat transport buffered during the turn, then
	// re-consume the resume turn's events. Gated on the feature flag +
	// provider capability; a no-op when nothing was buffered. Runs BEFORE
	// steering so a memory-driven follow-up turn can itself produce the PR
	// that makes steering unnecessary.
	// 11·M (per follow-up turn). After EACH follow-up turn (memory inject,
	// then steering) re-resolve the turn verdict against it: the agent may
	// write its manifest during the follow-up, the follow-up's terminal
	// message (often just the PR URL steering asked for) has replaced
	// res.Summary, and its own anchored marker may lower — or, with no
	// manifest, replace — the verdict (see reapplyTurnManifest). Applying it
	// per turn lets a memory-inject follow-up that declines suppress steering.
	followUpRan := false
	applyFollowUp := func(tail streamObservation) {
		r.reapplyTurnManifest(runnerStatePath, qw, res, &streamRes, tail, manifestBeforeFollowUp)
		manifestBeforeFollowUp = stampManifest(runnerStatePath)
		r.acceptSessionPullRequest(verifyCtx, prVerifier, qw, res, &streamRes, tail)
		followUpRan = true
	}
	// lastTurn is the latest turn's own observation; tail recovery reads how
	// it ended.
	lastTurn := streamRes
	if runtimeInjectEnabled && !streamRes.blocked && budgetStop == nil {
		injRes := r.drainMemoryInjects(ctx, handle, runnerStatePath, qw, res, enforcer, sink, traceProcessor, injectCh)
		injRes.applyTo(res, provider.Name())
		if injRes.terminalEvent != nil || injRes.lastAssistantText != "" {
			applyFollowUp(injRes)
		}
		if injRes.terminalEvent != nil {
			lastTurn = injRes
		}
		budgetStop = r.stopAtBudget(qw, handle, enforcer, nil)
	}

	// 11. Tail recovery. Skipped entirely when the agent deliberately
	// declined (FailureAgentBlocked): there is nothing to steer toward and
	// no work to backstop into an empty branch.
	//
	// Belt-and-suspenders bypass on top of the publication gate inside
	// shouldSteer/shouldBackstop: a passing result whose completion contract
	// requires no PR has demonstrably completed its verdict-only obligation.
	//
	// Each pass reads how the LATEST turn ended (turn_continuation.go): a
	// turn that stopped early is continued, a turn that ended on a provider
	// error is retried, and a turn that left its verdict but no pull request
	// gets the pull request nudge (steering), once. Continuations are
	// bounded by progress (consecutive turns without a tool call) and by a
	// total ceiling, retries by their own count; a turn still unfinished at
	// a bound fails the session. The session's duration and token budgets
	// cover every follow-up turn: once the token meter passes the wrap-up
	// point, the next follow-up prompt asks the agent to wrap up (unless the
	// request already reached it mid-turn), and once a cap ends the session
	// no follow-up turn starts.
	publicationComplete := !RequiresPRURL(qw.WorkType) && res.WorkResult == "passed"
	followUps := r.newTurnFollowUps()
	if stallFollowUps.retried > 0 || stallFollowUps.exhausted {
		// A stall retry already ran on the first turn: carry its count
		// onto the tail-recovery record so the envelope reports every
		// retry, while the tail bounds themselves stay on the
		// continuation configuration, not the stall bound.
		followUps.retried = stallFollowUps.retried
		followUps.last = tailRetry
	}
	reviewWork := RequiresReviewVerdict(qw.WorkType)
	continuable := (RequiresPRURL(qw.WorkType) || reviewWork) && (caps.SupportsMessageInjection || caps.SupportsSessionResume)
	tailRecoverable := selectedRepositoryMutable || reviewWork
tailRecovery:
	for tailRecoverable && !r.skipSteering && !streamRes.blocked && budgetStop == nil {
		steerView := streamRes
		steerView.terminalSuccess = lastTurn.terminalSuccess
		reportedPR := prVerifier.reportsOwnRepository(lastTurn)
		ending := classifyTurnEnding(res, streamRes, lastTurn, reportedPR, reviewWork)
		followUps.undelivered = ""
		if pullRequestIsTheOnlyResult(res, streamRes, lastTurn, reportedPR, reviewWork) {
			// The session's pull request is all this turn left: it counts
			// only when it delivers the work (not a draft; on a rework, a
			// new commit since the run started).
			undelivered, moved, readErr := prVerifier.undelivered(verifyCtx)
			if readErr != nil {
				r.logger.Warn("could not fully re-read the session's pull request; an unread part counts as delivered",
					"sessionId", qw.SessionID, "url", res.PullRequestURL, "err", readErr)
			}
			if undelivered != "" {
				followUps.noteUndelivered(undelivered, moved)
				ending = turnStoppedEarly
			}
		}
		step := followUps.next(ending, lastTurn.toolCalls > 0, continuable, !publicationComplete && shouldSteer(steerView, caps, qw.WorkType))
		switch step {
		case tailDone:
			break tailRecovery
		case tailExhausted:
			if ending == turnProviderError || ending == turnProviderErrorFatal {
				followUps.providerError = lastTurn.providerError
				if res.Upstream == nil && lastTurn.upstream != nil {
					res.Upstream = agent.CanonicalUpstreamError(lastTurn.upstream)
				}
			}
			followUps.fail(res)
			r.logger.Warn("turn still unfinished at a follow-up bound; failing the session",
				"sessionId", qw.SessionID,
				"failureMode", res.FailureMode,
				"undelivered", followUps.undelivered,
				"continued", followUps.continued,
				"unproductive", followUps.unproductive,
				"retried", followUps.retried,
				"limit", followUps.limit,
				"ceiling", followUps.ceiling,
			)
			break tailRecovery
		case tailSteer:
			followUps.steered = true
			res.SteeringTriggered = true
			newHandle, err := r.attemptSteering(ctx, provider, handle, spec, caps, qw, steerView, res)
			if err != nil {
				r.logger.Warn("steering failed", "sessionId", qw.SessionID, "err", err)
				break tailRecovery
			}
			// attemptSteering returns the handle to keep draining: unchanged
			// on inject success/soft-fail, or the new Handle Provider.Resume
			// returned on the stop-and-resume fallback. The deferred Stop
			// above closes over this variable, so reassigning it here also
			// makes teardown target whichever handle is now live.
			handle = newHandle
			// Re-consume any events the steering inject/resume produced.
			// The stall detector stays disarmed here for the same reason
			// as the inject drain above: tail recovery owns follow-up
			// liveness.
			tailRes, tailErr := r.consumeEventsWithoutStallDetector(ctx, handle, runnerStatePath, qw, res, enforcer, sink, traceProcessor)
			tailRes.applyTo(res, provider.Name())
			applyFollowUp(tailRes)
			lastTurn = tailRes
			if budgetStop = r.stopAtBudget(qw, handle, enforcer, tailErr); budgetStop != nil {
				break tailRecovery
			}
			continue
		}

		prompt := continuationPrompt(reviewWork, followUps.undelivered)
		wrapUp := enforcer.wrapUpDue()
		if step == tailRetry {
			prompt = retryPrompt
			r.logger.Warn("turn ended on a model provider error; retrying",
				"sessionId", qw.SessionID,
				"attempt", followUps.retried+1,
				"limit", followUps.limit,
				"providerError", lastTurn.providerError,
			)
			if err := r.waitRetryBackoff(streamCtx, followUps.retried+1); err != nil {
				stopped, budget, stopErr := r.classifyStreamStop(qw, res, handle, enforcer, pulser, lostOwnership, streamObservation{}, err)
				if stopped {
					res.TurnContinuations = followUps.report()
					res.Cost = enforcer.cost()
					return res, stopErr
				}
				budgetStop = budget
				break tailRecovery
			}
		} else {
			r.logger.Info("turn ended before the work was finished; continuing",
				"sessionId", qw.SessionID,
				"undelivered", followUps.undelivered,
				"continuation", followUps.continued+1,
				"unproductive", followUps.unproductive,
				"limit", followUps.limit,
				"ceiling", followUps.ceiling,
			)
		}
		if wrapUp {
			// The token meter passed the wrap-up point and the agent has
			// not been asked yet: this follow-up asks it to finish.
			prompt = wrapUpPrompt
			r.logger.Info("token budget nearly spent; asking the agent to wrap up",
				"sessionId", qw.SessionID, "maxTokens", enforcer.limits.MaxTokens)
		}
		newHandle, delivered, err := r.deliverFollowUp(ctx, provider, handle, spec, caps, qw, prompt)
		handle = newHandle
		if err != nil || !delivered {
			r.logger.Warn("follow-up prompt not delivered; ending tail recovery",
				"sessionId", qw.SessionID, "err", err)
			break tailRecovery
		}
		if wrapUp {
			enforcer.wrapUpSent()
		}
		if step == tailRetry {
			followUps.retried++
		} else {
			followUps.sentContinuation()
		}
		tail, tailErr := r.consumeEventsWithoutStallDetector(streamCtx, handle, runnerStatePath, qw, res, enforcer, sink, traceProcessor)
		stopped, budget, stopErr := r.classifyStreamStop(qw, res, handle, enforcer, pulser, lostOwnership, tail, tailErr)
		if stopped {
			res.TurnContinuations = followUps.report()
			res.Cost = enforcer.cost()
			return res, stopErr
		}
		tail.applyTo(res, provider.Name())
		applyFollowUp(tail)
		lastTurn = tail
		if budget != nil {
			budgetStop = budget
			break tailRecovery
		}
	}
	res.TurnContinuations = followUps.report()
	res.Cost = enforcer.cost()

	// 11·B. Budget stop. A cap ended the session: the runner stopped the
	// provider at the turn boundary and started no further turn. When the
	// work was already delivered — a verified pull request and a passed
	// turn result — the session ends completed with the breach recorded;
	// otherwise it fails as budget-exceeded, as before.
	if budgetStop != nil {
		res.BudgetReport = enforcer.Report(r.now())
		res.BudgetBreach = &agent.BudgetBreach{Cap: string(budgetStop.Cap), Detail: budgetStop.Detail}
		if !deliveredAtBudgetStop(qw, res, repositoryDeclaration) {
			res.Status = "failed"
			res.FailureMode = FailureBudgetExceeded
			res.Error = budgetStop.Error()
			return res, budgetStop
		}
		res.Status = "completed"
		r.logger.Info("budget cap reached after the work was delivered; ending the session completed",
			"sessionId", qw.SessionID,
			"cap", string(budgetStop.Cap),
			"detail", budgetStop.Detail,
			"pullRequestUrl", res.PullRequestURL,
		)
	}

	// 11·M (blocked fork). A blocked verdict a follow-up turn produced takes
	// the 10a fork — but only when the runner has recorded no failure of its
	// own: an agent-authored decline never relabels a crash.
	if followUpRan {
		if res.FailureMode == "" && classifyBlocked(res, streamRes) {
			r.logger.Info("agent blocked: deliberate decline detected after follow-up turn",
				"sessionId", qw.SessionID,
				"reason", streamRes.blockedReason,
			)
		}
	}

	// Amend-existing-branch contract: on ref-bearing runs, skip gh pr create — the
	// fix lands on the existing branch/PR, not a new one.
	backstopEligible := !repositoryFree && shouldBackstop(res, qw.WorkType)
	if repositoryDeclaration != nil && RequiresPRURL(qw.WorkType) {
		switch res.FailureMode {
		case FailureLostOwnership, FailureTimeout, FailureProviderResolve, FailureAgentBlocked, FailureOperatorCancelled:
			backstopEligible = false
		default:
			backstopEligible = true
		}
	}
	// Continue-mode delivery: the run's pull request is the continued
	// one, seeded on the envelope before tail recovery (see above). The
	// seed must not read as delivered work that disables the backstop:
	// evaluate eligibility with the seed cleared so the session's commits
	// are still pushed to the continued head branch. The backstop itself
	// never opens a new pull request in continue mode.
	if qw.ContinuePullRequest != nil {
		probe := *res
		probe.PullRequestURL = ""
		backstopEligible = !repositoryFree && shouldBackstop(&probe, qw.WorkType)
	}
	if !r.skipBackstop && !publicationComplete && backstopEligible && budgetStop == nil {
		switch {
		// Continue+ref runs take the push lane below: the ref names the
		// continued head branch, which IS the push target.
		case trimRef(qw.Ref) != "" && qw.ContinuePullRequest == nil:
			r.logger.Info("skipping backstop gh pr create on ref-bearing run", "branch", branch, "ref", qw.Ref)
		case repositoryDeclaration != nil:
			bsCtx, bsCancel := context.WithTimeout(context.Background(), 90*time.Second)
			bsReport := r.runDeclaredBackstops(bsCtx, qw, branch, res, *repositoryDeclaration, declaredRepositoryPaths)
			bsCancel()
			res.BackstopReport = &bsReport
		default:
			bsCtx, bsCancel := context.WithTimeout(context.Background(), 90*time.Second)
			bsReport := r.runBackstop(bsCtx, qw, branch, res, nil)
			bsCancel()
			res.BackstopReport = &bsReport
			if bsReport.PRURL != "" && res.PullRequestURL == "" {
				res.PullRequestURL = bsReport.PRURL
			}
		}
	}

	// Continue-mode divergence: the backstop refused to push because the
	// continued pull request's head moved after dispatch. Record the typed
	// failure here, before the delivery gate, so the terminal status says
	// the head diverged instead of the gate reading the unpushed head as
	// "no new commit".
	if qw.ContinuePullRequest != nil && res.FailureMode == "" && continueDiverged(res.BackstopReport) {
		res.Status = "failed"
		res.FailureMode = FailureContinuePullRequestDiverged
		res.Error = fmt.Sprintf("%s: pull request #%d branch %q refused the session's push as a non-fast-forward; its commits were not published",
			ErrContinuePullRequestDiverged, qw.ContinuePullRequest.Number, continuePullRequestBranch(qw.ContinuePullRequest))
	}

	// Continue-mode delivery gate: the run's pull request is the
	// continued one, and it counts as delivered only when the session
	// moved its head past the dispatched head — pushed, so the remote
	// head carries the session's commit — and the pull request is not a
	// draft. A no-new-commit continue run (and a draft) must NOT read as
	// delivered: fail the session instead of ending completed against an
	// unchanged head, mirroring the verifier's no-new-commit/draft rules.
	// Under a dispatch-declared delivery policy the draft check is
	// skipped when the policy allows drafts, and the code-change check
	// counts merge resolutions when the policy allows merges; every
	// other check still applies. Runs only when no failure was already
	// recorded, so a runner-authored refusal (divergence, provision)
	// keeps its own typed reason.
	if qw.ContinuePullRequest != nil && !repositoryFree && RequiresPRURL(qw.WorkType) && res.FailureMode == "" && budgetStop == nil {
		startHead := strings.TrimSpace(qw.ContinuePullRequest.HeadSha)
		if res.PullRequestURL != "" || continuePullRequestURL(verifyCtx, qw, repositoryDeclaration, wpath) != "" {
			gateCtx, gateCancel := context.WithTimeout(context.Background(), 30*time.Second)
			func() {
				defer gateCancel()
				localHead, _ := captureHeadSHA(gateCtx, wpath)
				remoteHead, _ := gitStdout(gateCtx, wpath, nil, "ls-remote", "origin", "refs/heads/"+continuePullRequestBranch(qw.ContinuePullRequest))
				if fields := strings.Fields(remoteHead); len(fields) > 0 {
					remoteHead = fields[0]
				}
				lookup := r.pullRequestDraftLookup
				if lookup == nil {
					lookup = githubPullRequestDraft
				}
				continuedURL := res.PullRequestURL
				if continuedURL == "" {
					continuedURL = continuePullRequestURL(gateCtx, qw, repositoryDeclaration, wpath)
				}
				headMoved := !strings.EqualFold(strings.TrimSpace(localHead), startHead) && strings.TrimSpace(localHead) != "" && strings.TrimSpace(startHead) != ""
				draft, draftErr := lookup(gateCtx, wpath, continuedURL)
				var inspection continueRangeInspection
				var inspectErr error
				if headMoved {
					inspection, inspectErr = inspectContinueRange(gateCtx, wpath, continuePullRequestBranch(qw.ContinuePullRequest), startHead, localHead)
				}
				draftBlocks := draftErr == nil && draft && !qw.Delivery.AllowsDraft()
				switch {
				case inspectErr != nil:
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d delivery check failed: %v", qw.ContinuePullRequest.Number, inspectErr)
				case len(inspection.scratchPaths) > 0:
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d commits scratch paths since dispatch: %s", qw.ContinuePullRequest.Number, strings.Join(inspection.scratchPaths, ", "))
				case draftBlocks:
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d is still a draft", qw.ContinuePullRequest.Number)
				case localHead == "" || startHead == "" || strings.EqualFold(localHead, startHead):
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d has no new commit since dispatch", qw.ContinuePullRequest.Number)
				case remoteHead == "" || !strings.EqualFold(remoteHead, localHead):
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d has no new commit since dispatch", qw.ContinuePullRequest.Number)
				case !inspection.deliversUnder(qw.Delivery):
					res.Status = "failed"
					res.FailureMode = FailureBackstop
					res.Error = fmt.Sprintf("continued pull request #%d has no code change since dispatch (only merges or scratch files)", qw.ContinuePullRequest.Number)
				}
			}()
		}
	}

	// 11c-b. Pushed-but-no-PR terminal classification. The backstop only
	// runs for a PR-requiring work type that reached teardown without a PR
	// (shouldBackstop gates on both). If it ran but still could not open
	// one — e.g. a 403 on `gh pr create`, a push failure, or a main/master
	// push refusal — AND the work type's completion contract actually owes a
	// PR, the contract is UNSATISFIED: there is no PR for the v2 exit handler
	// to merge. Without this the run would fall through to the "completed"
	// default below with an empty PullRequestURL, and the exit handler would
	// try to merge PR #0. Report it as an explicit backstop failure and
	// surface the backstop's diagnostics into the terminal envelope
	// (res.Error) so the platform's completion post carries the reason
	// instead of an opaque completed-with-no-PR.
	//
	// The gate is RequiresPRURL (contract owes a PR — today development /
	// inflight), NOT isResultSensitive: QA / acceptance / merge / coordination
	// are result-sensitive yet legitimately produce no new PR. A passing QA or
	// merge session whose PR URL never surfaced would have res.PullRequestURL
	// == "" with a non-nil BackstopReport; gating on isResultSensitive here
	// would flip it to failed → resolveTargetStatus forces effectiveResult
	// "failed" → the issue transitions to Rejected instead of Delivered.
	missingPRs := []string(nil)
	if repositoryDeclaration != nil && RequiresPRURL(qw.WorkType) {
		missingPRs = missingMutablePullRequests(res, *repositoryDeclaration)
	}
	// An exhausted session keeps the failure mode of the bound it ran out
	// of: the backstop's open-PR attempt ran, but the turn never finished.
	if repositoryDeclaration != nil && len(missingPRs) > 0 && !followUps.exhausted {
		res.Status = "failed"
		res.FailureMode = FailureBackstop
		res.Error = "completion contract missing pull requests for mutable repositories: " + strings.Join(missingPRs, ", ")
	} else if res.BackstopReport != nil && res.PullRequestURL == "" && RequiresPRURL(qw.WorkType) {
		res.Status = "failed"
		if res.FailureMode == "" {
			res.FailureMode = FailureBackstop
		}
		if res.Error == "" && res.BackstopReport.Diagnostics != "" {
			res.Error = res.BackstopReport.Diagnostics
		}
	}

	// 11d. Correlation-key capture (ADR-2026-06-10-durable-ci-wait.md).
	// AFTER tail recovery and the backstop — both of which may add
	// commits — capture the worktree's head commit and stamp
	// Result.CommitSHA so the terminal status post carries the key the
	// orchestration layer's durable CI gate correlates
	// workflow_run.completed events against. Nothing pushes after this
	// point (the session is torn down), so the captured sha is the sha
	// CI runs against. Best-effort: a capture failure is logged, never
	// fatal — the platform degrades headSha-less exit events to its
	// timeout/reconciliation path. Background ctx so a cancelled run
	// ctx does not lose the capture.
	if !repositoryFree {
		shaCtx, shaCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if sha, shaErr := captureHeadSHA(shaCtx, wpath); shaErr != nil {
			r.logger.Warn("head commit capture failed",
				"sessionId", qw.SessionID, "err", shaErr)
		} else {
			res.CommitSHA = sha
		}
		shaCancel()
	}

	// 12. Finalise the Result envelope. Status defaults to
	// "completed" when no failure mode was set; otherwise the
	// classifier above has already filled it in.
	if res.Status == "" {
		if streamRes.terminalSuccess {
			res.Status = "completed"
		} else {
			res.Status = "failed"
			if res.FailureMode == "" {
				res.FailureMode = FailureSilentExit
			}
		}
	}

	// Attach the budget enforcement report on the success path.
	// Always non-nil; when
	// .Enforced is false (legacy work, no StageBudget) it serves as a
	// "no budget enforced" observation record. Breach paths attach the
	// report on the failure short-circuit above.
	if res.BudgetReport == nil {
		res.BudgetReport = enforcer.Report(r.now())
	}
	// Per-seat budget evidence: report what the seat actually ran under so
	// the platform can see it. Stamped from the dispatched share; the
	// cooperative caps were applied to the harness env above, and the hard
	// Linux confinement (when any) was applied by the daemon at spawn.
	if qw.SeatBudget != nil && !qw.SeatBudget.disabled() {
		res.SeatBudget = &agent.SeatBudgetReport{
			Mode:     qw.SeatBudget.Mode,
			CPUs:     qw.SeatBudget.CPUs,
			MemoryMB: qw.SeatBudget.MemoryMB,
			Detail:   qw.SeatBudget.Detail,
		}
	}

	// 11b. Post-session Linear state transition. Runs after
	// the Result.Status has been finalised so resolveTargetStatus sees
	// the same "completed"/"failed" classification the platform will
	// receive. Skipped when SkipPostSession is set, or when the runner
	// has no IssueID to address (e.g. governor work types without a
	// Linear-side row).
	if !r.skipPostSession && qw.IssueID != "" {
		r.runPostSession(ctx, qw, res)
	}

	// 11c. Router-learning A2 (write side). POST a routing observation so the
	// platform updates the donmai provider×workType posterior store. Self-gates
	// on ROUTING_RECORDER_ENABLED + required fields; best-effort, never fatal.
	r.recordRoutingFeedback(ctx, qw, res)

	// Update state.json terminal snapshot (best-effort).
	if _, err := r.store.Update(runnerStatePath, func(s *state.State) error {
		s.CurrentStep = "completed"
		if s.ProviderSessionID == "" {
			s.ProviderSessionID = res.ProviderSessionID
		}
		return nil
	}); err != nil {
		r.logger.Debug("state final update failed", "err", err)
	}

	return res, nil
}

// consumeEventsWithStallRetries drains the first turn's event stream and,
// when the stalled-model-request detector fires, stops the hung provider
// and retries the turn through the provider-error path instead of ending
// the seat at the outer idle timeout.
//
// A stall is a model request that never answers the tool result it should
// follow: post-tool silence longer than ProviderStallTimeout with no tool
// call in flight (consumeEvents flags obs.providerStall). The hung request
// is aborted (handle.Stop) and the turn is retried — the same
// stop-and-resume rail steering uses, against the SAME provider-native
// session — with backoff, up to ProviderStallRetries attempts. Each retry
// is recorded on a turnFollowUps so it rides Result.TurnContinuations
// exactly like the provider-error retries it reuses.
//
// A stall the retries exhaust fails the seat as FailureProviderError, not
// FailureNoProgress, so the no-charge rule for provider-side failures
// applies. A slow tool with no output never trips the detector: a tool
// call in flight keeps the window disarmed, and the tool's own bounded
// timeout still ends the CALL with an error the agent sees. Nor does a
// healthy long generation or reasoning pass before the first tool result:
// the detector only arms once a tool result has been observed, so
// pre-tool silence never starts the window.
//
// A stall the detector flags but the rail cannot retry — retries disabled,
// or a Resume the harness refuses — fails the seat as FailureProviderError
// through the same classification the exhausted path uses, never as the
// generic idle cut-off or a context-cancelled timeout.
//
// Only the first turn retries here. Follow-up turns stream through
// consumeEventsWithoutStallDetector with the detector disarmed: tail
// recovery already retries a turn that ends on an explicit provider
// error, and a stall there reads as an unfinished turn the continuation
// bound owns.
func (r *Runner) consumeEventsWithStallRetries(
	ctx context.Context,
	streamCtx context.Context,
	provider agent.Provider,
	handle *agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
	traceProcessor spanEventProcessor,
	spec agent.Spec,
	followUps *turnFollowUps,
) (streamObservation, error) {
	limit := followUps.limit
	for attempt := 0; ; attempt++ {
		obs, err := r.consumeEvents(streamCtx, *handle, worktreePath, qw, res, enforcer, sink, traceProcessor)
		if !obs.providerStall || obs.terminalEvent != nil {
			if followUps.retried > 0 || followUps.exhausted {
				res.TurnContinuations = followUps.report()
			}
			return obs, err
		}
		// The detector flagged a stall: the post-tool model request never
		// answered. Retry it through the stop-and-resume rail while the
		// bound lasts; when the rail cannot run, or the bound is spent,
		// fail as a provider error through one shared helper so every
		// exit reads the same classification, never the generic idle
		// cut-off or a context-cancelled timeout.
		failAsStall := func(o streamObservation, oerr error) (streamObservation, error) {
			followUps.providerError = stallProviderErrorText(r.providerStallTimeout)
			followUps.exhaust(boundRetries)
			res.TurnContinuations = followUps.report()
			o.providerError = followUps.providerError
			o.providerErrorNotRetryable = false
			o.noProgress = true
			o.stallFailed = true
			return o, oerr
		}
		if attempt >= limit {
			r.logger.Warn("model request stalled repeatedly; failing the seat as a provider error",
				"sessionId", qw.SessionID,
				"retries", followUps.retried,
				"limit", followUps.limit,
			)
			return failAsStall(obs, err)
		}
		r.logger.Warn("model request stalled with no tool call in flight; stopping the hung request and retrying",
			"sessionId", qw.SessionID,
			"attempt", followUps.retried+1,
			"limit", followUps.limit,
			"stallTimeout", r.providerStallTimeout.String(),
		)
		if werr := r.waitRetryBackoff(streamCtx, followUps.retried+1); werr != nil {
			// The backoff was cut short by cancellation (or the retry
			// clock): the turn ends on that signal, not on the stall.
			// Still record the provider-side cause on the observation
			// so classification reads provider-error rather than the
			// generic cut-off — a stall interrupted by teardown is
			// still a provider error, not no-progress.
			obs.providerError = stallProviderErrorText(r.providerStallTimeout)
			obs.providerErrorNotRetryable = false
			obs.noProgress = true
			obs.stallFailed = true
			obs.providerStall = false
			if followUps.retried > 0 || followUps.exhausted {
				res.TurnContinuations = followUps.report()
			}
			return obs, werr
		}
		// Stop the hung request before resuming: resumeWithDirective
		// stops the turn itself, but naming the abort here keeps the
		// stall path explicit when that rail is reused elsewhere.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = (*handle).Stop(stopCtx)
		stopCancel()
		next, resumed, rerr := r.resumeWithDirective(ctx, provider, *handle, spec, qw, retryPrompt, nil)
		if rerr != nil || !resumed {
			r.logger.Warn("stalled model request could not resume a new turn; failing the seat as a provider error",
				"sessionId", qw.SessionID,
				"err", rerr,
			)
			// The hung request was already stopped above. Classify the
			// provider-side failure explicitly rather than leaving the
			// generic idle cut-off or a context-cancelled timeout to
			// own it — a stall the rail cannot retry is still a
			// provider error, not no-progress.
			obs.noProgress = true
			return failAsStall(obs, err)
		}
		*handle = next
		followUps.retried++
		obs.providerStall = false
	}
}

// stallRetryLimit is the bound consumeEventsWithStallRetries applies to
// stop-and-retry attempts after a stalled model request. Zero is the
// default; a negative retry count disables the retries.
func (r *Runner) stallRetryLimit() int {
	switch {
	case r.providerStallRetries == 0:
		return DefaultProviderStallRetries
	case r.providerStallRetries < 0:
		return 0
	default:
		return r.providerStallRetries
	}
}

// stallProviderErrorText is the provider-error text recorded when a stalled
// model request exhausts its retries.
func stallProviderErrorText(timeout time.Duration) string {
	return fmt.Sprintf("model request stalled: no provider response within %s and no tool call in flight", timeout)
}

// classifyStreamStop stamps the terminal failure of a turn whose stream
// stopped on lost ownership (or an operator cancel), the idle watchdog or a
// cancelled context, stops the provider where tokens could keep running, and
// reports whether it did so; the caller then returns (res, err). It
// classifies the first turn and every runner-driven follow-up turn alike
// (turn_continuation.go).
//
// A budget cap is not a terminal failure here: the provider is stopped and
// the cap is returned as budget, so the caller resolves the turn the cap
// ended — the work may already be delivered — and starts no further turn.
func (r *Runner) classifyStreamStop(
	qw QueuedWork,
	res *Result,
	handle agent.Handle,
	enforcer *BudgetEnforcer,
	pulser *heartbeat.Pulser,
	lostOwnership <-chan struct{},
	streamRes streamObservation,
	streamErr error,
) (stopped bool, budget *BudgetExceededError, err error) {
	// Disambiguate between ctx-cancelled and lost-ownership before
	// classifying the failure mode.
	select {
	case <-lostOwnership:
		res.Status = "failed"
		// Distinguish a deterministic operator cancel ({"stop": true} on
		// the lock-refresh, surfaced via Pulser.StopRequested) from the
		// 3-strike heartbeat fuse / hand-off. Operator cancel is an
		// intentional terminal outcome the platform MUST NOT
		// blind-re-dispatch, so it gets its own FailureMode (mirroring
		// FailureAgentBlocked routing); the fuse stays FailureLostOwnership.
		if pulser != nil && pulser.StopRequested() {
			res.FailureMode = FailureOperatorCancelled
			if res.Error == "" {
				res.Error = "operator cancelled session (lock-refresh stop=true)"
			}
		} else {
			res.FailureMode = FailureLostOwnership
			if res.Error == "" {
				res.Error = heartbeat.ErrLostOwnership.Error()
			}
		}
		// Best-effort stop the provider so it doesn't keep tokens
		// running.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		return true, nil, heartbeat.ErrLostOwnership
	default:
	}

	// Budget cap. Checked before the watchdog and the generic timeout: a
	// wall-clock cap surfaces as the stream ctx's deadline, and a token cap
	// the meter crossed mid-turn stays the reason even when the turn then
	// wedged before its boundary.
	if budget = r.stopAtBudget(qw, handle, enforcer, streamErr); budget != nil {
		return false, budget, nil
	}

	// Idle/no-progress watchdog cut-off. The watchdog cancels the stream
	// ctx (surfacing context.Canceled), so this must be checked BEFORE
	// the generic ctx-cancelled timeout branch below to classify the
	// wedged-but-channel-alive session as FailureNoProgress rather than
	// FailureTimeout. Stop the provider so it doesn't keep burning tokens.
	// A stall-detector cut-off (obs.providerStall) is NOT terminal here on
	// its own: consumeEventsWithStallRetries retries it above through the
	// stop-and-resume rail, and only a stall that exhausted its retries —
	// or one the rail could not retry (retries disabled, Resume refused)
	// — reaches this branch, marked obs.stallFailed and carrying the
	// provider-error text consumeEventsWithStallRetries recorded. Only that
	// mark reads as a provider error: an idle cut-off on any other stream
	// stays FailureNoProgress even when an earlier provider-error
	// observation is still on it (a harness that reported an error, began
	// its own retry and then went silent), exactly as before the detector
	// existed. A tail or inject drain runs with the detector disarmed and
	// never sets the mark.
	if streamRes.noProgress {
		res.Status = "failed"
		if streamRes.stallFailed {
			res.FailureMode = FailureProviderError
			if res.Error == "" {
				res.Error = streamRes.providerError
			}
		} else {
			res.FailureMode = FailureNoProgress
			if res.Error == "" {
				res.Error = fmt.Sprintf("no agent event within idle timeout (%s)", r.idleTimeout)
			}
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		return true, nil, streamErr
	}

	if streamErr != nil && errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
		res.Status = "failed"
		res.FailureMode = FailureTimeout
		if res.Error == "" {
			res.Error = streamErr.Error()
		}
		return true, nil, streamErr
	}
	return false, nil, nil
}

// stopAtBudget reports the budget cap that ended the latest turn, if one
// did, and stops the provider so it spends nothing more. The cap is the
// enforcer's error the stream returned, a wall-clock cap its deadline
// tripped (CheckDuration), or a breach the enforcer recorded during a turn
// whose stream error the caller did not see (a memory-inject turn) or that
// stopped for another reason after the cap was crossed. nil when the
// session is within its budget.
func (r *Runner) stopAtBudget(qw QueuedWork, handle agent.Handle, enforcer *BudgetEnforcer, streamErr error) *BudgetExceededError {
	var budget *BudgetExceededError
	if !errors.As(streamErr, &budget) {
		if errors.Is(streamErr, context.DeadlineExceeded) {
			budget = enforcer.CheckDuration(r.now())
		}
		if budget == nil {
			budget = enforcer.breached()
		}
	}
	if budget == nil {
		return nil
	}
	// Best-effort stop the provider so it doesn't keep tokens running past
	// the cap.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = handle.Stop(stopCtx)
	stopCancel()
	r.logger.Warn("[runner-stage]",
		"sid", qw.SessionID,
		"stageId", qw.StageID,
		"event", "budget.breach",
		"cap", string(budget.Cap),
		"detail", budget.Detail,
	)
	return budget
}

// deliveredAtBudgetStop reports whether a session a budget cap ended had
// already delivered its work: work that owes a pull request, with the
// session's own verified pull request (one per mutable repository of a
// declared workarea), a passed turn result, and no failure the runner
// recorded. Such a session ends completed with the breach recorded, not
// failed: the cap stopped work that was already done.
func deliveredAtBudgetStop(qw QueuedWork, res *Result, declaration *workarea.NormalizedDeclaration) bool {
	if !RequiresPRURL(qw.WorkType) || res.PullRequestURL == "" || res.WorkResult != "passed" {
		return false
	}
	if res.FailureMode != "" || res.Status == "failed" {
		return false
	}
	return declaration == nil || len(missingMutablePullRequests(res, *declaration)) == 0
}

// takesMidTurnWrapUp reports whether a harness that declares nd takes the
// wrap-up request while its turn is still running. Only a steer channel
// does: pi's RPC steer delivers a message after the current model turn's
// tool calls and before its next model call. Every other mechanism either
// runs a second writer against a live turn (resume-inject), queues the
// message behind the whole turn, or delivers nothing, so there the request
// rides the next follow-up prompt instead.
func takesMidTurnWrapUp(nd agent.NoticeDelivery) bool {
	return nd == agent.NoticeDeliveryRPCSteer
}

func (r *Runner) protectedRuntimeMCPV2Applies(qw QueuedWork, selection harnessSelection) bool {
	if !r.protectedRuntimeMCPV2Selector.configured() || qw.toolLifecycleProfileID != r.protectedRuntimeMCPV2Selector.AdapterProfileID {
		return false
	}
	return protectedRuntimeMCPTargetsSession(qw, selection, r.protectedRuntimeMCPV2Selector.realizationSelector())
}

// newInjectAcceptor builds the heartbeat's OnInject callback: the PRODUCTION
// implementation of the runtime-inject accept contract, extracted so tests
// exercise this function rather than a hand-copied replica of it (a mirrored
// copy in a test keeps passing after the real closure is deleted).
//
// The returned func runs on the heartbeat goroutine. It owns seenInject —
// dedup-by-DeliveryID lives here exclusively, so the map is never touched off
// that goroutine — and performs a NON-BLOCKING send onto injectCh:
//
//   - buffered → marked seen; acked only if ackOnBuffer.
//   - already-seen DeliveryID → not re-buffered; acked only if ackOnBuffer.
//   - buffer full → ALWAYS rejected (returns false) rather than stalling the
//     heartbeat loop; the pulser leaves it unacked and the producer re-offers
//     it on a later refresh. The DeliveryID is deliberately NOT marked seen,
//     so the re-delivery is accepted once capacity frees up.
//
// # ackOnBuffer: where the ack belongs
//
// Consumers differ by run mode: headless drains at the post-terminal seam,
// interview parks on the channel per turn, interactive writes each payload
// into the live PTY as a notice. The first two consume the buffer within the
// same Run, at a seam the runner itself reaches — buffering there is a good
// proxy for delivery, so they pass ackOnBuffer=true.
//
// The interactive consumer is different in kind: its write is gated on the
// human at the terminal (a notice is refused while they are mid-composition)
// and the session can end at any moment. Acking at buffer time stamped up to
// nine payloads delivered, the session ended, they were logged as a count and
// dropped, and the platform never re-offered them because acked_at was set.
// So interactive passes ackOnBuffer=false: the acceptor takes custody without
// claiming delivery, and the consumer calls [heartbeat.Pulser.AckInject] once
// the bytes are actually on the PTY. Until then the payload stays unacked and
// requeueable — ack-or-requeue rather than ack-and-hope.
func newInjectAcceptor(
	injectCh chan<- heartbeat.InjectPayload,
	seenInject map[string]struct{},
	logger *slog.Logger,
	sessionID string,
	ackOnBuffer bool,
) func(heartbeat.InjectPayload) bool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(p heartbeat.InjectPayload) bool {
		if p.DeliveryID != "" {
			if _, ok := seenInject[p.DeliveryID]; ok {
				logger.Debug("memory inject: skipping already-seen delivery",
					"sessionId", sessionID, "deliveryId", p.DeliveryID,
					"ackOnBuffer", ackOnBuffer)
				return ackOnBuffer
			}
		}
		select {
		case injectCh <- p:
			if p.DeliveryID != "" {
				seenInject[p.DeliveryID] = struct{}{}
			}
			return ackOnBuffer
		default:
			logger.Warn("memory inject: channel full, leaving unacked for re-delivery",
				"sessionId", sessionID, "deliveryId", p.DeliveryID)
			return false
		}
	}
}

// drainMemoryInjects delivers every memory block the heartbeat transport
// buffered onto injectCh during the just-completed turn, then re-consumes
// the resume turn's events. It is invoked at the post-terminal seam (the
// turn has reached a ResultEvent) on the single runner goroutine, so all
// handle.Inject calls remain serialised (claude is single-in-flight).
//
// For each buffered block: inject via the shared injectDirective helper
// (non-fatal on ErrUnsupported / ErrSessionNotReady / ErrInjectInFlight),
// then drain the events the inject produced via consumeEvents. The returned
// observation merges all resume turns; the caller applies it onto the
// Result so a memory-driven follow-up turn's PR/cost/summary lands.
//
// Liveness guard: a cancelled ctx aborts the drain (the events emitted
// after Stop / ctx-cancel are silently dropped by the provider, so there is
// no point injecting into a dead handle). Empty-text blocks are skipped.
func (r *Runner) drainMemoryInjects(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
	traceProcessor spanEventProcessor,
	injectCh <-chan heartbeat.InjectPayload,
) streamObservation {
	var merged streamObservation
	for {
		select {
		case <-ctx.Done():
			// Handle is being torn down — do not inject into a dead session.
			return merged
		case p := <-injectCh:
			if strings.TrimSpace(p.Text) == "" {
				r.logger.Debug("memory inject: skipping empty block",
					"sessionId", qw.SessionID, "deliveryId", p.DeliveryID)
				continue
			}
			r.logger.Info("memory inject: delivering block",
				"sessionId", qw.SessionID,
				"deliveryId", p.DeliveryID,
				"len", len(p.Text),
			)
			if err := r.injectDirective(ctx, handle, p.Text); err != nil {
				// Non-benign inject failure — log and stop draining; the
				// remaining blocks ride the next heartbeat re-delivery.
				r.logger.Warn("memory inject: delivery failed",
					"sessionId", qw.SessionID,
					"deliveryId", p.DeliveryID,
					"err", err,
				)
				return merged
			}
			// Re-consume the resume turn's events so the follow-up work
			// (commit/PR/cost) is observed + mirrored. Each turn is applied
			// as it ends so an earlier turn's pull request, verdict or error
			// still reaches the envelope when a later turn carries none;
			// consumeEvents already counted the turn's tool calls.
			// The stall detector stays disarmed here: tail recovery owns
			// follow-up liveness through its continuation bound, and a
			// post-tool silence on a follow-up turn is an unfinished
			// turn — never a provider error to retry outside that bound.
			injRes, _ := r.consumeEventsWithoutStallDetector(ctx, handle, worktreePath, qw, res, enforcer, sink, traceProcessor)
			injRes.applyTo(res, res.ProviderName)
			merged = injRes
			if enforcer != nil && enforcer.breached() != nil {
				// A budget cap ended the turn: deliver nothing more.
				return merged
			}
		default:
			// No more buffered injects.
			return merged
		}
	}
}

// streamObservation captures the per-event-stream observations
// runner.runLoop accumulates while consuming the provider's events
// channel. Pulled out into its own struct so steering and backstop
// can read the same data without re-scanning the events log.
type streamObservation struct {
	terminalSuccess bool
	terminalEvent   *agent.ResultEvent
	errorEvent      *agent.ErrorEvent
	pullRequestURL  string
	// pullRequestCandidates is every GitHub pull request URL the stream
	// carried (assistant text and tool output), oldest first, each once at
	// its latest position. They are candidates only: the session pull
	// request verifier decides which, if any, is the session's own.
	pullRequestCandidates []string
	commentPosted         bool
	issueUpdated          bool
	subIssuesMade         bool
	// workResult is this stream's passed|failed|unknown verdict: the one
	// carried by the LATEST assistant message with a line-anchored marker
	// (the FIRST anchored marker within that message wins — see
	// scanVerdict). A blocked verdict sets blocked instead and clears it.
	workResult string
	// reviewVerdict is this stream's structured review outcome
	// (scanReviewVerdict): one of APPROVE | APPROVE_WITH_FOLLOWUPS |
	// REQUEST_CHANGES from the LATEST assistant message carrying a
	// line-anchored REVIEW_VERDICT marker. Empty when the turn gave none.
	reviewVerdict string
	providerID    string
	// lastAssistantText is the most recent non-empty assistant message
	// observed on this stream. It is the summary fallback for providers
	// whose terminal ResultEvent carries no Message (codex's
	// turn/completed maps to ResultEvent{Success, Cost} with no text) —
	// without it the session's Summary posts empty and the platform's
	// exit CloudEvent derives result=unknown even though the agent's
	// final message carried the WORK_RESULT marker (2026-06-10 codex
	// qa/acceptance rehearsals).
	lastAssistantText string
	// blocked is set when the agent emitted an explicit decline marker
	// ("WORK_RESULT:blocked" or "AGENT_BLOCKED: …") — a deliberate,
	// reasoned refusal to proceed (ambiguous spec, unmet preconditions)
	// rather than a crash or silent exit. The runner reads it in the
	// post-stream classification to fork to FailureAgentBlocked and to
	// suppress steering/backstop (nothing to recover).
	blocked bool
	// blockedReason is the human-readable reason captured from an
	// "AGENT_BLOCKED: <reason>" marker, surfaced on Result.Error.
	blockedReason string
	// budgetBreach is set when the in-flight enforcer tripped a cap
	// during ObserveEvent. consumeEvents then ends the turn at its next
	// boundary and returns the breach, which the runner's budget stop
	// (stopAtBudget) acts on.
	budgetBreach *BudgetExceededError
	// providerError is the provider error text of a model call that ended
	// on a provider error (agent.SystemSubtypeProviderError) with no
	// assistant message or tool call after it in this stream — the turn
	// ended on that error rather than because the agent stopped.
	// providerErrorNotRetryable is set when that error is one a new
	// attempt cannot fix (splitProviderErrorRetryable): where the runner
	// would retry the turn, it ends it at once with the error recorded.
	// The zero value is retryable, as every provider error was before.
	providerError             string
	providerErrorNotRetryable bool
	// upstream is the structured endpoint error of the latest provider
	// error observation (or failed terminal) in this stream — the HTTP
	// status, provider code, truncated provider message, and reset time
	// the harness exposed. A later assistant message or tool call means
	// the model recovered and clears it, exactly like providerError.
	upstream *agent.UpstreamError
	// toolCalls counts the tool calls (agent.ToolUseEvent) this stream
	// carried. Tail recovery reads a turn with at least one as productive
	// (turn_continuation.go).
	toolCalls int
	// noProgress is set when the idle/no-progress watchdog fired — the
	// event stream produced no agent.Event for longer than the runner's
	// IdleTimeout window. The runner reads it in the post-stream
	// classification path to fork to FailureNoProgress instead of the
	// generic FailureTimeout (ctx-cancelled) branch, so a wedged session
	// is routed distinctly from a deadline expiry.
	noProgress bool
	// providerStall is set when the stalled-model-request detector fired:
	// no agent.Event arrived within the ProviderStallTimeout window while
	// no tool call was in flight, so a hung model request — not a slow
	// tool — owns the silence. The runner reads it alongside providerError
	// to retry the turn through the provider-error path instead of ending
	// the seat at the outer idle timeout.
	providerStall bool
	// stallFailed is set by consumeEventsWithStallRetries when it ends the
	// turn on a stall it could not retry (the bound is spent, retries are
	// disabled, Resume was refused, or the backoff was cut short). With
	// noProgress it is what classifyStreamStop reads as FailureProviderError.
	stallFailed bool
}

// splitProviderErrorRetryable separates the harness-reported retryability
// marker (agent.ProviderErrorNotRetryableSuffix) from the provider's own
// error text and reports whether a new attempt cannot fix the failure. A
// context overflow is always retryable (providerretry.ContextOverflow); else
// an explicit marker wins; else the text is judged by
// providerretry.TextRetryable, which reads a status only from a strictly
// anchored form, so a port or a number in prose never makes a network error
// fatal.
func splitProviderErrorRetryable(detail string) (text string, notRetryable bool) {
	text, marked := strings.CutSuffix(detail, agent.ProviderErrorNotRetryableSuffix)
	text = strings.TrimSpace(text)
	switch {
	case providerretry.ContextOverflow(text):
		return text, false
	case marked:
		return text, true
	default:
		return text, !providerretry.TextRetryable(text)
	}
}

// verdict is the stream's single verdict: "blocked" when its latest anchored
// marker declined, else its passed|failed|unknown work-result, else "".
func (o streamObservation) verdict() string {
	if o.blocked {
		return "blocked"
	}
	return o.workResult
}

// applyTo merges the observation into a Result envelope. Idempotent
// when called multiple times (e.g. after steering re-consumes events).
// The session's tool-call count is not applied here: consumeEvents meters
// it onto the envelope for every stream, on every exit path.
func (o streamObservation) applyTo(res *Result, providerName agent.ProviderName) {
	if res.ProviderName == "" {
		res.ProviderName = providerName
	}
	if o.providerID != "" && res.ProviderSessionID == "" {
		res.ProviderSessionID = o.providerID
	}
	if o.pullRequestURL != "" {
		res.PullRequestURL = o.pullRequestURL
	}
	if o.workResult != "" {
		res.WorkResult = o.workResult
	}
	if o.reviewVerdict != "" {
		res.ReviewVerdict = o.reviewVerdict
	}
	// Cost is not taken from the stream: the session's usage meter (the
	// budget enforcer) counts every turn, and runLoop reports its total.
	//
	// Terminal summary stamping. The terminal event's message is
	// authoritative and LAST-wins: when a background-poll wakeup (memory
	// inject / steering) produces a resume turn, its terminal message is
	// the TRUE final assistant message and must replace the stale
	// pre-wakeup summary (2026-06-10 rehearsal 3 — the stale text was
	// re-emitted as the close response with no result marker).
	//
	// When the terminal event carries no message (codex), fall back to
	// the latest assistant text observed on this stream so the summary —
	// and the WORK_RESULT marker the agent's final message ends with —
	// still reach the platform's exit event.
	switch {
	case o.terminalEvent != nil && o.terminalEvent.Message != "":
		res.Summary = o.terminalEvent.Message
	case o.lastAssistantText != "" && (o.terminalEvent != nil || res.Summary == ""):
		res.Summary = o.lastAssistantText
	}
	// ErrorEvent is a non-recoverable provider terminal, unlike a tool result
	// with IsError. A follow-up failure must survive the initial turn's success
	// when runLoop later finalizes the one session envelope.
	if o.errorEvent != nil && (res.Status == "" || res.Status == "completed") {
		res.Status = "failed"
	}
	if o.errorEvent != nil && res.Error == "" {
		res.Error = o.errorEvent.Message
		if res.FailureMode == "" {
			res.FailureMode = FailureProviderError
		}
	}
	// The structured endpoint cause rides only when the session failed
	// on it: a terminal failure (or a provider-error exhaustion) with
	// this stream's structured cause and no newer recovery. A later
	// successful turn clears it (observeEvent clears upstream alongside
	// providerError), and a failure with no structured cause leaves any
	// older value untouched.
	if res.Status == "failed" && res.Upstream == nil && o.upstream != nil && !o.upstream.Empty() {
		res.Upstream = agent.CanonicalUpstreamError(o.upstream)
	}
}

// consumeEvents drains the handle's events channel with the
// stalled-model-request detector armed at the Runner's configured window
// (see consumeEventsWithin).
func (r *Runner) consumeEvents(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
	traceProcessor spanEventProcessor,
) (streamObservation, error) {
	return r.consumeEventsWithin(ctx, r.providerStallTimeout, handle, worktreePath, qw, res, enforcer, sink, traceProcessor)
}

// consumeEventsWithoutStallDetector drains a follow-up turn's events with
// the stalled-model-request detector disarmed: tail recovery (the steering
// re-consume, the deliverFollowUp drain, the memory-inject drain) owns
// follow-up liveness through its continuation bound, so a post-tool
// silence there is an unfinished turn that bound owns — never a provider
// error to retry outside it, and never a provider-error classification on
// the way out. Only the first turn arms the detector, through
// consumeEventsWithStallRetries. The window is passed per call, never
// written to the Runner, which concurrent sessions share.
func (r *Runner) consumeEventsWithoutStallDetector(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
	traceProcessor spanEventProcessor,
) (streamObservation, error) {
	return r.consumeEventsWithin(ctx, 0, handle, worktreePath, qw, res, enforcer, sink, traceProcessor)
}

// consumeEventsWithin drains the handle's events channel, mirrors each
// event to .agent/events.jsonl + state store, and returns the
// observation summary on terminal event or channel close.
//
// Returns the observation and the ctx err (if cancellation tripped
// the loop). A nil err with terminalSuccess=false means the channel
// closed without a terminal Result — the caller classifies as
// FailureSilentExit.
//
// Every event also feeds the budget enforcer. A sub-agent cap ends the
// stream at once; a token cap the meter crossed ends it at the turn's next
// boundary (atTurnBoundary), returning the *BudgetExceededError. Past the
// wrap-up point, on a harness that takes a message into a running turn, the
// agent is asked to wrap up at its next tool call (wrapUpMidTurn).
//
// The stream's tool calls are added to res.ToolCalls (res may be nil) on
// every return: each stream counts exactly once, including a turn the
// caller stops on (no progress, timeout, lost ownership, cancel) before
// applying its observation.
//
// stallTimeout is the stalled-model-request window for this drain; zero
// or negative disarms the detector.
func (r *Runner) consumeEventsWithin(
	ctx context.Context,
	stallTimeout time.Duration,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	res *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
	traceProcessor spanEventProcessor,
) (streamObservation, error) {
	if sink == nil {
		sink = noopSink{}
	}
	if traceProcessor == nil {
		traceProcessor = noopSpanProcessor{}
	}
	obs := streamObservation{}
	if res != nil {
		defer func() { res.ToolCalls += obs.toolCalls }()
	}

	// Open the events.jsonl audit file under <worktree>/.agent/.
	jsonlPath := filepath.Join(worktreePath, state.AgentDirName, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o750); err != nil {
		r.logger.Warn("events.jsonl mkdir failed", "err", err)
	}
	//nolint:gosec // G304: path is owned by the runner via worktree manager.
	jsonlFile, err := os.OpenFile(jsonlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.logger.Warn("events.jsonl open failed", "err", err)
	} else {
		defer func() { _ = jsonlFile.Close() }()
	}
	var jsonlMu sync.Mutex
	appendJSONL := func(ev agent.Event) {
		if jsonlFile == nil {
			return
		}
		body, err := agent.MarshalEvent(ev)
		if err != nil {
			return
		}
		jsonlMu.Lock()
		defer jsonlMu.Unlock()
		_, _ = jsonlFile.Write(append(body, '\n'))
	}

	// Idle/no-progress watchdog. A resettable timer is reset on every
	// agent.Event; if no event arrives within r.idleTimeout the session
	// is wedged-but-channel-alive (the events channel is still open, so
	// it is not a silent exit, but forward progress has stopped). A tool
	// call in flight also re-arms the timer: ToolUseEvent marks the call
	// start and the matching ToolResultEvent (or a terminal/assistant-text
	// event) clears it, so a legitimate long tool call that streams no
	// intermediate events does not trip the timer — the bounded tool timeout
	// (policy extension, 300s) still ends the CALL with an error the agent
	// sees, while the idle watchdog keeps owning the session. On
	// expiry we cancel a stream-scoped context and flag obs.noProgress so
	// the caller classifies FailureNoProgress instead of the generic
	// FailureTimeout. A non-positive r.idleTimeout disables the watchdog
	// (idleC stays nil → its select case never fires).
	//
	// The stalled-model-request detector shares the same timer source. It
	// is armed to stallTimeout only AFTER a tool result has
	// been observed while no tool call is in flight, and reset on every
	// agent.Event while that post-tool silence lasts — the narrow window
	// the reported hangs share: the last recorded activity is a tool
	// result, then a model request that never returns. It fires ONLY
	// while no tool call is in flight: a hung model request after a tool
	// result trips it, while a slow tool never does. Stretches with no
	// tool result yet (the first model generation, a reasoning pass
	// before the first tool call) never arm it: no harness emits an
	// event while a response streams, so an event-silence window there
	// cannot tell a hung request from a healthy long generation, and
	// arming it would abort healthy turns. On expiry the stream is
	// flagged obs.providerStall so the caller stops the provider and
	// retries the turn through the provider-error path instead of ending
	// the seat at the outer idle timeout. A non-positive
	// stallTimeout disables the detector (stallC stays nil →
	// its select case never fires), and a zero tool-in-flight count at
	// expiry is what distinguishes it from the idle watchdog.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	var idleTimer interviewTimer
	var idleC <-chan time.Time
	if r.idleTimeout > 0 {
		idleTimer = r.idleTimer(r.idleTimeout)
		defer idleTimer.Stop()
		idleC = idleTimer.Chan()
	}
	// stallTimer is created lazily: it stays nil until the first tool
	// result while no tool call is in flight, so the pre-tool silence
	// of a healthy generation never starts the window. armStall creates
	// it on that first post-tool result; disarmStall stops it when a
	// new tool call starts or the turn ends, so each post-tool window
	// starts fresh.
	var stallTimer interviewTimer
	var stallC <-chan time.Time
	armStall := func() {
		if stallTimeout <= 0 {
			return
		}
		if stallTimer == nil {
			stallTimer = r.idleTimer(stallTimeout)
			stallC = stallTimer.Chan()
			return
		}
		stallTimer.Reset(stallTimeout)
	}
	disarmStall := func() {
		if stallTimer == nil {
			return
		}
		stallTimer.Stop()
		stallTimer = nil
		stallC = nil
	}
	// resetIdle re-arms the watchdog after each observed event. Drains a
	// possibly-already-fired timer tick before Reset so a stale fire from
	// the prior window cannot trip the next select.
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		idleTimer.Reset(r.idleTimeout)
	}
	// resetStall re-arms the stall detector after each observed event
	// while the post-tool window is armed. It is a no-op before the
	// first post-tool result arms the timer.
	resetStall := func() {
		if stallTimer == nil {
			return
		}
		stallTimer.Reset(stallTimeout)
	}
	// toolInFlight tracks tool calls that started (ToolUseEvent) without a
	// matching result yet, keyed by tool-use id. A call in flight re-arms
	// the watchdog on expiry instead of ending the session: the bounded
	// tool timeout still ends the CALL with an error the agent sees.
	// sawToolResult records that a tool result arrived while no call
	// remains in flight: the model request that should follow is the one
	// the stall detector watches. It arms the detector on the result and
	// stays set until a new tool call starts or the turn ends.
	toolInFlight := map[string]struct{}{}
	sawToolResult := false
	// trackToolEvent maintains toolInFlight from the correlated stream.
	// ToolUseID may be empty on some harnesses; those calls share the ""
	// key as a single in-flight slot, which is enough to suppress the
	// watchdog while any unidentified call runs. A tool result that leaves
	// no call in flight arms the stall detector: the silence that follows
	// is the model request the reported hangs never answer. Any model
	// output — assistant text, a usage report for a completed model call,
	// quota or progress signals — or a new tool call disarms it: the
	// model is answering again, or the silence belongs to the tool.
	trackToolEvent := func(ev agent.Event) {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			toolInFlight[e.ToolUseID] = struct{}{}
			sawToolResult = false
			disarmStall()
		case agent.ToolResultEvent:
			delete(toolInFlight, e.ToolUseID)
			if len(toolInFlight) == 0 {
				sawToolResult = true
				armStall()
			}
		case agent.ResultEvent:
			clear(toolInFlight)
			sawToolResult = false
			disarmStall()
		case agent.ErrorEvent:
			// A call still runs past an error the session continues past.
			if !e.SessionContinues {
				clear(toolInFlight)
				sawToolResult = false
				disarmStall()
			}
		case agent.AssistantTextEvent,
			agent.LlmCallEvent,
			agent.UsageEvent,
			agent.ToolProgressEvent,
			agent.SubagentEvent:
			// Model output ends the post-tool silence the detector
			// watches: a response is arriving, so there is no hung
			// request to retry. Usage, progress and sub-agent
			// lifecycle events only arrive while the provider runs,
			// never from a wedged request.
			sawToolResult = false
			disarmStall()
		case agent.SystemEvent:
			// A provider-error observation means the model call
			// ANSWERED with an error: the request is not hung, and
			// the turn ends on that error through the normal
			// provider-error path. Disarm so the detector cannot
			// race it. All other system chatter (compaction,
			// rate-limit notices, harness lifecycle) is not model
			// output and leaves the window armed.
			if e.Subtype == agent.SystemSubtypeProviderError {
				sawToolResult = false
				disarmStall()
			}
		}
	}
	// wrapUpTried limits the mid-turn wrap-up request to one attempt per
	// turn; a request the harness refused rides the next follow-up prompt.
	wrapUpTried := false

	for {
		select {
		case <-watchCtx.Done():
			// watchCtx is cancelled either because the parent ctx was
			// cancelled (timeout / lost-ownership / budget) or because a
			// watchdog fired below. obs.noProgress / obs.providerStall
			// disambiguate the latter two.
			return obs, watchCtx.Err()
		case <-stallC:
			if len(toolInFlight) > 0 || !sawToolResult {
				// A tool call is still running, or no tool result has
				// been seen yet: the silence belongs to the tool or
				// to a pre-tool generation, not to a hung post-tool
				// model request. Re-arm when the window is armed and
				// keep waiting — a slow tool with no output and a
				// healthy long generation must never trip the stall
				// detector, and the tool's own bounded timeout still
				// ends the CALL with an error the agent sees.
				resetStall()
				r.logger.Debug("provider stall detector: no post-tool window — re-arming",
					"sessionId", qw.SessionID,
					"inFlight", len(toolInFlight),
					"sawToolResult", sawToolResult,
				)
				continue
			}
			r.logger.Warn("provider stall detector: no model response within window and no tool call in flight — flagging a stalled request",
				"sessionId", qw.SessionID,
				"stallTimeout", stallTimeout.String(),
			)
			obs.providerStall = true
			watchCancel()
			return obs, watchCtx.Err()
		case <-idleC:
			if len(toolInFlight) > 0 {
				// A tool call is still running: treat the call as
				// progress, not a stall. Re-arm and keep waiting — the
				// tool's own bounded timeout ends the CALL with an
				// error the agent can continue from.
				resetIdle()
				r.logger.Info("idle watchdog: tool call in flight — re-arming, not cancelling",
					"sessionId", qw.SessionID,
					"inFlight", len(toolInFlight),
				)
				continue
			}
			r.logger.Warn("idle watchdog: no event within window — cancelling stream",
				"sessionId", qw.SessionID,
				"idleTimeout", r.idleTimeout.String(),
			)
			obs.noProgress = true
			watchCancel()
			return obs, watchCtx.Err()
		case ev, ok := <-handle.Events():
			if !ok {
				return obs, nil
			}
			// Forward progress observed — re-arm the idle watchdog, and
			// the stall detector while its post-tool window is armed.
			resetIdle()
			resetStall()
			for _, correlatedEvent := range traceProcessor.Process(ev) {
				trackToolEvent(correlatedEvent)
				appendJSONL(correlatedEvent)
				r.observeEvent(correlatedEvent, &obs, worktreePath, qw)
				// Forward sparse quota updates to the admitting daemon:
				// a codex `account/rateLimits/updated` notification or
				// a claude `rate_limit_event` arrives here as a
				// UsageEvent and merges by window id onto the probe
				// snapshot behind the heartbeat quota field.
				r.reportQuotaEvent(watchCtx, qw.SessionID, correlatedEvent)
				// Push every correlated/synthetic event to the platform's
				// activity buffer. LlmCallEvent intentionally maps to no legacy
				// activity, while tool events retain their stamped IDs.
				sink.Send(watchCtx, correlatedEvent)
				if enforcer != nil {
					if berr := enforcer.ObserveEvent(correlatedEvent); berr != nil {
						if berr.Cap != CapTokens {
							obs.budgetBreach = berr
							return obs, berr
						}
						// Keep the crossing: later calls repeat the breach
						// with a larger count.
						if obs.budgetBreach == nil {
							obs.budgetBreach = berr
						}
					}
					// A token cap the meter crossed mid-turn ends the
					// turn at its next boundary, not at once: a tool call
					// already running is never cut off.
					if obs.budgetBreach != nil && atTurnBoundary(correlatedEvent, len(toolInFlight)) {
						return obs, obs.budgetBreach
					}
					if !wrapUpTried && obs.budgetBreach == nil && r.wrapUpMidTurn(watchCtx, handle, enforcer, qw, correlatedEvent) {
						wrapUpTried = true
					}
				}
				if _, terminal := correlatedEvent.(agent.ResultEvent); terminal {
					return obs, nil
				}
			}
		}
	}
}

// atTurnBoundary reports whether the stream is at a turn boundary once ev
// is applied: the turn ended (ResultEvent), or a model call just reported
// its usage (a harness-reported, non-synthetic LlmCallEvent) with no tool
// call running. It is where the runner stops a turn the token cap ended.
// Harnesses report a model call's usage at different points — pi after the
// call's tool calls ran, a Claude-style stream before them — so at the call
// that crossed the cap, the tool calls that call requested may not run; the
// usage the call spent is metered either way. A tool call that is running is
// never cut off.
func atTurnBoundary(ev agent.Event, toolsInFlight int) bool {
	switch e := ev.(type) {
	case agent.ResultEvent:
		return true
	case agent.LlmCallEvent:
		return !e.Synthetic && e.UsageSource != agent.LlmUsageAggregate && toolsInFlight == 0
	default:
		return false
	}
}

// wrapUpMidTurn asks the agent to wrap up while its turn is still running,
// once the token meter has passed the wrap-up point: on a harness that
// takes a message into a running turn (takesMidTurnWrapUp), at a tool call
// — the turn is certainly in flight then, so the request reaches the model
// before its next call. It reports whether it tried; a refused request stays
// due and rides the next follow-up prompt.
func (r *Runner) wrapUpMidTurn(ctx context.Context, handle agent.Handle, enforcer *BudgetEnforcer, qw QueuedWork, ev agent.Event) bool {
	if _, toolCall := ev.(agent.ToolUseEvent); !toolCall || !enforcer.midTurnWrapUp || !enforcer.wrapUpDue() {
		return false
	}
	if err := handle.Inject(ctx, wrapUpPrompt); err != nil {
		r.logger.Warn("mid-turn wrap-up request not delivered; it rides the next follow-up prompt",
			"sessionId", qw.SessionID, "err", err)
		return true
	}
	enforcer.wrapUpSent()
	r.logger.Info("token budget nearly spent; asked the agent to wrap up mid-turn",
		"sessionId", qw.SessionID, "maxTokens", enforcer.limits.MaxTokens)
	return true
}

// observeEvent applies a single event to the observation accumulator.
// Side effects:
//   - InitEvent → captures provider session id; mirrors to state.json.
//   - ToolUseEvent → tracks comment/issue/sub-issue flags and
//     extracts a PR URL when the agent invokes `gh pr create`.
//   - AssistantTextEvent → scans for the WORK_RESULT marker and
//     accumulates the agent's running narrative.
//   - ResultEvent → captures terminal cost/success.
//   - ErrorEvent → records for FailureProviderError classification,
//     unless it says the session continues (SessionContinues).
func (r *Runner) observeEvent(ev agent.Event, obs *streamObservation, worktreePath string, _ QueuedWork) {
	switch e := ev.(type) {
	case agent.InitEvent:
		obs.providerID = e.SessionID
		// Mirror to state.json so a crash here is recoverable.
		_, _ = r.store.Update(worktreePath, func(s *state.State) error {
			s.ProviderSessionID = e.SessionID
			s.CurrentStep = "streaming"
			return nil
		})
	case agent.AssistantTextEvent:
		if strings.TrimSpace(e.Text) != "" {
			obs.lastAssistantText = e.Text
			obs.providerError = ""
			obs.providerErrorNotRetryable = false
			obs.upstream = nil
		}
		// One verdict per message, from its FIRST line-anchored marker
		// (scanVerdict); the latest message that carries one decides the
		// stream's verdict. "blocked" — "WORK_RESULT: blocked" or
		// "AGENT_BLOCKED: <reason>" — is a deliberate decline, captured on
		// obs.blocked so the post-stream classifier forks to
		// FailureAgentBlocked instead of funneling a reasoned refusal as a
		// crash/silent-exit (which would trigger backstop + re-dispatch).
		switch verdict, reason := scanVerdict(e.Text); verdict {
		case "":
		case "blocked":
			obs.blocked = true
			obs.workResult = ""
			if reason != "" {
				obs.blockedReason = reason
			}
		default:
			obs.workResult = verdict
			obs.blocked = false
			obs.blockedReason = ""
		}
		// Structured review outcome, independent of the pass/fail marker:
		// the latest message carrying a line-anchored REVIEW_VERDICT
		// marker decides the stream's review verdict.
		if review := scanReviewVerdict(e.Text); review != "" {
			obs.reviewVerdict = review
		}
		if u := scanPRURL(e.Text); u != "" {
			obs.pullRequestURL = u
		}
		obs.pullRequestCandidates = appendPullRequestCandidates(obs.pullRequestCandidates, e.Text)
	case agent.SystemEvent:
		if e.Subtype == agent.SystemSubtypeProviderError {
			obs.providerError, obs.providerErrorNotRetryable = splitProviderErrorRetryable(strings.TrimSpace(e.Message))
			obs.upstream = agent.CanonicalUpstreamError(e.Upstream)
			if obs.providerError == "" {
				obs.providerError = "model provider error"
			}
		}
	case agent.ToolUseEvent:
		obs.toolCalls++
		// The model produced a tool call: any earlier provider error in
		// this stream was recovered from.
		obs.providerError = ""
		obs.providerErrorNotRetryable = false
		obs.upstream = nil
		toolName := strings.ToLower(e.ToolName)
		// Heuristic: track Linear-side outputs and PR creation.
		// Bash invocations of `gh pr create` are not tracked here —
		// the URL the agent prints lands in the matching
		// ToolResultEvent branch below, which scans for it.
		if strings.Contains(toolName, "linear") || strings.Contains(toolName, "af_linear") {
			if strings.Contains(toolName, "comment") {
				obs.commentPosted = true
			}
			if strings.Contains(toolName, "update_issue") {
				obs.issueUpdated = true
			}
			if strings.Contains(toolName, "create_issue") {
				obs.subIssuesMade = true
			}
		}
	case agent.ToolResultEvent:
		if u := scanPRURL(e.Content); u != "" && obs.pullRequestURL == "" {
			obs.pullRequestURL = u
		}
		obs.pullRequestCandidates = appendPullRequestCandidates(obs.pullRequestCandidates, e.Content)
	case agent.ResultEvent:
		obs.terminalEvent = &e
		obs.terminalSuccess = e.Success
		if !e.Success && e.Upstream != nil {
			obs.upstream = agent.CanonicalUpstreamError(e.Upstream)
		}
	case agent.ErrorEvent:
		// An error the session continues past is on the record (events.jsonl,
		// the activity sink) but is not the session's terminal, so it never
		// becomes the run's failure.
		if !e.SessionContinues {
			obs.errorEvent = &e
			if e.Upstream != nil {
				obs.upstream = agent.CanonicalUpstreamError(e.Upstream)
			}
		}
	}
}

// resolveKitDemand returns the kit toolchain demand to provision for this
// session, applying the explicit-overrides-detection precedence (OD-1,
// KITS PIVOT #3):
//
//  1. qw.Kits — the platform-resolved lifecycle demand threaded on the work
//     item. Its exact selected kit versions still undergo local command
//     ownership preflight before any provisioning.
//  2. r.kitComposer / r.kitDetector — fallback: detect kits from the cloned worktree at
//     wpath and compose a demand for r.kitTargetOS (sandbox OS for cloud,
//     host OS for local). Requires KitComposer or KitDetector to be wired at
//     runner construction; nil disables the fallback entirely.
//
// Returns nil when there is nothing to provision (no platform demand AND no
// detector / no detected kits) — the caller skips step 2b. On a detect or
// compose error it stamps res.Status="failed" + FailureKitProvision and
// returns a non-nil (non-empty) demand so the caller short-circuits Run.
func (r *Runner) resolveKitDemand(qw QueuedWork, wpath string, res *Result) *kit.ToolchainDemand {
	targetOS := r.kitTargetOS
	if qw.Kits != nil && qw.Kits.OS != "" {
		targetOS = qw.Kits.OS
	}
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
	}

	// 1. Platform lifecycle demand remains authoritative, but its exact kit
	// selection must pass local command ownership preflight. This prevents a
	// platform payload from bypassing the same collision/lock checks used by
	// repository detection.
	if hasPlatformKitDemand(qw.Kits) {
		if r.kitComposer == nil {
			return failKitComposition(res, targetOS, errors.New("platform-supplied kit demand requires local command composition preflight"))
		}
		selected, err := parseExactKitSelections(qw.Kits.Kits)
		if err != nil {
			return failKitComposition(res, targetOS, err)
		}
		composed, err := r.kitComposer(wpath, kit.CompositionTarget{
			OS: targetOS, WorkType: qw.WorkType, PathScope: ".",
		}, selected)
		if err != nil {
			return failKitComposition(res, targetOS, err)
		}
		if composed == nil {
			return failKitComposition(res, targetOS, errors.New("local command composition returned no demand"))
		}
		demand := cloneToolchainDemand(qw.Kits)
		if demand.OS == "" {
			demand.OS = targetOS
		}
		demand.Commands = append([]kit.QualifiedCommand(nil), composed.Commands...)
		demand.CommandBindings = append([]kit.GenericCommandBinding(nil), composed.CommandBindings...)
		demand.CompositionDigest = composed.CompositionDigest
		// The dependency-store plan is local authority too: a payload's own
		// dependency_stores never survive the preflight.
		demand.DependencyStores = append([]kit.ComposedDependencyStore(nil), composed.DependencyStores...)
		demand.DependencyStoresDigest = composed.DependencyStoresDigest
		r.logger.Info("kit toolchain: using platform-supplied lifecycle demand after command composition preflight",
			"sessionId", qw.SessionID,
			"os", demand.OS,
			"kits", demand.Kits,
			"compositionDigest", demand.CompositionDigest,
			"commandCount", len(demand.Commands),
			"bindingCount", len(demand.CommandBindings),
		)
		return demand
	}

	// 2. Detection fallback — only when a detector/composer is wired.
	if r.kitDetector == nil && r.kitComposer == nil {
		return nil
	}
	if r.kitComposer != nil {
		demand, composeErr := r.kitComposer(wpath, kit.CompositionTarget{
			OS: targetOS, WorkType: qw.WorkType, PathScope: ".",
		}, nil)
		if composeErr != nil {
			return failKitComposition(res, targetOS, composeErr)
		}
		if demand.IsEmpty() {
			return nil
		}
		r.logger.Info("kit command composition resolved",
			"sessionId", qw.SessionID,
			"compositionDigest", demand.CompositionDigest,
			"commandCount", len(demand.Commands),
			"bindingCount", len(demand.CommandBindings),
		)
		return demand
	}
	views, detErr := r.kitDetector(wpath, targetOS)
	if detErr != nil {
		res.Status = "failed"
		res.FailureMode = FailureKitProvision
		res.Error = fmt.Sprintf("kit detect: %v", detErr)
		// Non-nil sentinel so the caller's res.Status=="failed" branch fires.
		return &kit.ToolchainDemand{OS: targetOS}
	}
	demand, cmpErr := kit.Compose(views, targetOS)
	if cmpErr != nil {
		res.Status = "failed"
		res.FailureMode = FailureKitProvision
		res.Error = fmt.Sprintf("kit compose: %v", cmpErr)
		return &kit.ToolchainDemand{OS: targetOS}
	}
	if demand.IsEmpty() {
		return nil
	}
	return demand
}

func hasPlatformKitDemand(demand *kit.ToolchainDemand) bool {
	return demand != nil && (!demand.IsEmpty() || len(demand.Kits) > 0)
}

func parseExactKitSelections(refs []string) ([]kit.Selection, error) {
	if len(refs) == 0 {
		return nil, errors.New("platform kit demand must include at least one exact id@version selection")
	}
	selected := make([]kit.Selection, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		at := strings.LastIndex(ref, "@")
		if at <= 0 || at == len(ref)-1 {
			return nil, fmt.Errorf("platform kit selection %q must be an exact id@version reference", ref)
		}
		selection := kit.Selection{ID: ref[:at], Version: ref[at+1:]}
		key := selection.ID + "\x00" + selection.Version
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate platform kit selection %s@%s", selection.ID, selection.Version)
		}
		seen[key] = struct{}{}
		selected = append(selected, selection)
	}
	return selected, nil
}

func failKitComposition(res *Result, targetOS string, err error) *kit.ToolchainDemand {
	res.Status = "failed"
	res.FailureMode = FailureKitProvision
	res.Error = fmt.Sprintf("kit compose: %v", err)
	return &kit.ToolchainDemand{OS: targetOS}
}

func cloneToolchainDemand(source *kit.ToolchainDemand) *kit.ToolchainDemand {
	clone := *source
	clone.Kits = append([]string(nil), source.Kits...)
	clone.ToolchainInstall = append([]string(nil), source.ToolchainInstall...)
	clone.PostAcquire = append([]string(nil), source.PostAcquire...)
	clone.PreRelease = append([]string(nil), source.PreRelease...)
	if source.Env != nil {
		clone.Env = make(map[string]string, len(source.Env))
		for key, value := range source.Env {
			clone.Env[key] = value
		}
	}
	return &clone
}

// classifyWorktreeErr maps a worktree.Provision error to the
// runner-level FailureMode classification.
func classifyWorktreeErr(err error) string {
	switch {
	case errors.Is(err, worktree.ErrLostOwnership):
		return FailureLostOwnership
	default:
		return FailureWorktreeProvision
	}
}

// envToMap converts the env composer's KEY=VALUE slice back into a
// map for assignment to agent.Spec.Env. Splitting at the first '=' is
// safe — env values may contain '=' but keys never do.
func envToMap(in []string) map[string]string {
	out := make(map[string]string, len(in))
	for _, kv := range in {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// envOrDefault returns the process env value for key when it is set and
// non-empty, otherwise def. Used to let a provisioner-stamped value win over
// a locally-derived fallback.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildSessionEnv collects the per-session env entries every agent
// session needs. Mirrors the legacy TS LINEAR_* + DONMAI_* keys.
//
// GIT_AUTHOR_*/GIT_COMMITTER_* give backstop commits (and any commits made by
// the agent) a traceable author identity rather than whatever the git global
// config happens to contain inside the cloud sandbox or local worktree.
//
// Single-source precedence: a provisioner-stamped identity wins. A cloud box
// provisioner may inject its own canonical agent identity as
// GIT_AUTHOR_*/GIT_COMMITTER_* in the box env; when present it is authoritative
// here, so the runner's backstop commits carry the SAME identity as the agent's
// own in-box commits instead of overriding them with a divergent "Donmai Agent"
// persona. Absent a provisioner value (standalone / local worktree), fall back
// to a session-derived default: the fixed display name "Donmai Agent" and the
// session id as the email, so every commit is unambiguously linked to its
// originating session. The display name never carries the issue identifier:
// tracker keys can be private, and commits on a public repository (including
// the co-author trailers a squash merge composes from them) would publish it.
func buildSessionEnv(qw QueuedWork) map[string]string {
	// A fixed display name; the session id in the email carries attribution.
	gitName := "Donmai Agent"
	gitEmail := "agent+" + qw.SessionID + "@donmai.dev"

	// Honor a provisioner-supplied identity when set; committer defaults to the
	// resolved author (matching git's own convention) when only GIT_AUTHOR_* is
	// stamped, so the four values never diverge.
	authorName := envOrDefault("GIT_AUTHOR_NAME", gitName)
	authorEmail := envOrDefault("GIT_AUTHOR_EMAIL", gitEmail)
	committerName := envOrDefault("GIT_COMMITTER_NAME", authorName)
	committerEmail := envOrDefault("GIT_COMMITTER_EMAIL", authorEmail)

	envMap := map[string]string{
		"DONMAI_SESSION_ID": qw.SessionID,
		"LINEAR_SESSION_ID": qw.SessionID,
		// Git identity — provisioner-stamped when present, else pinned to the
		// session so backstop and agent commits are attributable even in
		// sandboxes whose git global config is empty or wrong.
		"GIT_AUTHOR_NAME":     authorName,
		"GIT_AUTHOR_EMAIL":    authorEmail,
		"GIT_COMMITTER_NAME":  committerName,
		"GIT_COMMITTER_EMAIL": committerEmail,
	}
	if qw.IssueID != "" {
		envMap["LINEAR_ISSUE_ID"] = qw.IssueID
	}
	if qw.IssueIdentifier != "" {
		envMap["LINEAR_ISSUE_IDENTIFIER"] = qw.IssueIdentifier
	}
	if qw.WorkType != "" {
		envMap["LINEAR_WORK_TYPE"] = qw.WorkType
	}
	if qw.ProjectName != "" {
		envMap["DONMAI_PROJECT"] = qw.ProjectName
	}
	if qw.OrganizationID != "" {
		envMap["DONMAI_ORG_ID"] = qw.OrganizationID
	}
	if qw.PlatformURL != "" {
		envMap["DONMAI_API_URL"] = qw.PlatformURL
	}
	// Credential-scope contract: AuthToken is the worker's platform runtime
	// bearer (heartbeat, result post, session preflight) and NOTHING else. It
	// is never a GitHub token on any work type or mode — a local daemon
	// forwards its own runtime JWT, a sandbox runner receives the runtime JWT
	// the provisioner minted at pre-registration, and a standalone daemon
	// carries its registration token — so it must never be surfaced as
	// GH_TOKEN. The composer lets Spec.Env win over the host env, so exporting
	// it here would clobber the seat's real gh auth (or a provisioner-stamped
	// short-lived git token) and present the platform bearer to api.github.com
	// on every gh call. GitHub access for the agent comes from the host env
	// and the credential snapshot, never from this layer. A ref-bearing run
	// (headless repair on an existing branch, or an interactive seat pinned to
	// a branch) changes the checkout, not the credentials.
	if qw.AuthToken != "" {
		envMap["WORKER_AUTH_TOKEN"] = qw.AuthToken
	}
	// Surface the stage id + budget into the agent's env so sub-agents spawned
	// via Task can self-identify which stage instance they belong to without
	// re-fetching the session detail.
	if qw.StageID != "" {
		envMap["DONMAI_STAGE_ID"] = qw.StageID
	}
	if b := qw.StageBudget; b != nil {
		if b.MaxDurationSeconds > 0 {
			v := fmt.Sprintf("%d", b.MaxDurationSeconds)
			envMap["DONMAI_STAGE_MAX_DURATION_SECONDS"] = v
		}
		if b.MaxSubAgents != nil {
			v := fmt.Sprintf("%d", *b.MaxSubAgents)
			envMap["DONMAI_STAGE_MAX_SUB_AGENTS"] = v
		}
		if b.MaxTokens > 0 {
			v := fmt.Sprintf("%d", b.MaxTokens)
			envMap["DONMAI_STAGE_MAX_TOKENS"] = v
		}
	}
	return envMap
}

// sessionPromptMode returns the session mode the exact harness adapter will
// resolve for the spec this run produces, so every runner-owned decision that
// depends on a mode-scoped profile reads the SAME mode the adapter will.
//
// agent.PromptModeForSpec prefers Spec.PromptMode and falls back to
// Spec.Interactive. Receipt-bearing runs stamp Spec.PromptMode from the
// admitted execution cell (buildPreparedSourceSpec), and validateReceiptCell
// already pins that cell to human-controlled exactly when the work is
// interactive or an interview — so the OR below is faithful to both lanes:
// the cell decides when there is one, and Spec.Interactive decides otherwise.
func trimRef(ref string) string { return strings.TrimSpace(ref) }

func validateRef(ref string) error {
	if ref == "" || strings.Contains(ref, "..") || strings.HasPrefix(ref, "/") || strings.Contains(ref, "\x00") {
		return fmt.Errorf("invalid ref %q", ref)
	}
	// Allow typical branch/tag refs: alphanum + /._- . Be strict but not exotic.
	for _, r := range ref {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '/' || r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("invalid ref %q: illegal character %q", ref, r)
		}
	}
	return nil
}

func sessionPromptMode(qw QueuedWork, cell executioncell.ResolvedExecutionCell) agent.PromptSessionMode {
	if cell.SessionMode == executioncell.SessionHumanControlled || qw.isInteractive() {
		return agent.PromptModeHumanControlled
	}
	return agent.PromptModeAutonomous
}

// harnessDeliversMCP reports whether the exact harness selected for a session
// can deliver Spec.MCPServers AT ALL in the given session mode.
//
// The predicate is the DECLARED tool/lifecycle profile's MCPDelivery, read off
// the live manifest — never inferred from the harness's name. That field is
// precisely what agent.AdaptToolLifecycle consults to admit or deny the
// "mcp-servers" requirement, so reading the same field the adapter reads is
// what keeps the runner from ever injecting a channel the adapter will refuse.
//
// Capabilities().AcceptsMcpServerSpec is only the FLAT projection: nothing ties
// it structurally to MCPDelivery (matrix/parity_test.go pins it against
// Manifest().Caps alone), and the adapter never reads it. It is therefore the
// fallback for a runtime that exposes no manifest, not the authority.
//
// A harness that declares no profile for the mode cannot deliver anything in
// that mode — agent.PrepareToolLifecycle denies the whole spawn — so it reports
// false rather than adding a requirement that is guaranteed to be denied.
func harnessDeliversMCP(provider agent.Provider, mode agent.PromptSessionMode) bool {
	if provider == nil {
		return false
	}
	if harness, ok := provider.(agent.HarnessProvider); ok {
		profile, found := harness.Manifest().ToolLifecycleProfile(mode)
		return found && profile.MCPDelivery != agent.ToolDeliveryUnsupported
	}
	return provider.Capabilities().AcceptsMcpServerSpec
}

// platformMCPServerName is the client-side label for the platform per-session
// MCP gateway entry.
//
// Brand-derived: a rebranded build of this same code renders its own brand's
// label byte-identically. The platform reports its own serverInfo.name
// independently; this is the client-side label only.
func platformMCPServerName() string { return statehome.Brand() + "-platform" }

// mcpGatewayBearer returns the bearer for the platform per-session MCP gateway.
//
// Only the session-scoped token the platform stamps on the work item is ever
// returned. When the platform mints no session bearer (a degraded mint) there
// is no usable gateway bearer: the worker runtime bearer (qw.AuthToken) is
// worker/host authority for heartbeat, result-post, activity-post and session
// preflight, and must never authenticate MCP calls as the host identity.
// Callers omit the gateway when this returns empty and surface the degraded
// mint instead of substituting the host bearer.
//
// Why session-scoped matters: the header this bearer lands in is written ONCE
// into an MCP config file at spawn, and nothing — not the daemon's runtime-
// credential refresh, not the harness — ever rewrites it. So the gateway keeps
// presenting whichever bearer was chosen here for the session's whole life, and
// the moment it expires the harness's tools disappear with no error surfaced.
// The session-scoped token is minted to outlive the session for exactly that
// reason.
//
// The value is opaque to this repo: never parsed, validated, or logged.
func mcpGatewayBearer(qw QueuedWork) string {
	return strings.TrimSpace(qw.McpAuthToken)
}

// isMCPGatewayDegradedMint reports whether a platform-connected session that
// would otherwise mount the per-session MCP gateway has no session-scoped
// bearer to mount it with. The gateway is then omitted (see
// defaultMCPServersForHarness) and the run loop surfaces this condition
// instead of authenticating MCP calls with the worker bearer.
//
// Pure — no I/O, no logging — so tests pin the condition without a harness.
// Local-transport sessions never mount the gateway by policy, so they are
// never degraded: standalone operation without a platform bearer is the
// normal path there, not a degraded mint.
func isMCPGatewayDegradedMint(qw QueuedWork) bool {
	if isLocalRuntimeTransport(qw.runtimeTransport) {
		return false
	}
	if strings.TrimSpace(qw.PlatformURL) == "" || strings.TrimSpace(qw.SessionID) == "" {
		return false
	}
	return strings.TrimSpace(qw.McpAuthToken) == ""
}

// logMCPGatewayDegradedMint emits one WARN line when a platform-connected
// session omits the MCP gateway because no session-scoped bearer was minted.
// This is the degraded-mint receipt: without it the missing gateway looks
// exactly like a standalone session that never had a platform. Strictly a
// signal — the caller never branches on it. Logs the session and worker
// identifiers, never bearer bytes.
func logMCPGatewayDegradedMint(logger *slog.Logger, qw QueuedWork) {
	if logger == nil || !isMCPGatewayDegradedMint(qw) {
		return
	}
	logger.Warn("[runner] platform MCP gateway omitted: no session bearer was minted",
		"sessionId", qw.SessionID,
		"workerId", qw.WorkerID,
	)
}

// protectedRuntimeMCPServer derives the exact session-scoped HTTP server used
// by the protected ACK contract. Unlike the legacy default path it never falls
// back to the worker bearer.
func protectedRuntimeMCPServer(qw QueuedWork, provider agent.Provider, mode agent.PromptSessionMode) (agent.MCPServerConfig, error) {
	if !harnessDeliversMCP(provider, mode) {
		return agent.MCPServerConfig{}, errors.New("runner: protected runtime MCP is unsupported by the selected harness profile")
	}
	if qw.PlatformURL == "" || qw.SessionID == "" || qw.McpAuthToken == "" || strings.TrimSpace(qw.McpAuthToken) != qw.McpAuthToken {
		return agent.MCPServerConfig{}, errors.New("runner: protected runtime MCP gateway configuration is unavailable")
	}
	return agent.MCPServerConfig{
		Name: platformMCPServerName(),
		Type: executioncell.ProtectedRuntimeMCPTransportHTTP,
		URL:  strings.TrimRight(qw.PlatformURL, "/") + "/api/mcp/" + qw.SessionID,
		Headers: map[string]string{
			"Authorization": "Bearer " + qw.McpAuthToken,
		},
	}, nil
}

// logMCPGatewayBearerExpiry emits one advisory INFO line naming when the
// gateway's bearer dies, so the one case this design does not close — a session
// that outlives its bearer still loses its tools silently — is at least visible
// in the logs beforehand.
//
// Strictly advisory. It returns nothing and the caller must not branch on it:
// the runner never refuses a spawn, shortens a session, or drops the gateway
// because of an expiry. It stays quiet unless there is something to say — an
// expiry hint AND a gateway actually mounted.
//
// Logs WHEN the bearer dies, never WHAT it is.
func logMCPGatewayBearerExpiry(logger *slog.Logger, qw QueuedWork, servers []agent.MCPServerConfig, now time.Time) {
	expiresAt := strings.TrimSpace(qw.McpAuthTokenExpiresAt)
	if logger == nil || expiresAt == "" {
		return
	}
	gatewayName := platformMCPServerName()
	if !slices.ContainsFunc(servers, func(s agent.MCPServerConfig) bool { return s.Name == gatewayName }) {
		return
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		logger.Warn("[runner] platform MCP gateway bearer expiry is not RFC3339; ignoring the hint",
			"sessionId", qw.SessionID,
			"value", expiresAt,
		)
		return
	}
	logger.Info(fmt.Sprintf("[runner] platform MCP gateway bearer expires at %s (%dm from now)",
		expiry.UTC().Format(time.RFC3339),
		int(expiry.Sub(now).Round(time.Minute).Minutes()),
	), "sessionId", qw.SessionID)
}

// defaultMCPServersForHarness returns the list of MCP servers a session ships
// with by default, for one exact harness in one session mode.
//
// Leads with one HTTP entry per session pointing at the platform's
// per-session MCP endpoint (/api/mcp/<sessionId>). The platform applies
// the A2A capability bundle filter at list-tools time and the
// defense-in-depth allow-list check at tool-call time — see the A2A ADR
// at runs/2026-05-20-adr-a2a-per-session-mcp.md.
//
// When PlatformURL or a session-scoped bearer is missing (standalone-mode
// sessions without a platform, or a degraded mint with no session bearer), the
// gate entry is omitted: the agent runs without any platform MCP gate. An
// absent session bearer on a platform-connected session is a degraded mint —
// the worker bearer is never substituted (see mcpGatewayBearer) — and the run
// loop surfaces that condition via logMCPGatewayDegradedMint.
//
// The gate is ALSO omitted when the selected harness declares no MCP delivery
// for this mode (harnessDeliversMCP). This entry is the runner's own implicit
// injection, made on the caller's behalf and never requested by them: asking a
// harness that has no MCP channel to mount it denies the spawn outright for a
// capability the session never asked for. An MCP server the CALLER did request
// — an agent-card entry, or the code-intel plugin below — is deliberately NOT
// filtered here: it stays in the spec, reaches the adapter, and fails loudly
// rather than being silently stripped.
//
// F.5 code-intel: when qw.CodeIntel is set the runner appends the in-box
// af-code-intelligence stdio plugin (os.Executable() + `mcp code-intel --root
// <wpath>`) AFTER the platform gate. root is the provisioned worktree path
// (loop.go step 2) and MUST be passed explicitly — the caller builds this list
// AFTER Provision so wpath exists. This function is the single place the runner
// extends MCP defaults.
func defaultMCPServersForHarness(qw QueuedWork, wpath string, provider agent.Provider, mode agent.PromptSessionMode, routes ...codeIntelDeliveryRoute) []agent.MCPServerConfig {
	var servers []agent.MCPServerConfig

	// Platform per-session HTTP gate — omitted in standalone mode (no platform
	// creds). Always leads the list so it is never shadowed by a later entry.
	if bearer := mcpGatewayBearer(qw); !isLocalRuntimeTransport(qw.runtimeTransport) && harnessDeliversMCP(provider, mode) && qw.PlatformURL != "" && bearer != "" && qw.SessionID != "" {
		protected := QueuedWork{PlatformURL: qw.PlatformURL, McpAuthToken: bearer}
		protected.SessionID = qw.SessionID
		server, _ := protectedRuntimeMCPServer(protected, provider, mode)
		servers = append(servers, server)
	}

	// In-box code-intelligence stdio plugin. Purely in-box — no platform
	// coupling — so it is emitted whenever the capability block is present,
	// including standalone-mode sessions. When the block is nil this is a no-op
	// and the output is byte-identical to the pre-code-intel path.
	route := codeIntelDeliveryLegacy
	if len(routes) > 0 {
		route = routes[0]
	}
	if qw.CodeIntel != nil && route != codeIntelDeliveryNative {
		servers = append(servers, codeIntelMCPEntry(wpath, qw.CodeIntel))
	}

	return servers
}

// appendDegradedMintWarning records the degraded-mint condition on the Result
// envelope so it is visible wherever PostSessionWarnings surface (dashboards,
// result payloads), not only in the runner log. Returns the warnings slice
// unchanged when the session is not degraded. Pure — no I/O.
func appendDegradedMintWarning(warnings []string, qw QueuedWork) []string {
	if !isMCPGatewayDegradedMint(qw) {
		return warnings
	}
	return append(warnings, "platform MCP gateway omitted: no session bearer was minted for session "+qw.SessionID)
}

// foldInlineSkills appends the agent card's INLINE skill bodies (WS5) to an
// existing SkillAppend block and collects the union of their disallowedTools.
// Inline skills carry their body verbatim on the wire (no SKILL.md on disk),
// so the bodies are joined directly. Skills follow the kit (file-sourced)
// skills already in existingAppend, separated by a blank line. Whitespace-only
// bodies contribute no text but their disallowedTools still count. Returns the
// new append text, the unioned disallowed-tool patterns, and the number of
// non-empty bodies injected (for the caller's log line). Pure — no I/O.
func foldInlineSkills(existingAppend string, skills []prompt.SkillSpec) (newAppend string, disallowed []string, injected int) {
	var inlineBodies []string
	for _, sk := range skills {
		if strings.TrimSpace(sk.Body) != "" {
			inlineBodies = append(inlineBodies, sk.Body)
		}
		if len(sk.DisallowedTools) > 0 {
			disallowed = append(disallowed, sk.DisallowedTools...)
		}
	}
	newAppend = existingAppend
	if len(inlineBodies) > 0 {
		inlineAppend := strings.Join(inlineBodies, "\n\n")
		if existingAppend != "" {
			newAppend = existingAppend + "\n\n" + inlineAppend
		} else {
			newAppend = inlineAppend
		}
	}
	return newAppend, disallowed, len(inlineBodies)
}

// mergeMCPServers unions the runner's per-session default MCP set (the
// platform per-session HTTP gate) with the agent card's MCP servers (WS5).
// The defaults LEAD and WIN on a name collision: the platform gate is the
// A2A enforcement point and must never be shadowed by a card-supplied entry
// of the same name. Card entries whose name is not already present are
// appended in order. Returns nil only when both inputs are empty so the
// existing standalone-mode (no platform gate) back-compat path is preserved.
func mergeMCPServers(defaults, cardServers []agent.MCPServerConfig) []agent.MCPServerConfig {
	if len(cardServers) == 0 {
		return defaults
	}
	// A single source length is a safe initial hint. The append/map runtimes
	// grow for card entries without an overflow-prone sum of wire lengths.
	seen := make(map[string]struct{}, len(defaults))
	merged := make([]agent.MCPServerConfig, 0, len(defaults))
	for _, s := range defaults {
		seen[s.Name] = struct{}{}
		merged = append(merged, s)
	}
	for _, s := range cardServers {
		if _, dup := seen[s.Name]; dup {
			// Default wins on collision — skip the card entry.
			continue
		}
		seen[s.Name] = struct{}{}
		merged = append(merged, s)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// scanVerdict returns the verdict of the FIRST line-anchored marker in text —
// "passed", "failed", "unknown" or "blocked" — plus the reason of an
// `AGENT_BLOCKED: <reason>` line, or ("", "") when text carries none.
//
// A marker counts only at the start of a line, modulo leading blanks and an
// optional opening HTML-comment fence, with the verdict on the SAME line and
// followed by a word boundary:
//
//	WORK_RESULT:passed    WORK_RESULT: failed    <!-- WORK_RESULT:blocked -->
//	AGENT_BLOCKED: spec is ambiguous    <!-- AGENT_BLOCKED: no repo access -->
//
// This is the platform sentinel reader's rule, including FIRST-match-wins
// within one message, so the runner and the platform never disagree about the
// same text. A marker quoted in prose ("I am not claiming WORK_RESULT: passed")
// is ignored, so it can never set the verdict that drives the post-session
// transition of result-sensitive work, nor reclassify a successful session as
// a decline. Matching is case-insensitive over ASCII only (see asciiLower).
func scanVerdict(text string) (verdict, reason string) {
	lower := asciiLower(text)
	loc := workResultRE.FindStringSubmatchIndex(lower)
	if loc == nil {
		return "", ""
	}
	if loc[2] >= 0 {
		if verdict = lower[loc[2]:loc[3]]; verdict != "blocked" {
			return verdict, ""
		}
	}
	// Blocked, in either spelling: the reason is the message's FIRST anchored
	// `AGENT_BLOCKED: <reason>` line, wherever it sits — so the prompts' own
	// order (`WORK_RESULT: blocked` then `AGENT_BLOCKED: <reason>`) keeps it.
	if m := agentBlockedRE.FindStringSubmatchIndex(lower); m != nil {
		reason = strings.TrimSpace(text[m[2]:m[3]])
		// Drop a trailing HTML-comment fence so a marker emitted as
		// "<!-- AGENT_BLOCKED: reason -->" yields just the reason.
		reason = strings.TrimSpace(strings.TrimSuffix(reason, "-->"))
	}
	return "blocked", reason
}

// scanWorkResult returns the passed|failed|unknown verdict scanVerdict reads
// from text, or "" (a blocked verdict is not a QA work-result; it drives
// FailureAgentBlocked through scanBlocked instead).
func scanWorkResult(text string) string {
	if verdict, _ := scanVerdict(text); verdict != "blocked" {
		return verdict
	}
	return ""
}

// scanBlocked reports whether scanVerdict reads a blocked verdict from text —
// "WORK_RESULT: blocked" (no reason) or "AGENT_BLOCKED: <reason>" (reason up
// to the end of the line) — and returns the reason.
func scanBlocked(text string) (string, bool) {
	verdict, reason := scanVerdict(text)
	return reason, verdict == "blocked"
}

// scanReviewVerdict returns the structured review outcome of the FIRST
// line-anchored REVIEW_VERDICT marker in text — "APPROVE",
// "APPROVE_WITH_FOLLOWUPS" or "REQUEST_CHANGES" — or "" when text
// carries none. The rule mirrors scanVerdict (line-anchored, same-line
// value, word boundary, ASCII-only case folding): a review verdict quoted
// in prose is ignored, so it can never set the structured field graders
// and the scorecard read. Matching is case-insensitive over ASCII only;
// the returned value is always the canonical upper-case spelling.
func scanReviewVerdict(text string) string {
	loc := reviewVerdictRE.FindStringSubmatchIndex(asciiLower(text))
	if loc == nil || loc[2] < 0 {
		return ""
	}
	switch asciiLower(text[loc[2]:loc[3]]) {
	case "approve":
		return "APPROVE"
	case "approve_with_followups":
		return "APPROVE_WITH_FOLLOWUPS"
	case "request_changes":
		return "REQUEST_CHANGES"
	default:
		return ""
	}
}

// asciiLower lower-cases ASCII letters only, leaving every other byte — and
// therefore every byte offset — unchanged. Marker and label regexes are
// written in lower case and matched against this form instead of using (?i),
// whose Unicode case folding would accept lookalikes such as "paſſed" (long
// s) or a Kelvin-sign "K".
func asciiLower(text string) string {
	for i := 0; i < len(text); i++ {
		if c := text[i]; 'A' <= c && c <= 'Z' {
			b := []byte(text)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return text
}

// scanPRURL extracts a github.com/<owner>/<repo>/pull/<number> URL
// from arbitrary text. Returns the empty string on no match.
func scanPRURL(text string) string {
	return prURLRE.FindString(text)
}

// classifyBlocked stamps FailureAgentBlocked onto res when the agent
// deliberately declined (obs.blocked) and produced no PR. Returns true
// when it classified the result as blocked so the caller can log + skip
// steering/backstop. Pure aside from mutating res — no I/O — so the
// classification fork is unit-testable in isolation.
//
// A PR-producing session is never treated as blocked even if the agent's
// narrative mentioned a blocker; the work landed.
func classifyBlocked(res *Result, obs streamObservation) bool {
	if !obs.blocked || res.PullRequestURL != "" {
		return false
	}
	res.Status = "failed"
	res.FailureMode = FailureAgentBlocked
	if res.Error == "" {
		if obs.blockedReason != "" {
			res.Error = "agent declined to proceed: " + obs.blockedReason
		} else {
			res.Error = "agent declined to proceed (blocked)"
		}
	}
	return true
}

var (
	workResultRE    = regexp.MustCompile(verdictMarkerPattern)
	reviewVerdictRE = regexp.MustCompile(reviewVerdictMarkerPattern)
	prURLRE         = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/\d+`)
)

// lineStartPrefix anchors every structured line the runner reads out of
// assistant text — verdict markers here and the `Intended manifest:` label
// (manifest.go): start of a line, then optional blanks. Patterns built on it
// are written in lower case and matched against asciiLower(text).
const lineStartPrefix = `(?m)^[ \t]*`

// markerSeparator joins a marker keyword to its value on the SAME line:
// `KEY:value`, `KEY: value` or `KEY value`. It never crosses a line break.
const markerSeparator = `(?:[ \t]*:[ \t]*|[ \t]+)`

// markerLead is where every marker starts: the line start plus an optional
// opening HTML-comment fence.
const markerLead = lineStartPrefix + `(?:<!--[ \t]*)?`

// verdictMarkerPattern is the single line-anchored marker rule (see
// scanVerdict): `WORK_RESULT` + separator + a verdict (group 1) that must end
// at a word boundary, so `WORK_RESULT: passedly` does not count; or
// `AGENT_BLOCKED` followed by a colon (required, as downstream requires it).
const verdictMarkerPattern = markerLead + `(?:work_result` + markerSeparator +
	`(passed|failed|blocked|unknown)\b|agent_blocked[ \t]*:)`

// agentBlockedRE captures the reason of an anchored `AGENT_BLOCKED: <reason>`
// line, up to the end of that line (possibly empty; never the next line).
var agentBlockedRE = regexp.MustCompile(markerLead + `agent_blocked[ \t]*:([^\r\n]*)`)

// reviewVerdictMarkerPattern is the line-anchored marker rule for the
// structured review outcome (see scanReviewVerdict): `REVIEW_VERDICT` +
// separator + one of APPROVE | APPROVE_WITH_FOLLOWUPS | REQUEST_CHANGES
// (group 1) ending at a word boundary, so `REVIEW_VERDICT: APPROVED` does
// not count.
const reviewVerdictMarkerPattern = markerLead + `review_verdict` + markerSeparator +
	`(approve_with_followups|approve|request_changes)\b`

// _ silences unused-import warnings for json when the package only
// imports it transitively. Kept so future hooks can re-enable.
var _ = json.Marshal
