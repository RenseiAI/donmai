package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// Sibling context repositories (ADR-2026-07-07-sibling-context-repos, as
// amended by ADR-2026-08-22-session-owned-multi-repository-workarea).
//
// DONMAI_SIBLING_REPOS names read-only context repositories an agent expects
// at ../<name> beside its repository: comma-separated entries, each
// `<git-url>` or `<git-url>#<ref>`, carried on the work item's env (process
// env as the fallback for standalone runs). The wire is unchanged; where an
// entry lands depends on the executor:
//
//   - An executor that attests the session-root-v1 workarea protocol and
//     isolated-read-only-v1 enforcement gets each entry as a declared
//     read-only `context` leaf under the session root (siblingContextDeclaration),
//     beside the selected repository, so ../<name> still resolves. The leaf is
//     owned by the session: cleanup, archive and restart adoption reach it,
//     and a failed clone is skipped with a warning like any declared context
//     repository.
//   - Any other executor cannot hold a declared read-only leaf read-only
//     (ADR-2026-08-22 D6.6, D8.3), so it keeps the original placement
//     (D8.6): a shallow clone into the directory beside the session worktree,
//     freshened best-effort when present (provisionSiblings).
//
// A work item that carries a repository declaration of its own is
// authoritative: the variable is ignored for it, with a warning.
//
// Either way a sibling failure is never fatal to the session.

const siblingReposEnv = "DONMAI_SIBLING_REPOS"

// siblingProvisionTimeout bounds one legacy sibling entry: waiting for the
// target's lock, then cloning or freshening it.
const siblingProvisionTimeout = 5 * time.Minute

// siblingLockPollInterval spaces attempts to take a sibling target's lock.
const siblingLockPollInterval = 100 * time.Millisecond

// siblingCloneHook, when set (tests only), runs inside the target's lock just
// before a legacy sibling clone.
var siblingCloneHook func()

// siblingEntry is one parsed DONMAI_SIBLING_REPOS entry.
type siblingEntry struct {
	url  string
	ref  string
	name string
}

// siblingReposSpec returns the work item's DONMAI_SIBLING_REPOS value, or the
// process env's when the work item carries none.
func siblingReposSpec(qw QueuedWork) string {
	if spec := strings.TrimSpace(qw.Env[siblingReposEnv]); spec != "" {
		return spec
	}
	return strings.TrimSpace(os.Getenv(siblingReposEnv))
}

// parseSiblingEntries splits a DONMAI_SIBLING_REPOS value into entries. An
// entry whose directory name is unsafe (empty, ".", "..", or containing a path
// separator) is rejected.
func parseSiblingEntries(spec string) (entries []siblingEntry, rejected []string) {
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		url, ref, _ := strings.Cut(raw, "#")
		name := siblingDirName(url)
		if !safeSiblingName(name) {
			rejected = append(rejected, raw)
			continue
		}
		entries = append(entries, siblingEntry{url: url, ref: ref, name: name})
	}
	return entries, rejected
}

// siblingDirName derives the directory name from the URL basename with any
// trailing ".git" stripped.
func siblingDirName(url string) string {
	base := path.Base(strings.TrimRight(strings.TrimSpace(url), "/"))
	return strings.TrimSuffix(base, ".git")
}

// safeSiblingName rejects names that would escape or alias the parent.
func safeSiblingName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// siblingContextEligible reports whether qw is a plain single-repository work
// item whose DONMAI_SIBLING_REPOS entries may become declared context leaves:
// it declares no repositories itself, has a repository, and uses none of the
// shapes whose provisioning a declaration would change (a new branch from a
// base ref, a dispatched pull request, a shared or seeded workarea, an
// interactive session).
func siblingContextEligible(qw QueuedWork) bool {
	return qw.RepositoryDeclaration == nil && strings.TrimSpace(qw.Repository) != "" &&
		worktreeProvisionStrategy(qw) != worktree.StrategyEmpty &&
		qw.BaseRef == "" && qw.PullRequest == nil && qw.CacheSeedID == "" &&
		qw.WorkareaMode != worktree.ModeShared && qw.Mode == "" &&
		siblingReposSpec(qw) != ""
}

// executorAttestsSiblingContext reports whether provider can hold a declared
// read-only context leaf: it attests the session-root-v1 workarea protocol and
// isolated-read-only-v1 enforcement.
func executorAttestsSiblingContext(provider agent.Provider) bool {
	harness, ok := provider.(agent.HarnessProvider)
	if !ok {
		return false
	}
	caps := harness.Manifest().Caps
	if workarea.RepositoryAuthorityEnforcement(caps.RepositoryAuthorityEnforcement) != workarea.RepositoryAuthorityIsolatedReadOnlyV1 {
		return false
	}
	for _, protocol := range caps.MultiRepositoryWorkareaProtocols {
		if workarea.Protocol(protocol) == workarea.ProtocolSessionRootV1 {
			return true
		}
	}
	return false
}

// siblingContextDeclaration is the repository declaration a plain work item's
// DONMAI_SIBLING_REPOS entries describe: the work item's repository as the
// mutable primary, and each entry as a read-only context repository named
// after its directory. An entry whose name is not a valid leaf, or repeats the
// primary's or an earlier entry's, is left out. Nil when no entry remains.
func siblingContextDeclaration(qw QueuedWork) *workarea.RepositoryDeclarationV1 {
	entries, _ := parseSiblingEntries(siblingReposSpec(qw))
	primaryLeaf, err := workarea.RepositoryLeaf(qw.Repository)
	if err != nil {
		return nil
	}
	declaration := &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: qw.Repository, Ref: qw.Ref},
			Role:   workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}
	seen := map[string]struct{}{strings.ToLower(primaryLeaf): {}}
	for _, entry := range entries {
		key := strings.ToLower(entry.name)
		if _, duplicate := seen[key]; duplicate || workarea.ValidateRepositoryLeaf(entry.name) != nil {
			continue
		}
		seen[key] = struct{}{}
		declaration.Repositories = append(declaration.Repositories, workarea.DeclaredRepositoryV1{
			Source: workarea.RepositorySource{Repository: entry.url, Ref: entry.ref},
			Name:   entry.name, Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly,
		})
	}
	if len(declaration.Repositories) == 1 {
		return nil
	}
	return declaration
}

// provisionSiblings is the original placement, for an executor that cannot
// hold a declared read-only leaf: each entry is cloned into the directory
// beside the session worktree.
func (r *Runner) provisionSiblings(ctx context.Context, qw QueuedWork, wpath string) {
	spec := siblingReposSpec(qw)
	if spec == "" {
		return
	}
	provisionSiblingRepos(ctx, r.logger, spec, wpath)
}

func provisionSiblingRepos(ctx context.Context, logger *slog.Logger, spec, wpath string) {
	parent := filepath.Dir(wpath)
	entries, rejected := parseSiblingEntries(spec)
	for _, entry := range rejected {
		logger.Warn("sibling repo skipped: unsafe directory name", "entry", entry)
	}
	for _, entry := range entries {
		target := filepath.Join(parent, entry.name)
		if target == filepath.Clean(wpath) {
			logger.Warn("sibling repo skipped: target collides with session worktree",
				"url", entry.url, "path", target)
			continue
		}
		entryCtx, cancel := context.WithTimeout(ctx, siblingProvisionTimeout)
		err := ensureSibling(entryCtx, logger, target, entry.url, entry.ref)
		cancel()
		if err != nil {
			logger.Warn("sibling repo provision failed (non-fatal)",
				"url", entry.url, "ref", entry.ref, "path", target, "err", err)
			continue
		}
		logger.Info("sibling repo provisioned",
			"url", entry.url, "ref", entry.ref, "path", target)
	}
}

// ensureSibling clones url into target, or freshens an existing clone
// best-effort, holding target's lock across processes: concurrent sessions
// share the parent directory, and each session runs as its own process.
func ensureSibling(ctx context.Context, logger *slog.Logger, target, url, ref string) error {
	unlock, err := lockSiblingTarget(ctx, target)
	if err != nil {
		return err
	}
	defer unlock()

	if fi, err := os.Stat(target); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("target exists and is not a directory")
		}
		if _, gerr := os.Stat(filepath.Join(target, ".git")); gerr != nil {
			logger.Warn("sibling repo exists without .git; leaving untouched", "path", target)
			return nil
		}
		if out, perr := runGit(ctx, target, gitIdentity{}, "pull", "--ff-only", "--quiet"); perr != nil {
			logger.Warn("sibling repo freshen failed; using stale copy",
				"path", target, "err", perr, "output", out)
		}
		return nil
	}

	if siblingCloneHook != nil {
		siblingCloneHook()
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

// lockSiblingTarget takes an exclusive advisory lock on target's lock file
// (".<name>.sibling-lock" beside it), waiting until ctx ends. The lock holds
// across processes; the returned function releases it.
func lockSiblingTarget(ctx context.Context, target string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".sibling-lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lock file beside a runner-chosen sibling target
	if err != nil {
		return nil, fmt.Errorf("open sibling lock: %w", err)
	}
	fd := file.Fd()
	if fd > uintptr(math.MaxInt) {
		_ = file.Close()
		return nil, fmt.Errorf("open sibling lock: %w", syscall.EBADF)
	}
	for {
		err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(fd), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock sibling target: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock sibling target: %w", ctx.Err())
		case <-time.After(siblingLockPollInterval):
		}
	}
}
