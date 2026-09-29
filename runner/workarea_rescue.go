package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/internal/gitexec"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Teardown deletes the session's workarea. Before it does, the runner checks
// every git checkout in it for work that exists nowhere else — uncommitted
// changes (tracked edits, deletions, untracked files) and commits no remote
// holds — and archives that work as a patch under the rescue directory. When
// the archive cannot be written, the workarea is KEPT instead of deleted: a
// teardown never destroys unpublished work without first preserving it.
//
// The rescue never touches the checkout: the working state is staged into a
// throw-away index file, so the checkout's own index, branch and files are
// exactly as the agent left them. Paths the backstop would never commit
// (dependency and build output, runner and harness state) do not count as
// work and are left out of the archive. The patch is written with plumbing
// (git diff-tree with every output option pinned), so no user or repository
// diff configuration can alter it, and it is proven before teardown: applied
// to the base in another scratch index it must reproduce the working state
// exactly, or the workarea is kept. The last rescueKeepPerSession archives of
// a session are kept; older ones of the same session are pruned.

// rescueTarget is one git checkout of the session and the commit it was
// provisioned at.
type rescueTarget struct {
	// name labels the checkout in the archive: the declared repository
	// name, or "" for a single-repository workarea.
	name string
	path string
	// base is HEAD right after provisioning; "" when it could not be read.
	base string
}

// rescueGitTimeout bounds the whole rescue of one checkout.
const rescueGitTimeout = 2 * time.Minute

// rescueMaxPatchBytes bounds one archived patch. Beyond it the archive is
// abandoned and the workarea is kept instead.
const rescueMaxPatchBytes = 256 << 20

// rescueResetBatch caps the paths passed to one `git reset` invocation.
const rescueResetBatch = 100

// rescueKeepPerSession is how many archives (one per teardown) a session's
// rescue directory keeps; older ones are pruned after a new one is written.
// Archives of other sessions are never pruned: each holds work that exists
// nowhere else, so the directory is left to the operator rather than capped
// globally.
const rescueKeepPerSession = 5

// rescueArchiveDirRE matches the per-teardown archive directories this file
// creates (a UTC stamp plus a unique suffix); pruning touches nothing else.
var rescueArchiveDirRE = regexp.MustCompile(`^\d{8}T\d{6}Z-`)

// rescuePatchArgs is the git command that writes the patch from base to tree:
// plumbing, recursive, binary-safe, with the external diff driver, textconv,
// color, rename detection and path prefixes all pinned, so user or repository
// configuration (diff.external, diff.noprefix, color.diff=always, textconv
// attributes) cannot change a byte. A variable so a test can prove the
// round-trip check refuses a patch that does not reproduce the work.
var rescuePatchArgs = func(base, tree string) []string {
	return []string{
		"diff-tree", "-r", "-p", "--binary", "--full-index",
		"--no-ext-diff", "--no-textconv", "--no-color", "--no-renames",
		"--src-prefix=a/", "--dst-prefix=b/", base, tree,
	}
}

// rescueRoot is the directory unpublished work is archived under: the
// configured RescueDir, else a "rescue" directory beside the worktree parent
// (for the default layout, a sibling of the worktrees directory in the state
// home).
func (r *Runner) rescueRoot() string {
	if r.rescueDir != "" {
		return r.rescueDir
	}
	return filepath.Join(filepath.Dir(filepath.Clean(r.wt.ParentDir())), "rescue")
}

// recordRescueTargets reads each checkout's HEAD right after provisioning, so
// the rescue can tell the session's own commits from the ones it started on.
func recordRescueTargets(ctx context.Context, res *Result, targets []rescueTarget) {
	for i := range targets {
		headCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if sha, err := captureHeadSHA(headCtx, targets[i].path); err == nil {
			targets[i].base = sha
		}
		cancel()
	}
	res.rescueTargets = targets
}

// preserveUnpublishedWork archives every checkout's unpublished work before
// teardown and logs where it went. It returns false when some checkout's work
// could not be preserved; the caller must then keep the workarea.
func (r *Runner) preserveUnpublishedWork(qw QueuedWork, res *Result) bool {
	if res == nil || len(res.rescueTargets) == 0 {
		return true
	}
	preserved := true
	stamp := r.now().UTC().Format("20060102T150405Z")
	sessionRoot := filepath.Join(r.rescueRoot(), rescueSegment(qw.SessionID))
	archiveDir := ""
	// dirFor creates this teardown's archive directory on first use, unique
	// even when two teardowns of one session share a second.
	dirFor := func() (string, error) {
		if archiveDir != "" {
			return archiveDir, nil
		}
		if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
			return "", fmt.Errorf("create rescue directory: %w", err)
		}
		dir, err := os.MkdirTemp(sessionRoot, stamp+"-")
		if err != nil {
			return "", fmt.Errorf("create rescue directory: %w", err)
		}
		archiveDir = dir
		return dir, nil
	}
	for _, target := range res.rescueTargets {
		archive, err := r.rescueCheckout(qw.SessionID, dirFor, target)
		switch {
		case err != nil:
			preserved = false
			r.logger.Error("could not preserve unpublished work; keeping the workarea instead of tearing it down",
				"sessionId", qw.SessionID,
				"repository", rescueLabel(target.name),
				"path", target.path,
				"err", err,
			)
		case archive.patch != "":
			r.logger.Warn("unpublished work preserved before teardown",
				"sessionId", qw.SessionID,
				"repository", rescueLabel(target.name),
				"patch", archive.patch,
				"metadata", archive.metadata,
				"changedPaths", archive.changedPaths,
				"unpushedCommits", archive.unpushedCommits,
			)
		}
	}
	if archiveDir != "" {
		// Removes the directory only when every checkout turned out to hold
		// nothing but non-work paths.
		_ = os.Remove(archiveDir)
		pruneRescueArchives(r, qw.SessionID, sessionRoot)
	}
	return preserved
}

// pruneRescueArchives keeps the newest rescueKeepPerSession archive
// directories of one session and removes the older ones.
func pruneRescueArchives(r *Runner, sessionID, sessionRoot string) {
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return
	}
	var archives []string
	for _, entry := range entries {
		if entry.IsDir() && rescueArchiveDirRE.MatchString(entry.Name()) {
			archives = append(archives, entry.Name())
		}
	}
	sort.Strings(archives)
	for len(archives) > rescueKeepPerSession {
		old := filepath.Join(sessionRoot, archives[0])
		archives = archives[1:]
		if err := os.RemoveAll(old); err != nil {
			r.logger.Warn("could not prune an old rescue archive", "sessionId", sessionID, "path", old, "err", err)
		}
	}
}

// rescueArchive describes one written archive; patch is "" when the checkout
// held nothing unpublished.
type rescueArchive struct {
	patch           string
	metadata        string
	changedPaths    int
	unpushedCommits int
}

// rescueMetadata is the sidecar written next to an archived patch.
type rescueMetadata struct {
	SessionID       string   `json:"sessionId"`
	Repository      string   `json:"repository"`
	Branch          string   `json:"branch,omitempty"`
	Base            string   `json:"base"`
	Head            string   `json:"head"`
	ChangedPaths    []string `json:"changedPaths,omitempty"`
	UnpushedCommits []string `json:"unpushedCommits,omitempty"`
	CreatedAt       string   `json:"createdAt"`
	Apply           string   `json:"apply"`
}

// rescueCheckout archives one checkout's unpublished work, if it has any,
// into the directory dirFor returns.
func (r *Runner) rescueCheckout(sessionID string, dirFor func() (string, error), target rescueTarget) (rescueArchive, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rescueGitTimeout)
	defer cancel()
	if _, err := os.Stat(target.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rescueArchive{}, nil
		}
		return rescueArchive{}, fmt.Errorf("stat checkout: %w", err)
	}
	head, err := gitStdout(ctx, target.path, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	head = strings.TrimSpace(head)

	status, err := gitStdout(ctx, target.path, nil,
		"-c", "core.quotePath=false", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("git status: %w", err)
	}
	changed := meaningfulPaths(porcelainPaths(status))

	unpushed, err := unpushedCommits(ctx, target)
	if err != nil {
		return rescueArchive{}, err
	}
	if len(changed) == 0 && len(unpushed) == 0 {
		return rescueArchive{}, nil
	}

	// Stage the whole working state into a throw-away index, drop the paths
	// that are not work, and write that state as a tree. The checkout's own
	// index is never touched.
	indexDir, err := os.MkdirTemp("", "rescue-index-")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("create scratch index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index"), "GIT_LITERAL_PATHSPECS=1"}
	if _, err := gitStdout(ctx, target.path, indexEnv, "read-tree", "HEAD"); err != nil {
		return rescueArchive{}, fmt.Errorf("seed scratch index: %w", err)
	}
	if _, err := gitStdout(ctx, target.path, indexEnv, "add", "-A", "--", "."); err != nil {
		return rescueArchive{}, fmt.Errorf("stage working state: %w", err)
	}
	staged, err := gitStdout(ctx, target.path, indexEnv,
		"-c", "core.quotePath=false", "diff", "--cached", "--name-only", "-z", "HEAD")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("list staged working state: %w", err)
	}
	var excluded []string
	for _, path := range strings.Split(staged, "\x00") {
		if path != "" && shouldExcludeFromBackstop(path) {
			excluded = append(excluded, path)
		}
	}
	for i := 0; i < len(excluded); i += rescueResetBatch {
		batch := excluded[i:min(i+rescueResetBatch, len(excluded))]
		if _, err := gitStdout(ctx, target.path, indexEnv, append([]string{"reset", "-q", "HEAD", "--"}, batch...)...); err != nil {
			return rescueArchive{}, fmt.Errorf("drop non-work paths from scratch index: %w", err)
		}
	}
	tree, err := gitStdout(ctx, target.path, indexEnv, "write-tree")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("write working-state tree: %w", err)
	}
	tree = strings.TrimSpace(tree)

	base := target.base
	if base == "" {
		base = head
	}
	dir, err := dirFor()
	if err != nil {
		return rescueArchive{}, err
	}
	label := rescueSegment(rescueLabel(target.name))
	patchPath := filepath.Join(dir, label+".patch")
	written, err := writeRescuePatch(ctx, target.path, patchPath, base, tree)
	if err != nil {
		return rescueArchive{}, err
	}
	if written == 0 {
		// Everything that differed was non-work; nothing to keep.
		_ = os.Remove(patchPath)
		return rescueArchive{}, nil
	}
	if err := verifyRescuePatch(ctx, target.path, patchPath, base, tree); err != nil {
		return rescueArchive{}, err
	}

	branch, _ := gitStdout(ctx, target.path, nil, "branch", "--show-current")
	meta := rescueMetadata{
		SessionID:       sessionID,
		Repository:      rescueLabel(target.name),
		Branch:          strings.TrimSpace(branch),
		Base:            base,
		Head:            head,
		ChangedPaths:    changed,
		UnpushedCommits: unpushed,
		CreatedAt:       r.now().UTC().Format(time.RFC3339),
		Apply:           "git checkout " + base + " && git apply " + label + ".patch",
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return rescueArchive{}, fmt.Errorf("encode rescue metadata: %w", err)
	}
	metaPath := filepath.Join(dir, label+".json")
	if err := os.WriteFile(metaPath, append(body, '\n'), 0o600); err != nil {
		return rescueArchive{}, fmt.Errorf("write rescue metadata: %w", err)
	}
	return rescueArchive{patch: patchPath, metadata: metaPath, changedPaths: len(changed), unpushedCommits: len(unpushed)}, nil
}

// unpushedCommits lists the commits on HEAD that the checkout started without
// and no remote-tracking ref holds, newest first. With no recorded base and no
// remote-tracking refs there is nothing to measure against, so none are
// reported (the working-state patch still carries every uncommitted change).
func unpushedCommits(ctx context.Context, target rescueTarget) ([]string, error) {
	args := []string{"-c", "log.showSignature=false", "log", "--no-color", "--format=%H %s", "HEAD", "--not", "--remotes"}
	if target.base != "" {
		args = append(args, target.base)
	} else {
		remotes, err := gitStdout(ctx, target.path, nil, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes")
		if err != nil {
			return nil, fmt.Errorf("list remote-tracking refs: %w", err)
		}
		if strings.TrimSpace(remotes) == "" {
			return nil, nil
		}
	}
	out, err := gitStdout(ctx, target.path, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("list unpushed commits: %w", err)
	}
	return filterEmpty(strings.Split(strings.TrimSpace(out), "\n")), nil
}

// writeRescuePatch writes the rescuePatchArgs patch from base to tree to path,
// refusing a patch larger than rescueMaxPatchBytes. It returns the bytes
// written.
func writeRescuePatch(ctx context.Context, dir, path, base, tree string) (int64, error) {
	//nolint:gosec // G304: path is runner-owned, built from sanitized segments.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create rescue patch: %w", err)
	}
	capped := &cappedWriter{w: f, limit: rescueMaxPatchBytes}
	diffErr := gitStdoutTo(ctx, dir, capped, rescuePatchArgs(base, tree)...)
	closeErr := f.Close()
	switch {
	case capped.exceeded:
		_ = os.Remove(path)
		return 0, fmt.Errorf("rescue patch exceeds %d bytes", rescueMaxPatchBytes)
	case diffErr != nil:
		_ = os.Remove(path)
		return 0, fmt.Errorf("write rescue patch: %w", diffErr)
	case closeErr != nil:
		return 0, fmt.Errorf("close rescue patch: %w", closeErr)
	}
	return capped.written, nil
}

// verifyRescuePatch proves the archived patch reproduces the working state:
// applied to base in a scratch index (never the checkout's own), it must
// yield exactly tree. A patch that does not is no preservation, so the caller
// keeps the workarea instead of reporting success.
func verifyRescuePatch(ctx context.Context, dir, patchPath, base, tree string) error {
	indexDir, err := os.MkdirTemp("", "rescue-verify-")
	if err != nil {
		return fmt.Errorf("create scratch index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index")}
	if _, err := gitStdout(ctx, dir, env, "read-tree", base); err != nil {
		return fmt.Errorf("verify rescue patch: seed base: %w", err)
	}
	if _, err := gitStdout(ctx, dir, env, "apply", "--cached", "--whitespace=nowarn", patchPath); err != nil {
		return fmt.Errorf("verify rescue patch: it does not apply to its base: %w", err)
	}
	got, err := gitStdout(ctx, dir, env, "write-tree")
	if err != nil {
		return fmt.Errorf("verify rescue patch: write result: %w", err)
	}
	if strings.TrimSpace(got) != tree {
		return fmt.Errorf("verify rescue patch: applied to its base it yields %s, not the working state %s", strings.TrimSpace(got), tree)
	}
	return nil
}

// cappedWriter forwards writes until limit bytes, then fails every write.
type cappedWriter struct {
	w        io.Writer
	limit    int64
	written  int64
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.written+int64(len(p)) > c.limit {
		c.exceeded = true
		return 0, errors.New("rescue patch size limit reached")
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// porcelainPaths returns the paths of `git status --porcelain=v1 -z` output.
// A rename or copy entry is followed by its original path, which is skipped.
func porcelainPaths(out string) []string {
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, entry[3:])
		if entry[0] == 'R' || entry[0] == 'C' {
			i++
		}
	}
	return paths
}

// meaningfulPaths drops the paths the backstop would never commit.
func meaningfulPaths(paths []string) []string {
	var out []string
	for _, path := range paths {
		if !shouldExcludeFromBackstop(strings.TrimSuffix(path, "/")) {
			out = append(out, path)
		}
	}
	return out
}

// rescueLabel names a checkout in logs and archive file names.
func rescueLabel(name string) string {
	if name == "" {
		return "repository"
	}
	return name
}

var rescueSegmentRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// rescueSegment makes s safe as one path segment.
func rescueSegment(s string) string {
	s = rescueSegmentRE.ReplaceAllString(s, "_")
	if s == "" || s == "." || s == ".." {
		return "_"
	}
	return s
}

// gitStdout runs one git command in dir and returns its stdout; the error
// carries stderr.
func gitStdout(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := gitExec(ctx, dir, extraEnv, &stdout, args...); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// gitStdoutTo runs one git command in dir, streaming its stdout to w.
func gitStdoutTo(ctx context.Context, dir string, w io.Writer, args ...string) error {
	return gitExec(ctx, dir, nil, w, args...)
}

func gitExec(ctx context.Context, dir string, extraEnv []string, stdout io.Writer, args ...string) error {
	//nolint:gosec // G204: args come from runner-controlled call sites.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitexec.HardenedEnv(runtimeenv.FilterRunnerOnly(append(os.Environ(), extraEnv...)), false, gitexec.Auth{})
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, firstLine(stderr.String()))
	}
	return nil
}
