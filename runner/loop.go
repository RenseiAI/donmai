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

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/activity"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
	"github.com/RenseiAI/donmai/runtime/worktree"
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
func (r *Runner) runLoop(ctx context.Context, qw QueuedWork, startedAt int64) (*Result, error) {
	res := &Result{
		SessionID:       qw.SessionID,
		IssueIdentifier: qw.IssueIdentifier,
		StartedAt:       startedAt,
	}
	res.ProviderName = qw.resolvedProvider()

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

	// Sub-agent budget
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
	//
	// Router-learning A5b (read side). BEFORE resolving, consult the platform's
	// posterior selector for the best provider for this work type. Self-gates on
	// ROUTING_SELECTOR_ENABLED (default OFF) + required coordinates; the platform
	// flag is the authoritative kill-switch and OWNS the explicit-choice guard.
	// Hard static fallback (= qw.resolvedProvider()) on flag-off / proxy fail /
	// provider-not-in-candidates — so flag-off is byte-for-byte today's behaviour.
	selectedName := qw.resolvedProvider()
	if name, ok := r.selectProviderByPosterior(ctx, qw); ok {
		r.logger.Info("provider overridden by posterior selection",
			"sessionId", qw.SessionID,
			"static", string(qw.resolvedProvider()),
			"selected", name,
		)
		selectedName = agent.ProviderName(name)
	}
	// Reflect the provider actually run so the A2-Go reward write attributes the
	// observation to it (not the pre-selection static choice).
	res.ProviderName = selectedName
	provider, err := r.registry.Resolve(selectedName)
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
	if _, gerr := runGit(ctx, wpath, gitIdentity{}, "checkout", "-b", branch); gerr != nil {
		r.logger.Debug("create work branch failed (may already exist)",
			"branch", branch, "err", gerr)
	}

	// 2a-bis. Materialize read-only sibling context repos named by
	// DONMAI_SIBLING_REPOS next to the session worktree so agents find
	// their governing corpus at ../<name> as their repo AGENTS.md
	// contracts promise (ADR-2026-07-07-sibling-context-repos). Never
	// fatal: a failed sibling logs a warning and the session proceeds —
	// agents fall back to cloning it themselves.
	r.provisionSiblings(ctx, qw, wpath)

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
	if demand := r.resolveKitDemand(qw, wpath, res); demand != nil {
		if res.Status == "failed" {
			// resolveKitDemand classified a detect/compose error onto res.
			return res, fmt.Errorf("kit demand: %s", res.Error)
		}
		if !demand.IsEmpty() {
			r.logger.Info("kit toolchain provision starting",
				"sessionId", qw.SessionID,
				"os", demand.OS,
				"kits", demand.Kits,
				"installSteps", len(demand.ToolchainInstall),
				"postAcquireSteps", len(demand.PostAcquire),
			)
			execer := shellExecer{baseEnv: buildSessionEnv(qw)}
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

	// 2c. Post-clone kit skill + prompt-fragment re-detection.
	//
	// The daemon pre-computed KitSkillSources at runner construction time
	// using its CWD, but the real repo has now been cloned to wpath so
	// detection against the actual repo contents may differ (e.g. a framework
	// manifest declares files=[pom.xml] and the daemon's CWD has no pom.xml).
	// When KitSkillDetector is wired, replace the pre-computed sources with a
	// fresh scan against wpath. Additive: nil KitSkillDetector keeps the
	// existing KitSkillSources (pre-K1-bootstrap behaviour).
	kitSkillSources := r.kitSkillSources // default: daemon-CWD pre-compute
	if r.kitSkillDetector != nil {
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
	if r.kitPromptFragDetector != nil {
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
	specEnv := buildSessionEnv(qw)

	// 4. Build MCP config (capability-gated by translateSpec later).
	//
	// The platform per-session HTTP gate (defaultMCPServers) ALWAYS leads:
	// it is the A2A capability-bundle + tool-call allow-list enforcement
	// point, so it must remain in the set. The agent card's MCP servers
	// (qw.McpServers, WS5) are APPENDED after it; dedup is by server name
	// with the default winning on collision (the platform gate is never
	// shadowed by a card entry of the same name). Additive: no card servers
	// → identical to the prior behaviour.
	mcpServers := mergeMCPServers(defaultMCPServers(qw, wpath), qw.McpServers)
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
	// Reset SkillAppend at the start of each Run so no session bleeds
	// into the next (the Runner is long-lived; SkillAppend is per-Run).
	r.promptBuilder.SkillAppend = ""

	var kitDisallowedTools []string
	if len(kitSkillSources) > 0 {
		loaded, skillErr := kitLoadSkills(kitSkillSources)
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

	// Fold the agent card's INLINE skills (WS5) into the prompt builder AFTER
	// the kit (file-sourced) skills, and union their disallowedTools into the
	// kit-derived disallowed set. Inline skills carry their body verbatim on
	// the wire (no SKILL.md on disk). Additive: no card skills → no change.
	if len(qw.Skills) > 0 {
		newAppend, inlineDisallow, injected := foldInlineSkills(r.promptBuilder.SkillAppend, qw.Skills)
		r.promptBuilder.SkillAppend = newAppend
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
			existing := r.promptBuilder.SkillAppend
			if existing != "" {
				r.promptBuilder.SkillAppend = existing + "\n\n" + loadedFrags.SystemAppend
			} else {
				r.promptBuilder.SkillAppend = loadedFrags.SystemAppend
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
	// prompt builder emits persona-first (the builder uses
	// SystemPromptOverride verbatim as the entire system prompt). The
	// persona pins the agent into one-question-per-turn / thinking-only
	// behaviour and survives a cloned-repo CLAUDE.md (a live sandbox run proves the
	// hostile-CLAUDE.md case in a live sandbox). Headless runs are
	// untouched — this only fires when qw.Mode == interview.
	if qw.isInterview() {
		qw.SystemPromptOverride = buildInterviewSystemPrompt(
			qw.SystemPromptOverride, interview.InterviewCompleteSentinel)
	}

	// Token-amplification guard: when the provider can deliver session
	// context via the first turn's input (codex re-sends baseInstructions
	// on every model turn), keep the large/volatile agent-memory block
	// OUT of the system-prompt prefix. We render the prompt with an empty
	// MemoryBlock so the builder does not fold it into the system prompt,
	// then thread the memory through Spec.InitialContext instead — it is
	// delivered once and then lives in cached conversation history. For
	// providers without that split the memory stays folded into the system
	// prompt exactly as before (additive — no behaviour change).
	buildQW := qw.QueuedWork
	var initialContext string
	if caps.SupportsTurnInputContext && strings.TrimSpace(buildQW.MemoryBlock) != "" {
		initialContext = buildQW.MemoryBlock
		buildQW.MemoryBlock = ""
	}

	systemPrompt, userPrompt, err := r.promptBuilder.Build(buildQW)
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
	systemPrompt = injectCodeIntelPartial(systemPrompt, caps, qw.CodeIntel)

	// 6. Translate to agent.Spec.
	composedEnv := envToMap(r.envc.Compose(hostEnv(), agent.Spec{Env: specEnv}))
	spec := translateSpec(qw, caps, SpecInputs{
		Cwd:                wpath,
		Prompt:             userPrompt,
		SystemPromptAppend: systemPrompt,
		InitialContext:     initialContext,
		MCPServers:         mcpServers,
		Env:                composedEnv,
		Autonomous:         true,
		Logger:             r.logger,
		ProviderName:       string(provider.Name()),
	})
	// Apply Kit skill + agent-card inline-skill tool disallow rules
	// (subtractive: skills may only narrow the tool surface, never widen
	// it). Appended after the defaults produced by translateSpec so the
	// declared restrictions are visible and auditable in the Spec. For
	// providers that route per-tool permission through PermissionConfig
	// (codex), also mirror these patterns into the approval bridge's
	// DisallowPatterns so the narrowing reaches the agent — translateSpec
	// only saw the platform DisallowedTools floor, not the kit/skill set
	// computed here.
	if len(kitDisallowedTools) > 0 {
		spec.DisallowedTools = append(spec.DisallowedTools, kitDisallowedTools...)
		if caps.NeedsPermissionConfig && !caps.AcceptsAllowedToolsList {
			if spec.PermissionConfig == nil {
				spec.PermissionConfig = &agent.PermissionConfig{}
			}
			spec.PermissionConfig.DisallowPatterns = append(
				spec.PermissionConfig.DisallowPatterns, kitDisallowedTools...)
		}
	}

	// Interview mode: lock down the tool surface at
	// the Spec level for interview sessions. Two categories:
	//
	//   1. AskUserQuestion — turn-taking happens via claude --resume (the
	//      runner injects the user's reply between turns), NOT via the tool.
	//      Leaving it enabled would let the agent block on its own tool prompt
	//      inside a single turn instead of ending its turn and parking for the
	//      next inject.
	//
	//   2. Code-authoring tools (Write, Edit, Task, Bash) — an interview
	//      session is thinking-only.  The hardened persona already instructs
	//      the agent to avoid these, but a hostile cloned-repo .claude/CLAUDE.md
	//      can drag the agent back into developer behaviour via worked examples
	//      (feedback_claudemd_overrides_system_prompt_directive precedent).
	//      Belt-and-suspenders: disallow them structurally at the Spec level so
	//      the provider rejects any attempt to invoke them regardless of what
	//      the persona or CLAUDE.md says.  The structural proof is in
	//      runner/interview_persona_hostile_test.go; the behavioural
	//      proof against a live hostile-repo sandbox is deferred to follow-up work.
	if qw.isInterview() {
		spec.DisallowedTools = append(spec.DisallowedTools,
			"AskUserQuestion",
			"Write",
			"Edit",
			"Task",
			"Bash",
		)
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
	// The channel is wired to OnInject whenever the provider supports message
	// injection. Whether any block is actually delivered is decided ENTIRELY by
	// the platform (it only returns an `inject` on the lock-refresh response
	// when the project's memory config has runtime-inject enabled) — so the
	// worker needs no env var or local config. Providers without injection
	// support rely on the dispatch-time fold (v1).
	injectCh := make(chan heartbeat.InjectPayload, 8)
	seenInject := map[string]struct{}{}
	runtimeInjectEnabled := caps.SupportsMessageInjection

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
	// Wave 3 runtime memory-inject: wire OnInject only when the feature is
	// enabled AND the provider can accept injects. The closure runs on the
	// heartbeat goroutine; it owns seenInject (dedup-by-DeliveryID lives
	// here exclusively so the map is never touched off this goroutine) and
	// performs a non-blocking send onto injectCh — a full buffer REJECTS
	// the inject (returns false) rather than stalling the heartbeat loop,
	// so the pulser leaves it unacked and the platform re-delivers it on a
	// later refresh (ack-or-requeue). A DeliveryID is marked seen only
	// AFTER a successful buffer so a re-delivery of a rejected inject is
	// not dedup-blocked. The runner goroutine drains injectCh at the
	// post-terminal seam and is the only caller of handle.Inject (claude's
	// single-in-flight contract).
	var onInject func(heartbeat.InjectPayload) bool
	if runtimeInjectEnabled {
		onInject = func(p heartbeat.InjectPayload) bool {
			if p.DeliveryID != "" {
				if _, ok := seenInject[p.DeliveryID]; ok {
					r.logger.Debug("memory inject: skipping already-seen delivery",
						"sessionId", qw.SessionID, "deliveryId", p.DeliveryID)
					// Already buffered earlier in this Run — safe to ack
					// so the platform stops re-sending it.
					return true
				}
			}
			select {
			case injectCh <- p:
				if p.DeliveryID != "" {
					seenInject[p.DeliveryID] = struct{}{}
				}
				return true
			default:
				r.logger.Warn("memory inject: channel full, leaving unacked for re-delivery",
					"sessionId", qw.SessionID, "deliveryId", p.DeliveryID)
				return false
			}
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
		OnInject:           onInject,
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
				SessionID:   qw.SessionID,
				OrgID:       qw.OrganizationID,
				WorkspaceID: qw.OrganizationID,
				WorkType:    qw.WorkType,
				System:      spanruntime.ProviderSystem(qw.resolvedProvider()),
				Model:       qw.ResolvedProfile.Model,
				Sender:      spanPoster,
				Now:         r.now,
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
		HTTPClient:         r.httpClient,
		Logger:             r.logger,
		// Interval intentionally left at the 15s default — calibrated
		// against the platform's 60s SESSION_STALE_THRESHOLD_MS.
	})
	if stepErr != nil {
		r.logger.Warn("step-heartbeat construct failed", "err", stepErr)
	} else if startErr := stepEmitter.Start(ctx); startErr != nil {
		r.logger.Warn("step-heartbeat start failed", "err", startErr)
	} else {
		defer func() { _ = stepEmitter.Stop() }()
	}

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
		return r.dispatchInterview(ctx, handle, wpath, qw, res, sink, traceProcessor, injectCh)
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

	streamRes, streamErr := r.consumeEvents(streamCtx, handle, wpath, qw, res, enforcer, sink, traceProcessor)

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
		return res, heartbeat.ErrLostOwnership
	default:
	}

	// Budget-exceeded short-circuit.
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

	// Idle/no-progress watchdog cut-off. The watchdog cancels the stream
	// ctx (surfacing context.Canceled), so this must be checked BEFORE
	// the generic ctx-cancelled timeout branch below to classify the
	// wedged-but-channel-alive session as FailureNoProgress rather than
	// FailureTimeout. Stop the provider so it doesn't keep burning tokens.
	if streamRes.noProgress {
		res.Status = "failed"
		res.FailureMode = FailureNoProgress
		if res.Error == "" {
			res.Error = fmt.Sprintf("no agent event within idle timeout (%s)", r.idleTimeout)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		return res, streamErr
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
	r.applyTurnManifest(wpath, qw, res, &streamRes)

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
	if runtimeInjectEnabled && !streamRes.blocked {
		injRes := r.drainMemoryInjects(ctx, handle, wpath, qw, res, enforcer, sink, traceProcessor, injectCh)
		injRes.applyTo(res, provider.Name())
	}

	// 11. Tail recovery. Skipped entirely when the agent deliberately
	// declined (FailureAgentBlocked): there is nothing to steer toward and
	// no work to backstop into an empty branch.
	//
	// Belt-and-suspenders bypass on top of the contract gate inside
	// shouldSteer/shouldBackstop: a non-result-sensitive work type that
	// already produced a passing WorkResult (marker or manifest verdict,
	// resolved into res.WorkResult above) has demonstrably completed — it
	// must not be steered toward a commit nor backstopped into an empty
	// PR. The contract check alone would already skip these, but the
	// explicit success marker makes the intent unambiguous and survives a
	// future contract-table edit.
	nonVCPassed := !isResultSensitive(qw.WorkType) && res.WorkResult == "passed"
	if !r.skipSteering && !streamRes.blocked && !nonVCPassed && shouldSteer(streamRes, caps, qw.WorkType) {
		res.SteeringTriggered = true
		if err := r.attemptSteering(ctx, handle, qw, streamRes); err != nil {
			r.logger.Warn("steering failed", "sessionId", qw.SessionID, "err", err)
		} else {
			// Re-consume any events the steering inject produced.
			tailRes, _ := r.consumeEvents(ctx, handle, wpath, qw, res, enforcer, sink, traceProcessor)
			tailRes.applyTo(res, provider.Name())
		}
	}

	if !r.skipBackstop && !nonVCPassed && shouldBackstop(res, qw.WorkType) {
		bsCtx, bsCancel := context.WithTimeout(context.Background(), 90*time.Second)
		bsReport := r.runBackstop(bsCtx, qw, branch, res)
		bsCancel()
		res.BackstopReport = &bsReport
		if bsReport.PRURL != "" && res.PullRequestURL == "" {
			res.PullRequestURL = bsReport.PRURL
		}
	}

	// 11c-b. Pushed-but-no-PR terminal classification. The backstop only
	// runs for a result-sensitive work type that reached teardown without a
	// PR (shouldBackstop gates on both). If it ran but still could not open
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
	if res.BackstopReport != nil && res.PullRequestURL == "" && RequiresPRURL(qw.WorkType) {
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
	shaCtx, shaCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if sha, shaErr := captureHeadSHA(shaCtx, wpath); shaErr != nil {
		r.logger.Warn("head commit capture failed",
			"sessionId", qw.SessionID, "err", shaErr)
	} else {
		res.CommitSHA = sha
	}
	shaCancel()

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
			// (commit/PR/cost) is observed + mirrored.
			injRes, _ := r.consumeEvents(ctx, handle, worktreePath, qw, res, enforcer, sink, traceProcessor)
			injRes.applyTo(res, qw.resolvedProvider())
			merged = injRes
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
	commentPosted   bool
	issueUpdated    bool
	subIssuesMade   bool
	workResult      string
	cost            *agent.CostData
	providerID      string
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
	// during ObserveEvent. The runner reads this in the post-stream
	// classification path to fork to FailureBudgetExceeded instead of
	// the generic FailureProviderError / FailureSilentExit branches.
	budgetBreach *BudgetExceededError
	// noProgress is set when the idle/no-progress watchdog fired — the
	// event stream produced no agent.Event for longer than the runner's
	// IdleTimeout window. The runner reads it in the post-stream
	// classification path to fork to FailureNoProgress instead of the
	// generic FailureTimeout (ctx-cancelled) branch, so a wedged session
	// is routed distinctly from a deadline expiry.
	noProgress bool
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
	traceProcessor spanEventProcessor,
) (streamObservation, error) {
	if sink == nil {
		sink = noopSink{}
	}
	if traceProcessor == nil {
		traceProcessor = noopSpanProcessor{}
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

	// Idle/no-progress watchdog. A resettable timer is reset on every
	// agent.Event; if no event arrives within r.idleTimeout the session
	// is wedged-but-channel-alive (the events channel is still open, so
	// it is not a silent exit, but forward progress has stopped). On
	// expiry we cancel a stream-scoped context and flag obs.noProgress so
	// the caller classifies FailureNoProgress instead of the generic
	// FailureTimeout. A non-positive r.idleTimeout disables the watchdog
	// (idleC stays nil → its select case never fires).
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if r.idleTimeout > 0 {
		idleTimer = time.NewTimer(r.idleTimeout)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}
	// resetIdle re-arms the watchdog after each observed event. Drains a
	// possibly-already-fired timer channel before Reset per the stdlib
	// time.Timer contract.
	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(r.idleTimeout)
	}

	for {
		select {
		case <-watchCtx.Done():
			// watchCtx is cancelled either because the parent ctx was
			// cancelled (timeout / lost-ownership / budget) or because the
			// watchdog fired below. obs.noProgress disambiguates the latter.
			return obs, watchCtx.Err()
		case <-idleC:
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
			// Forward progress observed — re-arm the watchdog.
			resetIdle()
			for _, correlatedEvent := range traceProcessor.Process(ev) {
				appendJSONL(correlatedEvent)
				r.observeEvent(correlatedEvent, &obs, worktreePath, qw)
				// Push every correlated/synthetic event to the platform's
				// activity buffer. LlmCallEvent intentionally maps to no legacy
				// activity, while tool events retain their stamped IDs.
				sink.Send(watchCtx, correlatedEvent)
				if enforcer != nil {
					if berr := enforcer.ObserveEvent(correlatedEvent); berr != nil {
						obs.budgetBreach = berr
						return obs, berr
					}
				}
				if _, terminal := correlatedEvent.(agent.ResultEvent); terminal {
					return obs, nil
				}
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
		if strings.TrimSpace(e.Text) != "" {
			obs.lastAssistantText = e.Text
		}
		if marker := scanWorkResult(e.Text); marker != "" {
			obs.workResult = marker
		}
		// Structural blocked-agent signal: a deliberate decline the agent
		// announced via "WORK_RESULT:blocked" or "AGENT_BLOCKED: <reason>".
		// Captured here so the post-stream classifier can fork to
		// FailureAgentBlocked instead of funneling a reasoned refusal as a
		// crash/silent-exit (which would trigger backstop + re-dispatch).
		if reason, ok := scanBlocked(e.Text); ok {
			obs.blocked = true
			if reason != "" && obs.blockedReason == "" {
				obs.blockedReason = reason
			}
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

// resolveKitDemand returns the kit toolchain demand to provision for this
// session, applying the explicit-overrides-detection precedence (OD-1,
// KITS PIVOT #3):
//
//  1. qw.Kits — the platform-resolved demand threaded on the work item.
//     When non-nil and non-empty it is authoritative; detection is skipped.
//  2. r.kitDetector — fallback: detect kits from the cloned worktree at
//     wpath and compose a demand for r.kitTargetOS (sandbox OS for cloud,
//     host OS for local). Requires KitDetector to be wired at runner
//     construction; nil disables the fallback entirely.
//
// Returns nil when there is nothing to provision (no platform demand AND no
// detector / no detected kits) — the caller skips step 2b. On a detect or
// compose error it stamps res.Status="failed" + FailureKitProvision and
// returns a non-nil (non-empty) demand so the caller short-circuits Run.
func (r *Runner) resolveKitDemand(qw QueuedWork, wpath string, res *Result) *kit.ToolchainDemand {
	// 1. Platform-supplied demand wins (explicit overrides detection).
	if qw.Kits != nil && !qw.Kits.IsEmpty() {
		r.logger.Info("kit toolchain: using platform-supplied demand",
			"sessionId", qw.SessionID,
			"os", qw.Kits.OS,
			"kits", qw.Kits.Kits,
		)
		return qw.Kits
	}

	// 2. Detection fallback — only when a detector is wired.
	if r.kitDetector == nil {
		return nil
	}
	targetOS := r.kitTargetOS
	if targetOS == "" {
		targetOS = kit.MustResolveOS()
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
// Single-source precedence: a provisioner-stamped identity wins. The cloud box
// provisioner injects the canonical "Rensei Agent" GIT_AUTHOR_*/GIT_COMMITTER_*
// into the box env; when present it is authoritative here, so the runner's
// backstop commits carry the SAME identity as the agent's own in-box commits
// instead of overriding them with a divergent "Donmai Agent" persona. Absent a
// provisioner value (standalone / local worktree), fall back to a
// session-derived default: the issue identifier as the display name and the
// session id as the email so every commit is unambiguously linked to its
// originating session.
func buildSessionEnv(qw QueuedWork) map[string]string {
	// Derive a stable display name: prefer the issue identifier, fall back
	// to a shortened session id prefix.
	gitName := "Donmai Agent"
	if qw.IssueIdentifier != "" {
		gitName = "Donmai Agent (" + qw.IssueIdentifier + ")"
	}
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
		if b.MaxSubAgents > 0 {
			v := fmt.Sprintf("%d", b.MaxSubAgents)
			envMap["DONMAI_STAGE_MAX_SUB_AGENTS"] = v
		}
		if b.MaxTokens > 0 {
			v := fmt.Sprintf("%d", b.MaxTokens)
			envMap["DONMAI_STAGE_MAX_TOKENS"] = v
		}
	}
	return envMap
}

// defaultMCPServers returns the list of MCP servers every session ships
// with by default.
//
// Leads with one HTTP entry per session pointing at the platform's
// per-session MCP endpoint (/api/mcp/<sessionId>). The platform applies
// the A2A capability bundle filter at list-tools time and the
// defense-in-depth allow-list check at tool-call time — see the A2A ADR
// at runs/2026-05-20-adr-a2a-per-session-mcp.md.
//
// When PlatformURL or AuthToken are missing (standalone-mode sessions
// without a platform), the gate entry is omitted: the agent runs without any
// platform MCP gate, which matches the legacy back-compat path.
//
// F.5 code-intel: when qw.CodeIntel is set the runner appends the in-box
// af-code-intelligence stdio plugin (os.Executable() + `mcp code-intel --root
// <wpath>`) AFTER the platform gate. root is the provisioned worktree path
// (loop.go step 2) and MUST be passed explicitly — the caller builds this list
// AFTER Provision so wpath exists. This function is the single place the runner
// extends MCP defaults.
func defaultMCPServers(qw QueuedWork, wpath string) []agent.MCPServerConfig {
	var servers []agent.MCPServerConfig

	// Platform per-session HTTP gate — omitted in standalone mode (no platform
	// creds). Always leads the list so it is never shadowed by a later entry.
	if qw.PlatformURL != "" && qw.AuthToken != "" && qw.SessionID != "" {
		url := strings.TrimRight(qw.PlatformURL, "/") + "/api/mcp/" + qw.SessionID
		servers = append(servers, agent.MCPServerConfig{
			// Brand-derived so OSS renders "donmai-platform" while the closed
			// rensei binary (statehome brand "rensei") renders "rensei-platform"
			// byte-identically. The platform reports its own serverInfo.name
			// independently; this is the client-side label only.
			Name: statehome.Brand() + "-platform",
			Type: "http",
			URL:  url,
			Headers: map[string]string{
				"Authorization": "Bearer " + qw.AuthToken,
			},
		})
	}

	// In-box code-intelligence stdio plugin. Purely in-box — no platform
	// coupling — so it is emitted whenever the capability block is present,
	// including standalone-mode sessions. When the block is nil this is a no-op
	// and the output is byte-identical to the pre-code-intel path.
	if qw.CodeIntel != nil {
		servers = append(servers, codeIntelMCPEntry(wpath, qw.CodeIntel))
	}

	return servers
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
	seen := make(map[string]struct{}, len(defaults)+len(cardServers))
	merged := make([]agent.MCPServerConfig, 0, len(defaults)+len(cardServers))
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

// scanBlocked detects a deliberate agent decline in assistant text and
// returns (reason, true) when found. Two marker forms are recognised,
// both provider-generic (the agent prints them as plain text):
//
//   - "WORK_RESULT:blocked"          — the verdict-marker form, no reason.
//   - "AGENT_BLOCKED: <reason text>" — captures the reason up to EOL.
//
// A blocked signal is a reasoned refusal (ambiguous spec, unmet
// preconditions), NOT a crash. The runner forks to FailureAgentBlocked so
// the deliberate decline is not funneled as a failure that triggers the
// empty-branch backstop or a re-dispatch into the same wall.
//
// Both markers MUST appear at the start of a line (modulo leading
// whitespace and an optional opening HTML-comment fence) to count. This
// mirrors the verdict convention agents are instructed to emit on their
// final line and prevents a false positive when a narrative turn merely
// quotes or discusses the marker mid-sentence (e.g. "I'd emit
// AGENT_BLOCKED if I were stuck, but I'm not"). Without the anchor a
// successful NO-PR-by-design session (research, qa, acceptance, …) whose
// prose mentioned the marker would be reclassified as a decline.
func scanBlocked(text string) (string, bool) {
	if m := agentBlockedRE.FindStringSubmatch(text); m != nil {
		reason := strings.TrimSpace(m[1])
		// Drop a trailing HTML-comment fence so a marker emitted as
		// "<!-- AGENT_BLOCKED: reason -->" yields just the reason.
		reason = strings.TrimSpace(strings.TrimSuffix(reason, "-->"))
		return reason, true
	}
	if workResultBlockedRE.MatchString(text) {
		return "", true
	}
	return "", false
}

var (
	workResultRE = regexp.MustCompile(`(?i)WORK_RESULT[:\s]+(passed|failed|unknown)`)
	prURLRE      = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/\d+`)
	// workResultBlockedRE matches the verdict-marker decline form. Kept
	// separate from workResultRE so the existing passed/failed/unknown
	// transition mapping is untouched (blocked is an outcome, not a QA
	// verdict the Linear status mapper should consume). Anchored to
	// line-start (with an optional HTML-comment fence) so it fires only on
	// a deliberate verdict line, not on a quote of the marker in prose.
	workResultBlockedRE = regexp.MustCompile(`(?im)^\s*(?:<!--\s*)?WORK_RESULT[:\s]+blocked`)
	// agentBlockedRE captures the reason from "AGENT_BLOCKED: <reason>"
	// up to the end of the line. Anchored the same way as
	// workResultBlockedRE to avoid mid-sentence false positives.
	agentBlockedRE = regexp.MustCompile(`(?im)^\s*(?:<!--\s*)?AGENT_BLOCKED[:\s]+([^\r\n]+)`)
)

// _ silences unused-import warnings for json when the package only
// imports it transitively. Kept so future hooks can re-enable.
var _ = json.Marshal
