package claude

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
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

	// The Stop-hook notice channel is BEST EFFORT at spawn time and load-
	// bearing at delivery time. A drop directory that cannot be created is a
	// session with no live-turn delivery, which is a degraded session — it is
	// not a failed one, because the durable mailbox is the floor and the agent
	// can still pull. So a setup failure spawns the session WITHOUT the hook
	// and lets the runner report the absence per message (it dead-letters
	// against a declared-but-absent channel, so the producer learns), rather
	// than killing a session over a latency optimisation.
	hook, hookErr := newStopHookChannel()
	settings := ""
	if hookErr == nil {
		if settings, err = hook.settingsJSON(); err != nil {
			_ = hook.close()
			hook, settings = nil, ""
		}
	} else {
		hook = nil
	}

	cleanup := func() error {
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

	h, err := ptycli.SpawnWithCleanup(ctx, p.binary,
		interactiveArgsWith(spec, mcpPath, settings), spec, p.Manifest(), cleanup)
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
// transcript — located through the hook's durable locator — and maps each new
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
// carrying the session's deduped transcript totals. A transcript that cannot
// be read — the hook never fired, the file never materialized — leaves the
// event untouched: no usage is reported, never a failure.
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
//	SystemPromptAppend → --append-system-prompt <text>
//	Prompt             → positional argument, ALWAYS LAST
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

	// SystemPromptAppend carries the composed session instructions assembled
	// by the runner. The Claude REPL accepts the same flag as the headless CLI;
	// omit it when empty so a bare interactive session stays bare.
	if spec.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", spec.SystemPromptAppend)
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
