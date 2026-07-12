// Package shell is the minimal, interactive-only harness (W4): it spawns the
// user's own login shell ($SHELL, falling back to DefaultShell) under a PTY
// via provider/harness/ptycli. It is the "plain-shell" spawn mode named
// alongside claude/codex in the interactive-attach-v1 wave plan.
//
// shell drives no model endpoint and has no headless mode — every other
// harness in this repo pairs a headless agent loop with (for claude/codex)
// an additional interactive spawn mode; shell is only the latter. Its
// non-interactive Spawn (Spec.Interactive == nil) returns a clear
// agent.ErrUnsupported rather than silently doing nothing, since there is no
// fallback behavior to silently do.
package shell
