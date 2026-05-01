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
)
