package codeintelhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/internal/credentials"
	"github.com/RenseiAI/donmai/internal/gitexec"
	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// GitFactory is the production Factory: it maintains one persistent bare
// mirror per catalog repository under StateDir/mirrors/ and one detached
// worktree per binding under StateDir/workareas/, verifying the exact
// revision object with git before constructing an mcpserver.Server rooted
// at the worktree. Both directories, and the .donmai/code-index each
// mcpserver.Server builds inside its worktree, persist across process
// restarts.
//
// ensureMirror + ensureRevision for a given catalog repository are
// serialized end-to-end via a per-repository mutex (see lockFor): two
// concurrent warms for different revisions of the SAME repository must
// never clone/fetch the same bare mirror at the same time, but warms for
// DIFFERENT repositories proceed fully in parallel.
type GitFactory struct {
	// Catalog resolves a binding's repositoryPathId to its clone source and
	// credential configuration. Required.
	Catalog *Catalog
	// StateDir is the absolute root directory under which mirrors/ and
	// workareas/ are created. Required.
	StateDir string
	// Tools, when non-empty, narrows the six-tool profile each workarea's
	// mcpserver.Server exposes. Empty means all six.
	Tools []string
	// Logf receives lifecycle/diagnostic log lines; nil discards them. It is
	// also threaded into each workarea's mcpserver.Config.Logf.
	Logf func(format string, args ...any)
	// GitAuth resolves per-invocation authentication for clone and fetch
	// operations. Nil preserves the static CatalogGit-only behavior.
	GitAuth GitAuth

	mirrorLocksMu sync.Mutex
	mirrorLocks   map[string]*sync.Mutex
}

var _ Factory = (*GitFactory)(nil)

// GitAuth resolves authorization for a remote Git operation. authHeader is
// injected as an in-memory http.extraHeader; suppressHelper disables configured
// OS credential helpers for this invocation. Returning an error aborts the Git
// operation. Returning ("", false, nil) leaves the static catalogue Git config
// as the only credential source.
type GitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

func (f *GitFactory) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// Create implements Factory. It resolves and fences the binding's
// repository, ensures a bare mirror exists and contains the exact revision
// object, ensures a detached worktree checked out at that revision exists,
// and constructs the workarea's code-intelligence server on top of it.
func (f *GitFactory) Create(ctx context.Context, binding Binding) (ToolCaller, io.Closer, error) {
	repo, err := f.Catalog.Lookup(binding.RepositoryPathID)
	if err != nil {
		return nil, nil, err
	}
	if repo.ProjectID != binding.ProjectID {
		return nil, nil, fmt.Errorf("%w: repository %q belongs to project %q, not %q",
			ErrProjectMismatch, binding.RepositoryPathID, repo.ProjectID, binding.ProjectID)
	}

	mirrorDir := filepath.Join(f.StateDir, "mirrors", mirrorDirName(repo.RepositoryPathID))

	// Serialize ensureMirror+ensureRevision per repository: both mutate the
	// same bare mirror directory, and git does not support two writers
	// (clone/fetch) racing against it. Different repositories use different
	// mutexes and proceed in parallel.
	mirrorMu := f.lockFor(repo.RepositoryPathID)
	mirrorMu.Lock()
	err = f.ensureMirror(ctx, repo, mirrorDir)
	if err == nil {
		err = f.ensureRevision(ctx, repo, mirrorDir, binding.Revision)
	}
	mirrorMu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	workDir := filepath.Join(f.StateDir, "workareas", binding.Fingerprint())
	if err := f.ensureWorktree(ctx, mirrorDir, workDir, binding.Revision); err != nil {
		return nil, nil, err
	}

	srv, err := mcpserver.New(mcpserver.Config{
		Root:  workDir,
		Tools: f.Tools,
		Logf:  f.Logf,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("construct code-intel server for workarea %s: %w", workDir, err)
	}
	return srv, &worktreeCloser{mirrorDir: mirrorDir, workDir: workDir, logf: f.Logf}, nil
}

// ensureMirror creates repo's persistent bare mirror at mirrorDir if it does
// not already exist. An existing directory has its origin provenance
// re-verified against repo.Source on every call (verifyMirrorOrigin) rather
// than being trusted blindly — the mirror path is a SHA-256 digest of the
// repository path ID, and an operator edit to daemon.yaml's source (or a
// corrupted/foreign on-disk state) must fail closed instead of silently
// serving the wrong repository's content. ensureRevision performs the
// fetch-on-miss that keeps an existing, provenance-verified mirror current.
func (f *GitFactory) ensureMirror(ctx context.Context, repo CatalogRepository, mirrorDir string) error {
	if info, err := os.Stat(mirrorDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("mirror path %s exists and is not a directory", mirrorDir)
		}
		return f.verifyMirrorOrigin(ctx, repo, mirrorDir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat mirror dir %s: %w", mirrorDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o750); err != nil {
		return fmt.Errorf("create mirror parent dir: %w", err)
	}
	networkEnv, err := f.networkGitEnv(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := runGit(ctx, networkEnv, "clone", "--mirror", "--", repo.Source, mirrorDir); err != nil {
		return fmt.Errorf("clone mirror for repository %s: %w", repo.RepositoryPathID, err)
	}
	return nil
}

// verifyMirrorOrigin fails closed (ErrMirrorOriginMismatch) when mirrorDir's
// recorded origin remote does not exactly equal repo.Source. Neither URL is
// echoed in the returned error: an operator-configured source or a
// `git remote get-url` result may itself carry a credential (defense in
// depth alongside NewCatalog's own rejection of embedded http(s) userinfo).
func (f *GitFactory) verifyMirrorOrigin(ctx context.Context, repo CatalogRepository, mirrorDir string) error {
	out, err := runGit(ctx, buildLocalGitEnv(), "--git-dir", mirrorDir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%w: repository %q: read existing mirror origin: %v", ErrMirrorOriginMismatch, repo.RepositoryPathID, err)
	}
	if strings.TrimSpace(string(out)) != repo.Source {
		return fmt.Errorf("%w: repository %q", ErrMirrorOriginMismatch, repo.RepositoryPathID)
	}
	return nil
}

// ensureRevision verifies that revision resolves to an exact commit object
// in mirrorDir, fetching once on a miss before failing with
// ErrRevisionUnavailable. It never falls back to a branch or HEAD.
func (f *GitFactory) ensureRevision(ctx context.Context, repo CatalogRepository, mirrorDir, revision string) error {
	if f.revisionExists(ctx, mirrorDir, revision) {
		return nil
	}
	networkEnv, err := f.networkGitEnv(ctx, repo)
	if err != nil {
		return err
	}
	if _, err := runGit(ctx, networkEnv, "--git-dir", mirrorDir, "fetch", "--prune", "--", "origin", "+refs/*:refs/*"); err != nil {
		return fmt.Errorf("fetch repository %s: %w", repo.RepositoryPathID, err)
	}
	if !f.revisionExists(ctx, mirrorDir, revision) {
		return fmt.Errorf("%w: revision %q not found in repository %q", ErrRevisionUnavailable, revision, repo.RepositoryPathID)
	}
	return nil
}

func (f *GitFactory) revisionExists(ctx context.Context, mirrorDir, revision string) bool {
	_, err := runGit(ctx, buildLocalGitEnv(), "--git-dir", mirrorDir, "cat-file", "-e", revision+"^{commit}")
	return err == nil
}

// ensureWorktree creates a detached worktree at workDir checked out at the
// exact revision, reusing an existing one in place when it is already at
// that revision (the restart-safe path: the persistent volume may already
// hold this workarea from a prior process lifetime) and otherwise removing
// and recreating it.
func (f *GitFactory) ensureWorktree(ctx context.Context, mirrorDir, workDir, revision string) error {
	if info, err := os.Stat(workDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("workarea path %s exists and is not a directory", workDir)
		}
		if head, herr := runGit(ctx, buildLocalGitEnv(), "-C", workDir, "rev-parse", "HEAD"); herr == nil &&
			strings.TrimSpace(string(head)) == revision {
			return nil
		}
		if err := f.removeWorktree(ctx, mirrorDir, workDir); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat workarea dir %s: %w", workDir, err)
	}

	if err := os.MkdirAll(filepath.Dir(workDir), 0o750); err != nil {
		return fmt.Errorf("create workarea parent dir: %w", err)
	}
	if _, err := runGit(ctx, buildLocalGitEnv(), "-C", mirrorDir, "worktree", "add", "--detach", "--", workDir, revision); err != nil {
		return fmt.Errorf("create detached worktree at %s: %w", workDir, err)
	}
	return nil
}

// removeWorktree removes a stale worktree via `git worktree remove --force`,
// falling back to a plain directory removal (mirroring
// runtime/worktree.Manager's teardown pattern) so a corrupted worktree
// admin record can never block eviction/recreation.
func (f *GitFactory) removeWorktree(ctx context.Context, mirrorDir, workDir string) error {
	if _, err := runGit(ctx, buildLocalGitEnv(), "-C", mirrorDir, "worktree", "remove", "--force", workDir); err != nil {
		f.logf("git worktree remove failed for %s (falling back to rm -rf): %v", workDir, err)
	}
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("remove stale workarea %s: %w", workDir, err)
	}
	_, _ = runGit(ctx, buildLocalGitEnv(), "-C", mirrorDir, "worktree", "prune")
	return nil
}

// worktreeCloser implements io.Closer for one Factory-provisioned workarea.
// Pool.pickVictimLocked/ReapIdle call Close only on an unleased, non-warming
// entry chosen for eviction — never on a currently-leased workarea.
type worktreeCloser struct {
	mirrorDir string
	workDir   string
	logf      func(format string, args ...any)
}

func (c *worktreeCloser) Close() error {
	log := c.logf
	if log == nil {
		log = func(string, ...any) {}
	}
	ctx := context.Background()
	if _, err := runGit(ctx, buildLocalGitEnv(), "-C", c.mirrorDir, "worktree", "remove", "--force", c.workDir); err != nil {
		log("evict workarea %s: git worktree remove failed (falling back to rm -rf): %v", c.workDir, err)
	}
	if err := os.RemoveAll(c.workDir); err != nil {
		return fmt.Errorf("remove workarea %s: %w", c.workDir, err)
	}
	_, _ = runGit(ctx, buildLocalGitEnv(), "-C", c.mirrorDir, "worktree", "prune")
	return nil
}

// runGit runs git with an explicit argv (never a shell) and an explicitly
// constructed environment, tied to ctx so a caller's cancellation/timeout
// terminates the subprocess.
//
// from validated catalog/binding fields, never interpolated shell text.
//
//nolint:gosec // G204: name is the hard-coded "git" binary; args are built
func runGit(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Deliberately exclude args and CombinedOutput from this error: args
		// may include repo.Source (an operator-configured clone URL) and
		// CombinedOutput may echo it back (e.g. git's own "fatal: unable to
		// access '<url-with-credentials>'" diagnostics) or reveal other
		// operational details. err's own Error() text (e.g. *exec.ExitError's
		// "exit status N") carries no such content. Callers that need stable,
		// non-secret context wrap this error with the repository path ID/operation
		// name, never with args or out.
		return out, fmt.Errorf("git command failed: %w", err)
	}
	return out, nil
}

// buildLocalGitEnv returns the environment for a git invocation that never
// touches a remote (worktree add/remove/prune, cat-file, rev-parse): the
// non-interactive baseline only, no credential configuration, and with the
// daemon's own auth surface (including CODE_INTEL_HOST_JWT_SECRET) filtered
// out of the child process environment.
func buildLocalGitEnv() []string {
	base := credentials.Filter(os.Environ())
	return gitexec.HardenedEnv(base, false, "")
}

// networkGitEnv resolves the GitAuth callback immediately before a remote Git
// operation, so short-lived credentials are never retained between clone/fetch
// calls. The callback's header is passed to git through GIT_CONFIG_VALUE_n, not
// argv or a persisted Git config.
func (f *GitFactory) networkGitEnv(ctx context.Context, repo CatalogRepository) ([]string, error) {
	var authHeader string
	var suppressHelper bool
	if f.GitAuth != nil {
		var err error
		authHeader, suppressHelper, err = f.GitAuth(ctx, repo.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve git auth for repository %q: %w", repo.RepositoryPathID, err)
		}
	}
	return buildNetworkGitEnv(repo, suppressHelper, authHeader), nil
}

// buildNetworkGitEnv returns the environment for a git invocation that
// touches repo's remote (clone --mirror, fetch): the non-interactive
// baseline plus the operator-configured credential helper and/or SSH key
// for this specific repository and a per-invocation auth header. All values
// are injected via GIT_CONFIG_COUNT/KEY/VALUE, never argv or .git/config,
// with the host's own auth surface filtered out of the child environment
// exactly as buildLocalGitEnv does.
func buildNetworkGitEnv(repo CatalogRepository, suppressHelper bool, authHeader string) []string {
	base := credentials.Filter(os.Environ())
	var pairs [][2]string
	if repo.Git != nil {
		if repo.Git.CredentialHelper != "" {
			pairs = append(pairs, [2]string{"credential.helper", repo.Git.CredentialHelper})
		}
		if repo.Git.SSHKey != "" {
			pairs = append(pairs, [2]string{"core.sshCommand", "ssh -i " + repo.Git.SSHKey + " -o IdentitiesOnly=yes"})
		}
	}
	// Pre-seed our own config pairs and let gitexec.HardenedEnv continue
	// numbering from there — this is the composition contract its package
	// doc describes for callers that pre-seed GIT_CONFIG_* keys.
	seeded := appendGitConfigPairs(base, pairs...)
	return gitexec.HardenedEnv(seeded, suppressHelper, authHeader)
}

// appendGitConfigPairs appends pairs as GIT_CONFIG_KEY_n/VALUE_n entries
// starting after any GIT_CONFIG_COUNT already present in env, and updates
// GIT_CONFIG_COUNT to the new total. A nil/empty pairs list returns env
// unchanged.
func appendGitConfigPairs(env []string, pairs ...[2]string) []string {
	if len(pairs) == 0 {
		return env
	}
	start := gitConfigCount(env)
	out := make([]string, len(env), len(env)+2*len(pairs)+1)
	copy(out, env)
	for i, p := range pairs {
		n := start + i
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, p[1]))
	}
	out = append(out, fmt.Sprintf("GIT_CONFIG_COUNT=%d", start+len(pairs)))
	return out
}

// gitConfigCount returns the value of the LAST GIT_CONFIG_COUNT assignment
// in env, or 0 when absent/unparseable — the same last-assignment-wins
// numbering contract internal/gitexec.HardenedEnv documents for composing
// callers (its own copy of this scan is unexported).
func gitConfigCount(env []string) int {
	const prefix = "GIT_CONFIG_COUNT="
	count := 0
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(e, prefix)))
		if err != nil || n < 0 {
			count = 0
			continue
		}
		count = n
	}
	return count
}

// mirrorDirName returns a collision-resistant, filesystem-safe directory
// component for a repository path ID: the SHA-256 hex digest of the raw ID.
// Two distinct path IDs can never sanitize/truncate down to the same directory
// name (the failure mode a character-substituting sanitizer such as the
// former sanitizeID could hit — e.g. "a/b" and "a_b" both sanitizing to
// "a_b"), which would otherwise let two different repositories share, and
// corrupt, one bare mirror.
func mirrorDirName(repositoryPathID string) string {
	sum := sha256.Sum256([]byte(repositoryPathID))
	return hex.EncodeToString(sum[:])
}

// lockFor returns the mutex serializing ensureMirror+ensureRevision for a
// repository path ID, creating it on first use. Distinct repositories use
// distinct mutexes, so warms for different repositories never block each
// other; concurrent warms for the SAME repository (e.g. two different
// revisions requested at once) are serialized end-to-end against the one
// bare mirror they share.
func (f *GitFactory) lockFor(repositoryPathID string) *sync.Mutex {
	f.mirrorLocksMu.Lock()
	defer f.mirrorLocksMu.Unlock()
	if f.mirrorLocks == nil {
		f.mirrorLocks = make(map[string]*sync.Mutex)
	}
	l, ok := f.mirrorLocks[repositoryPathID]
	if !ok {
		l = &sync.Mutex{}
		f.mirrorLocks[repositoryPathID] = l
	}
	return l
}
