// Package state owns the per-worktree .agent/state.json file used for
// session persistence and crash recovery.
//
// The legacy TS analogue lives in
// ../agentfactory/packages/core/src/orchestrator/state-recovery.ts. The
// shape ported here keeps wire compatibility with the legacy reader so
// a worktree initialized by either runner is recoverable by the other
// during the F.0/F.5 migration window.
//
// Each worktree contains a .agent/ directory with state.json (this
// package) and heartbeat.json (runtime/heartbeat). The state file is
// written atomically (tmpfile + rename) and protected by a per-file
// flock so concurrent updates inside a single worktree serialize.
//
// Cross-issue recovery is explicitly refused: if a Read finds state
// belonging to a different issue identifier than the caller expects,
// the call returns ErrIdentifierMismatch — the orchestrator must clean
// the worktree before reusing it.
package state
