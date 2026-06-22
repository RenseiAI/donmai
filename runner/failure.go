package runner

// Failure-mode classification constants for [Result.FailureMode].
//
// The values are stable wire strings so platform-side dashboards and
// Linear comments can dispatch off them without scraping log lines.
// Add new values at the bottom; never repurpose an existing one.
const (
	// FailureWorktreeProvision indicates the worktree manager could
	// not provision a worktree after MaxSpawnRetries attempts. Often
	// preceded by a transient git-conflict error or a lost-ownership
	// short-circuit (see FailureLostOwnership).
	FailureWorktreeProvision = "worktree-provision"

	// FailurePromptRender indicates the prompt builder rejected the
	// QueuedWork (typically because the caller passed empty issue
	// context). Permanent — retrying without changing the input
	// will fail the same way.
	FailurePromptRender = "prompt-render"

	// FailureProviderResolve indicates the runner could not resolve
	// the requested provider name in its registry. Permanent. Often
	// indicates a misconfigured ResolvedProfile.Provider.
	FailureProviderResolve = "provider-resolve"

	// FailureSpawn indicates Provider.Spawn returned an error before
	// the events channel opened (e.g. CLI binary missing, app-server
	// unreachable). Wraps agent.ErrSpawnFailed.
	FailureSpawn = "spawn-failed"

	// FailureProviderError indicates the provider emitted an
	// ErrorEvent before any terminal ResultEvent. The error message
	// surfaces via Result.Error.
	FailureProviderError = "provider-error"

	// FailureSilentExit indicates the provider closed the events
	// channel without emitting either a ResultEvent or an ErrorEvent.
	// The runner synthesizes a failure record for these cases.
	FailureSilentExit = "silent-exit"

	// FailureLostOwnership indicates the per-session heartbeat tripped
	// its 3-strike threshold mid-session (or the worktree manager
	// detected ownership loss between retries). The runner cancels
	// the provider via Handle.Stop and tears down without backstop.
	FailureLostOwnership = "lost-ownership"

	// FailureTimeout indicates ctx was cancelled before the session
	// terminated. Surfaces when the daemon's per-session deadline
	// expires.
	FailureTimeout = "timeout"

	// FailureBackstop indicates the deterministic git backstop ran
	// but failed to push or open a PR; diagnostics live on
	// Result.BackstopReport.Diagnostics.
	FailureBackstop = "backstop-failed"

	// FailureKitProvision indicates a kit toolchain-install command or a
	// post_acquire hook exited non-zero before the agent spawned (Seam 2:
	// "no partial toolchain"). The session aborts; the agent never starts.
	// The failing command + exit code surface via Result.Error.
	FailureKitProvision = "kit-provision"

	// FailureAgentBlocked indicates the agent terminated normally but
	// DELIBERATELY declined to do the work — it judged the spec ambiguous,
	// the preconditions unmet, or the task outside its remit, and said so
	// instead of producing code. This is distinct from a crash
	// (FailureProviderError), a silent exit (FailureSilentExit), or a
	// budget/timeout cut-off: the agent made a reasoned decision to stop.
	//
	// The signal is structural — the agent emits an explicit decline
	// marker ("WORK_RESULT:blocked" or "AGENT_BLOCKED: <reason>") which
	// the runner scans for. A blocked result MUST NOT trigger the empty-
	// branch backstop or steering (there is nothing to recover), and the
	// platform side should surface it as a needs-clarification outcome
	// rather than re-dispatching the identical context — re-dispatch only
	// re-runs the agent into the same wall while re-billing the full
	// context-assembly prefix.
	FailureAgentBlocked = "agent-blocked"

	// FailureOperatorCancelled indicates the platform deterministically
	// asked the session to stop — the lock-refresh response carried
	// {"stop": true}, which the heartbeat pulser surfaces by closing
	// LostOwnership immediately (the fast in-band leg of the cancel wire,
	// Guard 3). It is distinct from FailureLostOwnership (a 3-strike
	// heartbeat fuse or a hand-off) because it is an intentional operator
	// action: the work was cancelled on purpose, so the platform MUST NOT
	// blind-re-dispatch it (re-dispatch would just re-run the cancelled
	// work and re-bill the context prefix). Routing mirrors
	// FailureAgentBlocked — a terminal, non-retryable outcome the platform
	// surfaces as cancelled rather than failed-and-retry.
	FailureOperatorCancelled = "operator-cancelled"

	// FailureNoProgress indicates the idle/no-progress watchdog fired:
	// the event stream produced no agent.Event for longer than the
	// configured Options.IdleTimeout window. This catches the
	// wedged-but-channel-alive class — a session whose events channel is
	// still open (so it is not a silent exit or a closed channel) but
	// which has stopped making forward progress (no tool calls, no
	// assistant text, no terminal Result). The watchdog cancels the
	// stream context, so the runner observes ctx cancellation; this
	// failure mode disambiguates the watchdog cut-off from a genuine
	// ctx/deadline timeout (FailureTimeout) so the platform can route a
	// stuck session distinctly rather than blind-re-dispatching it.
	FailureNoProgress = "no-progress"
)
