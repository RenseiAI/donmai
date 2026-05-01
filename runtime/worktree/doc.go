// Package worktree provisions and tears down per-session git worktrees
// for the agent runner.
//
// Per F.1.1 §1, the runner clones a parent repository under a daemon-
// configured directory and then either (a) clones a fresh sibling for
// each session or (b) creates a `git worktree add` off the parent.
// Both strategies are supported via CloneStrategy. The retry contract
// mirrors the legacy TS git-worktree.ts:
//
//   - MaxSpawnRetries = 3
//   - SpawnRetryDelay = 15s
//   - Before each retry, the platform-side session ownership is
//     probed; if another worker has claimed the session, Provision
//     returns ErrLostOwnership without burning further retries.
//
// The package does not own the platform API contract — it accepts an
// OwnershipProber callback so unit tests can stub the probe and
// production code can wire the real afclient.GetSession lookup.
package worktree
