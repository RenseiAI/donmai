package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

// siblingReposEnv is the work-item env key naming the read-only context
// repositories the runner materializes inside the session workarea root
// (ADR-2026-07-07-sibling-context-repos, as amended for session-owned
// workarea namespaces). Value: comma-separated entries, each `<git-url>`
// or `<git-url>#<ref>`.
const siblingReposEnv = "DONMAI_SIBLING_REPOS"

// siblingLocks serializes provisioning per target directory. Session-owned
// roots already give every session its own leaf for a shared context repo,
// so cross-session collisions cannot happen; the lock remains for the
// retained flat layout, where two sessions still share the worktree root as
// a parent. Keys are cleaned absolute target paths; values are *sync.Mutex.
// Entries are never removed — the set of context repos a host materializes
// is small and stable across a process lifetime.
var siblingLocks sync.Map

// provisionSiblings materializes the read-only context repos named by
// DONMAI_SIBLING_REPOS as per-session leaves inside the session workarea
// root, so agents find their governing corpus at ../<name> relative to the
// selected repository exactly as repo AGENTS.md contracts promise.
//
// Under the session-owned layout <worktree-root>/<session-id>/<repo-leaf>,
// "../<name>" resolves to <worktree-root>/<session-id>/<name> — a leaf this
// session owns, never a global peer shared with unrelated sessions.
//
// Env source: the daemon injects the work item's per-session env map
// into the worker child's process env (worker_spawner composeEnv), and
// runner.QueuedWork carries no env field of its own — so the process
// env is the single read point for both daemon-spawned and standalone
// runs. qw is accepted for signature stability should a per-session env
// field land on the wire later.
//
// Failure is never fatal: every skip or error logs a warning and the
// session proceeds — agents fall back to cloning the repo themselves.
func (r *Runner) provisionSiblings(ctx context.Context, qw QueuedWork, layout workarea.Layout) {
	_ = qw // no per-session env field on QueuedWork today; see doc comment.
	spec := strings.TrimSpace(os.Getenv(siblingReposEnv))
	if spec == "" {
		return
	}
	provisionSiblingRepos(ctx, r.logger, spec, layout)
}

// provisionSiblingRepos clones or freshens each entry of spec into a
// per-session leaf of layout.Root. Pure worker for provisionSiblings; split
// out so tests can drive it without touching the process env.
func provisionSiblingRepos(ctx context.Context, logger *slog.Logger, spec string, layout workarea.Layout) {
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		url, ref, _ := strings.Cut(entry, "#")
		name := workarea.RepositoryLeaf(url)
		target, err := layout.SiblingPath(name)
		if err != nil {
			logger.Warn("sibling repo skipped: unusable directory name",
				"entry", entry, "name", name, "err", err)
			continue
		}
		if err := ensureSibling(ctx, logger, target, url, ref); err != nil {
			logger.Warn("sibling repo provision failed (non-fatal)",
				"url", url, "ref", ref, "path", target, "err", err)
			continue
		}
		logger.Info("sibling repo provisioned",
			"url", url, "ref", ref, "path", target)
	}
}

// ensureSibling makes target exist as a clone of url, serialized on a
// per-target mutex so two goroutines never clone the same path
// simultaneously.
//
// States handled:
//   - absent → shallow clone (`git clone --depth 1`, plus
//     `--branch <ref>` when ref is non-empty);
//   - present with a .git inside → best-effort freshen
//     (`git pull --ff-only --quiet`); a freshen failure logs and keeps
//     the stale copy (success — the corpus is still readable);
//   - present without a .git → warn and leave untouched (never delete).
func ensureSibling(ctx context.Context, logger *slog.Logger, target, url, ref string) error {
	muAny, _ := siblingLocks.LoadOrStore(target, &sync.Mutex{})
	mu := muAny.(*sync.Mutex) // sole stored type; see siblingLocks doc.
	mu.Lock()
	defer mu.Unlock()

	//nolint:gosec // G703: target = session workarea root + SafeRepositoryLeaf-validated basename (no separators, no dot dirs).
	if fi, err := os.Stat(target); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("target exists and is not a directory")
		}
		//nolint:gosec // G703: same validated target path as above.
		if _, gerr := os.Stat(filepath.Join(target, ".git")); gerr != nil {
			logger.Warn("sibling repo exists without .git; leaving untouched",
				"path", target)
			return nil
		}
		if out, perr := runGit(ctx, target, gitIdentity{}, "pull", "--ff-only", "--quiet"); perr != nil {
			logger.Warn("sibling repo freshen failed; using stale copy",
				"path", target, "err", perr, "output", out)
		}
		return nil
	}

	parent := filepath.Dir(target)
	//nolint:gosec // G703: parent is the session workarea root — target is that root joined with a SafeRepositoryLeaf-validated basename, so Dir cannot escape it.
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("mkdir workarea root: %w", err)
	}
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, target)
	if out, err := runGit(ctx, parent, gitIdentity{}, args...); err != nil {
		return fmt.Errorf("git clone: %w (output: %s)", err, out)
	}
	return nil
}
