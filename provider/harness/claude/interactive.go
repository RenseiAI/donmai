package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/google/uuid"
)

// spawnInteractive opens the claude CLI's OWN interactive REPL — bare
// `claude`, with NEITHER `-p`/`--print` NOR `--output-format stream-json`
// (those are the headless one-shot flags buildArgs uses for the default
// loop) — under a PTY via ptycli, seeded with spec.Prompt when set and
// carrying the session-level flags interactiveArgs maps from the Spec.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportCLIInjection (the headless loop's transport); TransportPTY
// is not claude's declared Transport, only its interactive spawn mode. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment for why the two are orthogonal, and provider/harness/shell for the
// contrasting harness whose ONLY transport is PTY.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up (SessionID stays empty — claude's session id is only observable
// through its JSONL headless-mode output, which the interactive REPL never
// emits) and a single terminal ResultEvent when the CLI process exits.
func (p *Provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	mcpPath, err := clijsonl.WriteMCPConfigWithEnv(spec.MCPServers, spec.Env)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Unattended trust: a bare interactive REPL parks on the workspace
	// trust modal for any cwd the CLI has not recorded as trusted, and
	// there is no flag that skips it — pre-seeding the record is the only
	// unattended path. Seed exactly the platform-authorized boundary
	// (the session cwd plus its declared mutable paths, resolved) before
	// the child starts. Best-effort: a seed that cannot be written still
	// spawns, and the modal — not a spawn failure — says so. The seed record
	// is kept so the session-end cleanup removes exactly these entries and
	// leaves pre-existing trust untouched (see removeSeededTrust).
	seed := seedWorkspaceTrust(spec)

	// The Stop-hook notice channel is BEST EFFORT at spawn time and load-
	// bearing at delivery time. A drop directory that cannot be created is a
	// session with no live-turn delivery, which is a degraded session — it is
	// not a failed one, because the durable mailbox is the floor and the agent
	// can still pull. So a setup failure spawns the session WITHOUT the hook
	// and lets the runner report the absence per message (it dead-letters
	// against a declared-but-absent channel, so the producer learns), rather
	// than killing a session over a latency optimisation.
	hook, hookErr := newStopHookChannel()
	// Pin a fresh Claude conversation identity before spawn, so its transcript
	// can be found even if the process dies before invoking its first Stop hook.
	sessionID := uuid.NewString()
	if hookErr == nil {
		hook.usageSessionID = sessionID
		hook.usageConfigDir = interactiveUsageConfigDir(spec.Env)
	}
	settings := ""
	if hookErr == nil {
		// The routing pin rides this same --settings string (see
		// pinnedRoutingEnv): the CLI resolves that layer above the process
		// environment and above repo settings files, so the session bearer
		// goes where the resolved binding says no matter what repo-local
		// harness config names.
		//
		// Only the declarative routing destinations are pinned here. Repo
		// config that runs code (hooks, apiKeyHelper and other helpers,
		// stdio MCP servers, the agent's own tools) still runs with the
		// session environment once the repo path is trusted, exactly as on
		// the headless lane — that execution surface is the accepted model
		// for a trusted repo, not something the pin closes.
		if settings, err = hook.settingsJSONWithPin(pinnedRoutingEnv(spec)); err != nil {
			// Fail closed: the Stop-hook channel is best-effort, but the
			// routing pin is not. A session that spawned without the pin
			// would let a repo-local settings file redirect its bearer,
			// so fall back to a pin-only --settings rather than none.
			_ = hook.close()
			hook = nil
			if pinOnly, pinErr := marshalRoutingPin(pinnedRoutingEnv(spec)); pinErr == nil && pinOnly != "" {
				settings = pinOnly
			} else {
				settings = ""
			}
		}
	} else {
		hook = nil
		// Fail closed without the hook channel too: carry the pin alone
		// rather than spawning unpinned.
		if pinOnly, pinErr := marshalRoutingPin(pinnedRoutingEnv(spec)); pinErr == nil && pinOnly != "" {
			settings = pinOnly
		}
	}

	cleanup := func() error {
		// Remove the trust entries this session seeded, first: the record
		// is still present at cleanup time, and leaving the seed behind
		// would trust the worktree for every later session on the host.
		// Best-effort and silent — cleanup never fails a session that
		// already ran.
		removeSeededTrust(seed)
		// Pin the transcript path BEFORE the drop is removed: the terminal
		// event fires after this cleanup, and the exit usage accounting
		// reads the transcript file through the pinned path.
		if hook != nil {
			hook.snapshotTranscriptPath()
		}
		errs := []error{clijsonl.RemoveMCPConfig(mcpPath)}
		if hook != nil {
			errs = append(errs, hook.close())
		}
		return errors.Join(errs...)
	}

	argv := append([]string{"--session-id", sessionID}, interactiveArgsWith(spec, mcpPath, settings)...)
	h, err := ptycli.SpawnWithCleanup(ctx, p.binary, argv, spec, p.Manifest(), cleanup)
	if err != nil {
		return nil, err
	}
	if hook == nil {
		return h, nil
	}
	return wrapInteractiveHandle(h, hook), nil
}

// interactiveEventSlots sizes the wrapped handle's event channel. The coarse
// events — InitEvent and the terminal ResultEvent — always have a reserved
// slot, so their blocking sends complete even when nobody drains Events.
// Transcript usage only ever fills the remaining slots, and is dropped (never
// queued) once they are taken.
const (
	interactiveCoarseEventSlots = 2
	interactiveUsageEventSlots  = 64
)

// wrapInteractiveHandle adds the Stop-hook pull channel and the transcript
// usage tail to a spawned PTY handle. The usage tail reads the session's own
// transcript — located through the hook or pinned conversation id — and maps each new
// assistant message.usage to an LlmCallEvent, so per-turn usage reaches the
// runner's activity sink live; the terminal ResultEvent carries the deduped
// session totals instead of the PTY's costless one.
//
// Ordering: usage the child wrote on its way out can only be read after
// InteractiveSession().Done closes, so the final flush is enqueued strictly
// after Done. ActivityFlushed closes once that flush is enqueued, and the
// terminal ResultEvent is forwarded only after it.
//
// Teardown: every helper goroutine returns at session end whether or not the
// caller drains Events — the tailer stops on Done and never blocks, and the
// coarse sends always fit the reserved slots — after which Events closes.
func wrapInteractiveHandle(h *ptycli.Handle, hook *stopHookChannel) *interactiveHandle {
	w := &interactiveHandle{
		Handle:          h,
		notices:         hook,
		events:          make(chan agent.Event, interactiveCoarseEventSlots+interactiveUsageEventSlots),
		activityFlushed: make(chan struct{}),
		finished:        make(chan struct{}),
	}
	session := h.InteractiveSession()
	// emit forwards one usage event best-effort. Only the tailer goroutine
	// calls it, and it never takes a slot reserved for the coarse events:
	// with at most interactiveCoarseEventSlots coarse sends over the handle's
	// life, the coarse senders below can never block on a channel usage
	// events filled. When the consumer is slow the event is dropped rather
	// than blocking the tailer behind the terminal.
	emit := func(ev agent.Event) {
		if len(w.events) >= cap(w.events)-interactiveCoarseEventSlots {
			return
		}
		select {
		case w.events <- ev:
		default:
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for event := range h.Events() {
			if result, terminal := event.(agent.ResultEvent); terminal {
				// ptycli sends the result strictly after Done; the tailer's
				// final flush, which also starts at Done, goes first.
				<-w.activityFlushed
				w.events <- withTranscriptCost(result, hook)
				continue
			}
			w.events <- event
		}
	}()
	// Transcript usage tail: while the session runs, its JSONL transcript is
	// tailed and each new assistant message.usage is forwarded as the same
	// LlmCallEvent the headless lane emits, so an interactive session's
	// spend reaches the session activity stream. Best-effort and
	// rate-limited (see interactive_usage.go). It stops when the PTY child
	// has exited and drained, after one final flush of trailing writes.
	go func() {
		defer wg.Done()
		defer close(w.activityFlushed)
		tailer := newInteractiveUsageTailer(hook)
		tailer.run(session.Done(), emit)
	}()
	go func() {
		wg.Wait()
		close(w.events)
		close(w.finished)
	}()
	return w
}

// withTranscriptCost replaces a costless terminal ResultEvent with one
// carrying the session's deduped transcript totals, or its saved statusline
// cumulative USD cost when no transcript usage is available. Unknown or untrusted
// usage leaves the event untouched: no fabricated zero, never a failure.
func withTranscriptCost(result agent.ResultEvent, hook *stopHookChannel) agent.Event {
	if result.Cost != nil || hook == nil {
		return result
	}
	total, err := hook.exitUsage()
	if err != nil || total == (agent.CostData{}) {
		return result
	}
	result.Cost = &agent.CostData{
		InputTokens:       total.InputTokens,
		OutputTokens:      total.OutputTokens,
		CachedInputTokens: total.CachedInputTokens,
		CacheWriteTokens:  total.CacheWriteTokens,
		NumTurns:          total.NumTurns,
		TotalCostUsd:      total.TotalCostUsd,
	}
	return result
}

// interactiveHandle adds the Stop-hook pull channel to the shared PTY handle.
//
// Embedding *ptycli.Handle rather than reimplementing it keeps agent.Handle and
// agent.InteractiveCapable behaviour byte-identical to every other interactive
// harness; the only thing this type adds is the door the runner delivers
// through, plus the session's own transcript usage at exit (the REPL has no
// stream-json wire, so without it an interactive session would end with no
// cost). Events carries the coarse Init/Result contract plus per-turn usage
// tailed from the session's transcript; the terminal ResultEvent carries the
// deduped session totals.
type interactiveHandle struct {
	*ptycli.Handle
	notices *stopHookChannel
	events  chan agent.Event
	// activityFlushed closes once the transcript tailer has enqueued its
	// final flush (or given up on a full channel) and returned.
	activityFlushed chan struct{}
	// finished closes after events is closed, i.e. once every helper
	// goroutine has returned.
	finished chan struct{}
}

var (
	_ agent.InteractiveCapable         = (*interactiveHandle)(nil)
	_ agent.NoticeChannelCapable       = (*interactiveHandle)(nil)
	_ agent.InteractiveActivityFlusher = (*interactiveHandle)(nil)
)

// NoticeChannel returns this session's Stop-hook channel.
func (h *interactiveHandle) NoticeChannel() agent.NoticeChannel { return h.notices }

// ContextUsageSidecarPath returns the session's statusline sidecar path:
// the file the session's own statusLine command maintains with the live
// context_window numbers. It implements the runner's session-scoped
// context-source seam (agent.ContextUsageSourcer): the interactive watcher
// polls this file — never a process-wide variable — so one session's
// readings can never land in another session's series. Empty when the
// session has no channel (a spawn without the hook still runs, without
// context notices).
func (h *interactiveHandle) ContextUsageSidecarPath() string {
	if h == nil || h.notices == nil {
		return ""
	}
	return h.notices.sidecarPath()
}

// ContextUsageRollout reports no rollout: the claude leg is the sidecar,
// never a rollout file. It satisfies the runner's session-scoped seam
// (agent.ContextUsageSourcer) alongside ContextUsageSidecarPath.
func (h *interactiveHandle) ContextUsageRollout() (string, string) { return "", "" }

// Events overrides ptycli.Handle.Events with the coarse Init/Result contract
// plus per-turn usage tailed from the session's own transcript (the runner
// forwards LlmCallEvents through its activity sink) and, after Done, the
// final flush plus the cost-bearing terminal ResultEvent. The terminal event
// is forwarded only after the flush, so a consumer reading to the ResultEvent
// (or waiting on ActivityFlushed after Done) sees every delivered usage event
// before the session totals.
func (h *interactiveHandle) Events() <-chan agent.Event { return h.events }

// ActivityFlushed implements agent.InteractiveActivityFlusher: it closes once
// the session's final transcript usage has been enqueued on Events.
func (h *interactiveHandle) ActivityFlushed() <-chan struct{} { return h.activityFlushed }

// interactiveArgs builds the argv for claude's own interactive REPL.
//
// Spec → CLI mapping (interactive spawn mode):
//
//	SessionName        → --name <name>
//	Model              → --model <id>
//	Effort             → --effort <level>
//	Autonomous         → --permission-mode bypassPermissions
//	AllowedTools       → --allowedTools <comma-separated>
//	DisallowedTools    → --disallowedTools <comma-separated>
//	SystemPromptAppend → --append-system-prompt <text>
//	Prompt             → positional argument, ALWAYS LAST
//
// The allow/deny mapping is byte-identical to buildArgs' (see
// cli_args.go) — one convention, two spawn modes. Before it existed the
// interactive REPL silently DROPPED Spec.AllowedTools/Spec.DisallowedTools
// and fell back to the CLI's own default permission set, so a stamped
// session that adapted cleanly on the headless lane denied closed on the
// interactive lane even though the CLI honors the same flags there: the
// flags are global session options (not --print-gated), feed the same
// permission context the REPL's /permissions panel renders, and a live
// interactive probe shows a stamped allow entry under Allow and a stamped
// deny entry under Deny. The CLI simply was not being asked.
//
// The Autonomous mapping is deliberately byte-identical to buildArgs' (see
// cli_args.go) — one convention, two spawn modes. Before it existed the
// interactive REPL silently DROPPED Spec.Autonomous and fell back to the
// CLI's own default permission mode, so a headless run and an interactive
// run built from the SAME Spec got DIFFERENT permission postures.
// --permission-mode is a session-level flag the REPL honors exactly as the
// headless invocation does, so the divergence was never capability gating
// (the CLI can honor it) — the CLI simply was not being asked.
//
// Model was the same class of bug: a work order dispatched under
// host-session/local auth with a platform-resolved model
// (QueuedWork.ResolvedProfile.Model → Spec.Model via translateSpec) reached
// the headless spawn mode's --model flag (buildArgs, cli_args.go) but the
// interactive REPL never read Spec.Model at all, so an interactive session
// silently ran under the CLI's OWN default model regardless of what the
// platform composer selected. There is no local mechanism to validate a
// model id before spawn — the CLI is the sole authority — so this mapping
// is a pure pass-through: an id the CLI rejects surfaces as the CLI's own
// nonzero exit, which ptycli.buildResult turns into a failed
// agent.ResultEvent (see runner.TestInteractive_ExitDetailIsNotASummary),
// not a silent fallback to the wrong model.
//
// The claude CLI accepts a positional prompt to seed the first message of
// an interactive session — `claude "fix the bug"` launches the REPL with
// that initial prompt already queued, distinct from `claude -p "..."`
// (headless, one-shot, no session) which buildArgs uses. An empty prompt
// starts the REPL bare, with no seeded message. The positional prompt must
// stay LAST so it is never consumed as the value of a preceding flag.
func interactiveArgs(spec agent.Spec) []string {
	return interactiveArgsWith(spec, "", "")
}

func interactiveArgsWithMCP(spec agent.Spec, mcpConfigPath string) []string {
	return interactiveArgsWith(spec, mcpConfigPath, "")
}

// interactiveArgsWith adds the notice-channel settings to the interactive argv.
//
// settingsJSON is passed as `--settings <json string>`, not as a file. The CLI
// accepts either, and the string form is what keeps the hook definition out of
// the session's worktree entirely — a settings FILE there is a file the agent
// can read, edit, or commit by accident.
//
// --settings is ADDITIVE: it layers onto whichever base layer the CLI's own
// --setting-sources selects, so declaring `hooks` here adds this Stop hook
// without replacing the user's configuration. That is why no --setting-sources
// is passed: suppressing the base layer would silently strip the operator's own
// settings from every interactive session, which is a far larger change than
// the one this flag is here to make.
func interactiveArgsWith(spec agent.Spec, mcpConfigPath, settingsJSON string) []string {
	var argv []string
	if spec.SessionName != "" {
		argv = append(argv, "--name", spec.SessionName)
	}

	// The interactive REPL accepts the same --add-dir working-directory
	// grants as the headless lane (buildArgs): every declared mutable
	// repository path, with the session CWD first. It must not diverge
	// from buildArgs' mapping — see additionalWorkdirs.
	for _, dir := range additionalWorkdirs(spec) {
		argv = append(argv, "--add-dir", dir)
	}

	// Notice channel first: it is a session-level flag like the rest, and every
	// flag-shaped argument must precede the positional prompt.
	if settingsJSON != "" {
		argv = append(argv, "--settings", settingsJSON)
	}

	// Model: mirrors buildArgs' identical branch (cli_args.go) so a
	// platform-resolved model reaches the interactive REPL the same way it
	// reaches a headless run of the same Spec. Empty leaves the REPL on the
	// CLI's own default — no flag emitted.
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	// Effort: mirrors buildArgs' identical branch (cli_args.go) so a
	// configured reasoning effort reaches the interactive REPL the same way
	// it reaches a headless run. The REPL accepts --effort exactly as the
	// headless CLI does. Spawn deliberately does NOT fix
	// CLAUDE_CODE_EFFORT_LEVEL here (effort.go): the operator keeps their
	// own effort control — the flag, the in-session effort command, and
	// saved preferences — on interactive sessions. Only stamped headless
	// sessions get the forced variable.
	// The flag is omitted unless the level is one Claude recognises: an
	// unrecognised stored value would be silently ignored by the CLI, which
	// then falls back to the operator's saved level while the session
	// records a different one.
	if spec.Effort.Known() {
		argv = append(argv, "--effort", string(spec.Effort))
	}

	// Permission mode: autonomous sessions get bypassPermissions so the
	// REPL does not stall on an approval prompt no human is present to
	// answer. Non-autonomous sessions inherit the CLI default. Mirrors
	// buildArgs' identical branch.
	if spec.Autonomous {
		argv = append(argv, "--permission-mode", "bypassPermissions")
	}

	// An operator-driven session whose control plane granted toolApproval
	// "bypass" may still choose bypassPermissions from the REPL (Shift+Tab).
	// The CLI only offers that mode when it was started with this flag; the
	// session itself starts in the CLI default mode. This matches the adapter's
	// declared rendering (execution_security.go: index 0, prompts skipped).
	if !spec.Autonomous && spec.ExecutionSecurity != nil &&
		spec.ExecutionSecurity.Levels.ToolApproval == agent.ToolApprovalBypass {
		argv = append(argv, "--allow-dangerously-skip-permissions")
	}

	// SystemPromptAppend carries the composed session instructions assembled
	// by the runner. The Claude REPL accepts the same flag as the headless CLI;
	// omit it when empty so a bare interactive session stays bare.
	if spec.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", spec.SystemPromptAppend)
	}

	// Tool policy: mirrors buildArgs' identical block (cli_args.go) so a
	// stamped allow/deny list reaches the interactive REPL the same way it
	// reaches a headless run of the same Spec. The REPL honors --allowedTools
	// / --disallowedTools exactly as the headless CLI does (same permission
	// context; the REPL's own permission panel renders the stamped entries),
	// so this mapping is a pure pass-through of the CLI's native grammar,
	// including the MCPToolNames merge into --allowedTools: the interactive
	// profile declares the same MCP tool-policy delivery as headless, and a
	// name filter stamped for one lane must reach the other.
	allowed := make([]string, 0)
	allowed = append(allowed, spec.AllowedTools...)
	allowed = append(allowed, spec.MCPToolNames...)
	allowed = dedupAndSort(allowed)
	if len(allowed) > 0 {
		argv = append(argv, "--allowedTools", strings.Join(allowed, ","))
	}

	// Disallowed tools always include the Linear MCP block list when
	// the session is autonomous (matches the legacy provider and buildArgs).
	disallowed := append([]string(nil), spec.DisallowedTools...)
	if spec.Autonomous {
		disallowed = append(disallowed, linearMCPDisallowList...)
	}
	disallowed = dedupAndSort(disallowed)
	if len(disallowed) > 0 {
		argv = append(argv, "--disallowedTools", strings.Join(disallowed, ","))
	}

	// Claude's interactive REPL accepts the same exact per-session MCP file as
	// headless mode. Strict mode prevents ambient user MCP config from silently
	// widening the session tool surface.
	if mcpConfigPath != "" {
		argv = append(argv, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}

	// Positional prompt LAST — every flag-shaped argument precedes it.
	if spec.Prompt != "" {
		argv = append(argv, spec.Prompt)
	}
	return argv
}
