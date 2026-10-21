package runner

// signalGuardProtocolPartial is the agent-brief guidance for the
// session-scoped signal guards. It tells the agent the one rule it needs:
// kill by PID; pattern kills (pkill -f, killall) are scoped to the session
// and anything outside is refused and reported.
const signalGuardProtocolPartial = "# Process signalling\nKill by PID (`kill <pid>`). Pattern kills (`pkill -f <pattern>`, `killall <name>`) are scoped to your session: only processes inside your session worktree or your session's process group can be signalled. A pattern that also matches processes outside your session is refused for those processes and reported — the refusal is visible in the session activity, not silent. Not scoped: absolute tool paths (`/usr/bin/pkill`), clearing the session scope variables, `kill $(pgrep ...)` composition, and login shells (`bash -lc`, `zsh -l`) that rebuild PATH around the guards."

// signalGuardDegradedProtocolPartial is the agent-brief guidance when the
// session's guard install failed: the guards are absent, so pattern kills
// are unscoped and the agent must prefer kill-by-PID.
const signalGuardDegradedProtocolPartial = "# Process signalling (degraded)\nKill by PID (`kill <pid>`). The session pattern-kill guards are unavailable for this run, so `pkill`/`killall` are UNSCOPED and can signal processes outside your session. Prefer `kill <pid>`; any pattern kill may affect other sessions or host processes."

func injectSignalGuardProtocolPartial(existing string) string {
	if existing == "" {
		return signalGuardProtocolPartial
	}
	return existing + "\n\n" + signalGuardProtocolPartial
}

// injectSignalGuardDegradedProtocolPartial appends the degraded brief
// (guards unavailable, pattern kills unscoped) instead of the scoped one.
func injectSignalGuardDegradedProtocolPartial(existing string) string {
	if existing == "" {
		return signalGuardDegradedProtocolPartial
	}
	return existing + "\n\n" + signalGuardDegradedProtocolPartial
}
