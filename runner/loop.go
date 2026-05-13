package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/internal/kit"
	"github.com/RenseiAI/agentfactory-tui/runtime/activity"
	"github.com/RenseiAI/agentfactory-tui/runtime/heartbeat"
	"github.com/RenseiAI/agentfactory-tui/runtime/state"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// kitLoadSkills is the seam used for Kit skill loading in the runner
// loop. It delegates to internal/kit.LoadSkills; isolated here so tests
// can verify integration without a real KitRegistry on disk.
var kitLoadSkills = kit.LoadSkills

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
//     11b. Linear state transition (REN-1467) — parse WORK_RESULT,
//     resolve target status from sdlc.go, post update via the
//     issue-tracker proxy. Failures recorded as PostSessionWarnings;
//     never fatal.
//  12. Build Result envelope
//
// The orchestration loop is long by design — splitting it further hides
// the step ordering that is the package's primary contract.
//
//nolint:gocyclo,funlen // intentional — see comment above.
func (r *Runner) runLoop(ctx context.Context, qw QueuedWork, startedAt int64) (*Result, error) {
	res := &Result{
		SessionID:       qw.SessionID,
		IssueIdentifier: qw.IssueIdentifier,
		StartedAt:       startedAt,
	}
	res.ProviderName = qw.resolvedProvider()

	// REN-1485 / REN-1487 Phase 2: log which dispatch path is in use
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

	// REN-1485 / REN-1487 acceptance criterion #4 — sub-agent budget
	// enforcement. The enforcer is always constructed; when qw.StageBudget
	// is nil (legacy path) it is a disabled no-op so the runner can
	// observe events through it unconditionally.
	enforcer := NewBudgetEnforcer(qw.StageBudget, time.UnixMilli(startedAt))
	if enforcer.Enabled() {
		r.logger.Info("[runner-stage]",
			"sid", qw.SessionID,
			"stageId", qw.StageID,
			"event", "budget.enforce",
			"maxDurationSeconds", qw.StageBudget.MaxDurationSeconds,
			"maxSubAgents", qw.StageBudget.MaxSubAgents,
			"maxTokens", qw.StageBudget.MaxTokens,
		)
	}

	// 1. Resolve provider.
	provider, err := r.registry.Resolve(qw.resolvedProvider())
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailureProviderResolve
		res.Error = err.Error()
		return res, err
	}
	caps := provider.Capabilities()
	r.logger.Info("provider resolved",
		"sessionId", qw.SessionID,
		"provider", provider.Name(),
		"injection", caps.SupportsMessageInjection,
		"resume", caps.SupportsSessionResume,
	)

	// 2. Provision worktree. We clone at the remote default branch
	// (typically main) and create the per-session work branch on
	// top inside the worktree afterward — passing a non-existent
	// branch to `git clone --branch` fails because the upstream
	// reference does not yet exist.
	branch := qw.Branch
	if branch == "" {
		branch = "agent/" + qw.SessionID
	}
	wpath, err := r.wt.Provision(ctx, worktree.ProvisionSpec{
		SessionID: qw.SessionID,
		RepoURL:   qw.Repository,
		// Branch left empty — clone the remote default. The agent
		// branch is created post-clone via `git checkout -b`.
		Strategy: worktree.StrategyClone,
	})
	if err != nil {
		res.Status = "failed"
		res.FailureMode = classifyWorktreeErr(err)
		res.Error = err.Error()
		return res, err
	}
	res.WorktreePath = wpath
	r.logger.Debug("worktree provisioned", "sessionId", qw.SessionID, "path", wpath)

	// Create the per-session work branch in the worktree. Best-effort:
	// when the branch already exists (replay during recovery) `git
	// checkout -b` returns non-zero; we surface a Debug log and
	// continue so the agent still operates on the existing branch.
	if _, gerr := runGit(ctx, wpath, "checkout", "-b", branch); gerr != nil {
		r.logger.Debug("create work branch failed (may already exist)",
			"branch", branch, "err", gerr)
	}

	// 3. Compose env. Daemon is expected to inject the resolved
	// credential into qw.AuthToken's matching env var via Spec.Env;
	// we forward whatever the caller set plus the standard session
	// metadata.
	specEnv := buildSessionEnv(qw)

	// 4. Build MCP config (capability-gated by translateSpec later).
	mcpServers := defaultMCPServers(qw)
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
	// 5a. Collect Kit [provide.skills] contributions and inject them into
	// the prompt builder before rendering. Skills are loaded in kit-priority
	// order (higher priority → earlier position); unreadable files are
	// skipped with a warning so a broken kit does not abort the session.
	// Tool disallow rules scraped from SKILL.md frontmatter are carried
	// forward to step 6 for application to the agent.Spec.
	var kitDisallowedTools []string
	if len(r.kitSkillSources) > 0 {
		loaded, skillErr := kitLoadSkills(r.kitSkillSources)
		if skillErr != nil {
			r.logger.Warn("kit skill loader: partial load (some skill files skipped)",
				"sessionId", qw.SessionID,
				"err", skillErr,
			)
		}
		r.promptBuilder.SkillAppend = loaded.SystemAppend
		kitDisallowedTools = loaded.DisallowedTools
		if loaded.SystemAppend != "" {
			r.logger.Info("kit skills injected into system prompt",
				"sessionId", qw.SessionID,
				"skillBytes", len(loaded.SystemAppend),
				"disallowCount", len(kitDisallowedTools),
			)
		}
	}

	systemPrompt, userPrompt, err := r.promptBuilder.Build(qw.QueuedWork)
	if err != nil {
		res.Status = "failed"
		res.FailureMode = FailurePromptRender
		res.Error = err.Error()
		return res, err
	}

	// 6. Translate to agent.Spec.
	composedEnv := envToMap(r.envc.Compose(hostEnv(), agent.Spec{Env: specEnv}))
	spec := translateSpec(qw, caps, SpecInputs{
		Cwd:                wpath,
		Prompt:             userPrompt,
		SystemPromptAppend: systemPrompt,
		MCPServers:         mcpServers,
		Env:                composedEnv,
		Autonomous:         true,
	})
	// Apply Kit skill tool disallow rules (subtractive: Kit skills may
	// only narrow the tool surface, never widen it). Appended after the
	// defaults produced by translateSpec so the Kit-declared restrictions
	// are visible and auditable in the Spec.
	if len(kitDisallowedTools) > 0 {
		spec.DisallowedTools = append(spec.DisallowedTools, kitDisallowedTools...)
	}

	// 7. Initialise the per-session state.json so a crash mid-spawn
	// is recoverable.
	if _, err := r.store.Update(wpath, func(s *state.State) error {
		s.IssueIdentifier = qw.IssueIdentifier
		s.IssueID = qw.IssueID
		s.SessionID = qw.SessionID
		s.ProviderName = provider.Name()
		s.WorkType = qw.WorkType
		s.WorkerID = qw.WorkerID
		s.CurrentStep = "spawning"
		if s.StartedAt == 0 {
			s.StartedAt = startedAt
		}
		s.AttemptCount++
		return nil
	}); err != nil {
		// state.json is best-effort — log and continue.
		r.logger.Warn("state init failed", "sessionId", qw.SessionID, "err", err)
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
	pulser, err := heartbeat.New(heartbeat.Config{
		SessionID: qw.SessionID,
		WorkerID:  qw.WorkerID,
		// IssueID is the Linear issue UUID — the platform's
		// /lock-refresh handler keys the lock on issue:lock:{id}
		// and rejects the request with 400 when this is empty.
		// Sourced from prompt.QueuedWork.IssueID (camelCase
		// "issueId" on the wire).
		IssueID:            qw.IssueID,
		BaseURL:            qw.PlatformURL,
		AuthToken:          qw.AuthToken,
		CredentialProvider: hbCredentialProvider,
		Interval:           r.hbInterval,
		HTTPClient:         r.httpClient,
		Logger:             r.logger,
	})
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
		ProviderName: string(qw.resolvedProvider()),
	})
	if actErr != nil {
		r.logger.Warn("activity poster construct failed", "err", actErr)
	} else if startErr := actPoster.Start(ctx); startErr != nil {
		r.logger.Warn("activity poster start failed", "err", startErr)
	} else {
		sink = actPoster
		defer func() { _ = actPoster.Stop() }()
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

	streamRes, streamErr := r.consumeEvents(streamCtx, handle, wpath, qw, res, enforcer, sink)

	// Disambiguate between ctx-cancelled and lost-ownership before
	// classifying the failure mode.
	select {
	case <-lostOwnership:
		res.Status = "failed"
		res.FailureMode = FailureLostOwnership
		if res.Error == "" {
			res.Error = heartbeat.ErrLostOwnership.Error()
		}
		// Best-effort stop the provider so it doesn't keep tokens
		// running.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		return res, heartbeat.ErrLostOwnership
	default:
	}

	// Budget-exceeded short-circuit (REN-1485 / REN-1487 acceptance #4).
	// Either the enforcer surfaced *BudgetExceededError directly via
	// streamErr, or the wall-clock deadline tripped streamCtx and we
	// detect the breach now via CheckDuration. Either way the failure
	// is classified as FailureBudgetExceeded — distinct from generic
	// FailureTimeout so dashboards can group them.
	var budgetErr *BudgetExceededError
	if errors.As(streamErr, &budgetErr) { //nolint:revive // intentional: ObserveEvent already produced WORK_RESULT
		// no-op: budget breach was already surfaced via ObserveEvent's WORK_RESULT emission
	} else if errors.Is(streamErr, context.DeadlineExceeded) {
		// May or may not be a duration cap. CheckDuration tells us.
		if dErr := enforcer.CheckDuration(r.now()); dErr != nil {
			budgetErr = dErr
		}
	}
	if budgetErr != nil {
		res.Status = "failed"
		res.FailureMode = FailureBudgetExceeded
		if res.Error == "" {
			res.Error = budgetErr.Error()
		}
		// Best-effort stop the provider so it doesn't keep tokens
		// running past the cap.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		res.BudgetReport = enforcer.Report(r.now())
		r.logger.Warn("[runner-stage]",
			"sid", qw.SessionID,
			"stageId", qw.StageID,
			"event", "budget.breach",
			"cap", string(budgetErr.Cap),
			"detail", budgetErr.Detail,
		)
		return res, budgetErr
	}

	if streamErr != nil && errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
		res.Status = "failed"
		res.FailureMode = FailureTimeout
		if res.Error == "" {
			res.Error = streamErr.Error()
		}
		return res, streamErr
	}

	// Apply event-stream observations onto the result envelope.
	streamRes.applyTo(res, provider.Name())

	// 11. Tail recovery.
	if !r.skipSteering && shouldSteer(streamRes, caps) {
		res.SteeringTriggered = true
		if err := r.attemptSteering(ctx, handle, qw, streamRes); err != nil {
			r.logger.Warn("steering failed", "sessionId", qw.SessionID, "err", err)
		} else {
			// Re-consume any events the steering inject produced.
			tailRes, _ := r.consumeEvents(ctx, handle, wpath, qw, res, enforcer, sink)
			tailRes.applyTo(res, provider.Name())
		}
	}

	if !r.skipBackstop && shouldBackstop(res) {
		bsCtx, bsCancel := context.WithTimeout(context.Background(), 90*time.Second)
		bsReport := r.runBackstop(bsCtx, qw, branch, res)
		bsCancel()
		res.BackstopReport = &bsReport
		if bsReport.PRURL != "" && res.PullRequestURL == "" {
			res.PullRequestURL = bsReport.PRURL
		}
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

	// Attach the budget enforcement report on the success path
	// (REN-1485 / REN-1487 acceptance #4). Always non-nil; when
	// .Enforced is false (legacy work, no StageBudget) it serves as a
	// "no budget enforced" observation record. Breach paths attach the
	// report on the failure short-circuit above.
	if res.BudgetReport == nil {
		res.BudgetReport = enforcer.Report(r.now())
	}

	// 11b. Post-session Linear state transition (REN-1467). Runs after
	// the Result.Status has been finalised so resolveTargetStatus sees
	// the same "completed"/"failed" classification the platform will
	// receive. Skipped when SkipPostSession is set, or when the runner
	// has no IssueID to address (e.g. governor work types without a
	// Linear-side row).
	if !r.skipPostSession && qw.IssueID != "" {
		r.runPostSession(ctx, qw, res)
	}

	// Update state.json terminal snapshot (best-effort).
	if _, err := r.store.Update(wpath, func(s *state.State) error {
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

// streamObservation captures the per-event-stream observations
// runner.runLoop accumulates while consuming the provider's events
// channel. Pulled out into its own struct so steering and backstop
// can read the same data without re-scanning the events log.
type streamObservation struct {
	terminalSuccess bool
	terminalEvent   *agent.ResultEvent
	errorEvent      *agent.ErrorEvent
	pullRequestURL  string
	commentPosted   bool
	issueUpdated    bool
	subIssuesMade   bool
	workResult      string
	cost            *agent.CostData
	providerID      string
	// budgetBreach is set when the in-flight enforcer tripped a cap
	// during ObserveEvent. The runner reads this in the post-stream
	// classification path to fork to FailureBudgetExceeded instead of
	// the generic FailureProviderError / FailureSilentExit branches.
	budgetBreach *BudgetExceededError
}

// applyTo merges the observation into a Result envelope. Idempotent
// when called multiple times (e.g. after steering re-consumes events).
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
	if o.cost != nil {
		res.Cost = o.cost
	}
	if o.terminalEvent != nil && o.terminalEvent.Message != "" && res.Summary == "" {
		res.Summary = o.terminalEvent.Message
	}
	if o.errorEvent != nil && res.Error == "" {
		res.Error = o.errorEvent.Message
		if res.FailureMode == "" {
			res.FailureMode = FailureProviderError
		}
	}
}

// consumeEvents drains the handle's events channel, mirrors each
// event to .agent/events.jsonl + state store, and returns the
// observation summary on terminal event or channel close.
//
// Returns the observation and the ctx err (if cancellation tripped
// the loop). A nil err with terminalSuccess=false means the channel
// closed without a terminal Result — the caller classifies as
// FailureSilentExit.
func (r *Runner) consumeEvents(
	ctx context.Context,
	handle agent.Handle,
	worktreePath string,
	qw QueuedWork,
	_ *Result,
	enforcer *BudgetEnforcer,
	sink activitySink,
) (streamObservation, error) {
	if sink == nil {
		sink = noopSink{}
	}
	obs := streamObservation{}

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

	for {
		select {
		case <-ctx.Done():
			return obs, ctx.Err()
		case ev, ok := <-handle.Events():
			if !ok {
				return obs, nil
			}
			appendJSONL(ev)
			r.observeEvent(ev, &obs, worktreePath, qw)
			// Push the event to the platform's activity buffer (best-
			// effort, non-blocking — the sink drops on overflow / HTTP
			// failure rather than stalling the runner). Lives next to
			// observeEvent so steering's tail consume picks it up too.
			sink.Send(ctx, ev)
			// Budget enforcement (REN-1485 / REN-1487 acceptance #4):
			// every event flows through the enforcer; on a cap breach
			// we surface the *BudgetExceededError so runLoop can
			// classify the failure as FailureBudgetExceeded.
			if enforcer != nil {
				if berr := enforcer.ObserveEvent(ev); berr != nil {
					obs.budgetBreach = berr
					return obs, berr
				}
			}
			if _, terminal := ev.(agent.ResultEvent); terminal {
				return obs, nil
			}
		}
	}
}

// observeEvent applies a single event to the observation accumulator.
// Side effects:
//   - InitEvent → captures provider session id; mirrors to state.json.
//   - ToolUseEvent → tracks comment/issue/sub-issue flags and
//     extracts a PR URL when the agent invokes `gh pr create`.
//   - AssistantTextEvent → scans for the WORK_RESULT marker and
//     accumulates the agent's running narrative.
//   - ResultEvent → captures terminal cost/success.
//   - ErrorEvent → records for FailureProviderError classification.
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
		if marker := scanWorkResult(e.Text); marker != "" {
			obs.workResult = marker
		}
		if u := scanPRURL(e.Text); u != "" {
			obs.pullRequestURL = u
		}
	case agent.ToolUseEvent:
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
	case agent.ResultEvent:
		obs.terminalEvent = &e
		obs.terminalSuccess = e.Success
		if e.Cost != nil {
			obs.cost = e.Cost
		}
	case agent.ErrorEvent:
		obs.errorEvent = &e
	}
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

// buildSessionEnv collects the per-session env entries every agent
// session needs. Mirrors the legacy TS LINEAR_* + AGENTFACTORY_*
// keys. Operators add more via repository config.
func buildSessionEnv(qw QueuedWork) map[string]string {
	envMap := map[string]string{
		"AGENTFACTORY_SESSION_ID": qw.SessionID,
		"LINEAR_SESSION_ID":       qw.SessionID,
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
		envMap["AGENTFACTORY_PROJECT"] = qw.ProjectName
	}
	if qw.OrganizationID != "" {
		envMap["AGENTFACTORY_ORG_ID"] = qw.OrganizationID
	}
	if qw.PlatformURL != "" {
		envMap["AGENTFACTORY_API_URL"] = qw.PlatformURL
	}
	if qw.AuthToken != "" {
		envMap["WORKER_AUTH_TOKEN"] = qw.AuthToken
	}
	// REN-1485 / REN-1487 Phase 2 — surface the stage id + budget into
	// the agent's env so sub-agents spawned via Task can self-identify
	// which stage instance they belong to without re-fetching the
	// session detail.
	if qw.StageID != "" {
		envMap["AGENTFACTORY_STAGE_ID"] = qw.StageID
	}
	if b := qw.StageBudget; b != nil {
		if b.MaxDurationSeconds > 0 {
			envMap["AGENTFACTORY_STAGE_MAX_DURATION_SECONDS"] = fmt.Sprintf("%d", b.MaxDurationSeconds)
		}
		if b.MaxSubAgents > 0 {
			envMap["AGENTFACTORY_STAGE_MAX_SUB_AGENTS"] = fmt.Sprintf("%d", b.MaxSubAgents)
		}
		if b.MaxTokens > 0 {
			envMap["AGENTFACTORY_STAGE_MAX_TOKENS"] = fmt.Sprintf("%d", b.MaxTokens)
		}
	}
	return envMap
}

// defaultMCPServers returns the list of MCP stdio servers every
// session ships with by default. Today empty — F.5 wires
// af_linear / af_code MCP entries through the daemon's installed
// plugin set. Kept here so the runner has a single point to extend.
func defaultMCPServers(_ QueuedWork) []agent.MCPServerConfig {
	return nil
}

// scanWorkResult scans the assistant text for the WORK_RESULT marker
// the platform expects (per F.0.1 §1). Returns "passed" / "failed" /
// "" matching the wire shape; whitespace and surrounding HTML comments
// are tolerated.
func scanWorkResult(text string) string {
	// Match "WORK_RESULT:passed", "WORK_RESULT:failed",
	// "<!-- WORK_RESULT:passed -->" etc.
	if loc := workResultRE.FindStringSubmatch(text); loc != nil {
		return strings.ToLower(loc[1])
	}
	return ""
}

// scanPRURL extracts a github.com/<owner>/<repo>/pull/<number> URL
// from arbitrary text. Returns the empty string on no match.
func scanPRURL(text string) string {
	return prURLRE.FindString(text)
}

var (
	workResultRE = regexp.MustCompile(`(?i)WORK_RESULT[:\s]+(passed|failed|unknown)`)
	prURLRE      = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/\d+`)
)

// _ silences unused-import warnings for json when the package only
// imports it transitively. Kept so future hooks can re-enable.
var _ = json.Marshal
