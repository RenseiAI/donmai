// Package activityrelay is the shared harness mechanism that carries
// lifecycle-hook telemetry from a spawned CLI to the runner.
//
// Both interactive seats (the Claude REPL and the Codex TUI) accept
// per-launch hook definitions that run a command on lifecycle moments
// (session start, prompt submit, stop, permission request). Each hook
// command is a tiny POSIX script that appends one JSON line to a
// per-session relay file and exits 0 without printing anything. The
// harness tails that file and surfaces each line as an agent.SystemEvent
// on the handle's Events channel, which the runner forwards to the
// activity stream.
//
// The hook parses nothing and decides nothing: the event name is baked
// into its argv, the relay path is baked into its body, and the CLI's
// own stdin payload is forwarded verbatim as an opaque object. A hook
// that has to render a decision or dig a field out of JSON is a hook
// that corrupts telemetry on the first quote character — and, worse, a
// hook whose output the CLI interprets (a Stop decision, a permission
// verdict) could steer the session. These hooks are observe-only by
// construction: they print nothing, so the CLI treats every fire as an
// abstention and the session proceeds exactly as it would with no hooks
// installed.
package activityrelay
