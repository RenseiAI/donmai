package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// siblingReposEnv is the work-item env key naming the read-only context
// repositories the runner materializes next to the session worktree
// (ADR-2026-07-07-sibling-context-repos). Value: comma-separated
// entries, each `<git-url>` or `<git-url>#<ref>`.
const siblingReposEnv = "DONMAI_SIBLING_REPOS"

// siblingLocks serializes provisioning per target directory. Concurrent
// sessions can share a parent directory (the worktree root), so two
// runners racing on the same sibling must not clone into the same path
// simultaneously. Keys are cleaned absolute target paths; values are
// *sync.Mutex. Entries are never removed — the set of sibling repos a
// host materializes is small and stable across a process lifetime.
var siblingLocks sync.Map

// provisionSiblings materializes the read-only context repos named by
// DONMAI_SIBLING_REPOS as siblings of the session worktree, so agents
// find their governing corpus at ../<name> exactly as repo AGENTS.md
// contracts promise.
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
func (r *Runner) provisionSiblings(ctx context.Context, qw QueuedWork, wpath string) {
	_ = qw // no per-session env field on QueuedWork today; see doc comment.
	spec := strings.TrimSpace(os.Getenv(siblingReposEnv))
	if spec == "" {
		return
	}
	provisionSiblingRepos(ctx, r.logger, spec, wpath)
}

// provisionSiblingRepos clones or freshens each entry of spec into a
// directory sibling to wpath. Pure worker for provisionSiblings; split
// out so tests can drive it without touching the process env.
func provisionSiblingRepos(ctx context.Context, logger *slog.Logger, spec, wpath string) {
	parent := filepath.Dir(wpath)
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		url, ref, _ := strings.Cut(entry, "#")
		name := siblingDirName(url)
		if !safeSiblingName(name) {
			logger.Warn("sibling repo skipped: unsafe directory name",
				"entry", entry, "name", name)
			continue
		}
		target := filepath.Join(parent, name)
		if target == filepath.Clean(wpath) {
			logger.Warn("sibling repo skipped: target collides with session worktree",
				"entry", entry, "path", target)
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

// siblingDirName derives the sibling directory name from a git URL: the
// URL path basename with a trailing ".git" stripped (e.g.
// "https://example.com/org/docs-corpus.git" → "docs-corpus").
func siblingDirName(url string) string {
	base := path.Base(strings.TrimRight(strings.TrimSpace(url), "/"))
	return strings.TrimSuffix(base, ".git")
}

// safeSiblingName rejects names that would escape or clobber the parent
// directory: empty, dot dirs, or anything carrying a path separator.
func safeSiblingName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// ensureSibling makes target exist as a clone of url, serialized on a
// per-target mutex so concurrent sessions sharing a parent directory
// never clone the same sibling simultaneously.
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

	//nolint:gosec // G703: target = worktree parent + safeSiblingName-validated basename (no separators, no dot dirs).
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

	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, target)
	if out, err := runGit(ctx, filepath.Dir(target), gitIdentity{}, args...); err != nil {
		return fmt.Errorf("git clone: %w (output: %s)", err, out)
	}
	return nil
}
