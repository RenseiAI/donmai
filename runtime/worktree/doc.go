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
//
// # Dispatched pull requests
//
// A session may be dispatched against an existing pull request rather than a
// bare branch. The work source then carries an optional record —
// workarea.PullRequestV1, on the wire as the session detail's "pullRequest"
// object — naming the pull request's number, its head commit, the base commit,
// and the local ref the agent is told to use:
//
//	{"owner":…, "repo":…, "number":7, "url":…,
//	 "headSha":…, "headRef":…, "baseSha":…, "baseRef":…,
//	 "title":…, "state":…, "localRef":"refs/pull/7/head"}
//
// ProvisionSpec.PullRequest carries it into StrategyClone, which after the
// clone and before the agent starts:
//
//  1. checks owner/repo against the repository this session clones, failing
//     with ErrPullRequestRepositoryMismatch before any git command runs;
//  2. fetches refs/pull/<number>/head into localRef over `origin` — the
//     remote, and therefore the credential, the clone already used, and the
//     only remote this step ever contacts;
//  3. resolves localRef and compares it with headSha, failing with
//     ErrPullRequestHeadMismatch — naming BOTH commits — when they differ;
//  4. requires baseSha to be reachable, failing and naming it when it is not.
//     The base is never fetched and there is no fallback to baseRef.
//
// Step 3 is the point of the feature. A pull request's head moves on every
// push and is rewritten by a force-push, so a fetch without a check would
// silently run the session against whatever the branch points at when
// provisioning happens, attributing a review, an approval or a follow-up
// commit to a head nobody chose. Step 4 refuses the same substitution on the
// other side of the diff: repairing an unreachable base by fetching the base
// branch's current tip would quietly change what the change is compared
// against. Step 1 is why neither can involve another repository. All of these
// failures are deterministic, so isRetriable refuses them: the session fails
// once, with a stated reason, instead of burning three spawn attempts.
//
// The agent receives no token and no forge API access from any of this. The
// record is inert data; the single credentialed operation is the git fetch,
// performed by the provisioning process, and what the agent gets is commits.
//
// A nil PullRequest — every session that is not dispatched against a pull
// request — leaves the clone step byte-identical to a manager that never knew
// about pull requests. The multi-repository declaration path builds its own
// per-repository specs and deliberately names no pull request.
package worktree
