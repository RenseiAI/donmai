package runner

// signalGuardProtocolPartial is the agent-brief guidance for the
// session-scoped signal guards. It tells the agent the one rule it needs:
// kill by PID; pattern kills (pkill -f, killall) are scoped to the session
// and anything outside is refused and reported.
const signalGuardProtocolPartial = "# Process signalling\nKill by PID (`kill <pid>`). Pattern kills (`pkill -f <pattern>`, `killall <name>`) are scoped to your session: only processes inside your session worktree or your session's process group can be signalled. A pattern that also matches processes outside your session is refused for those processes and reported — the refusal is visible in the session activity, not silent."

func injectSignalGuardProtocolPartial(existing string) string {
	if existing == "" {
		return signalGuardProtocolPartial
	}
	return existing + "\n\n" + signalGuardProtocolPartial
}
