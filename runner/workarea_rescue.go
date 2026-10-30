package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/internal/gitexec"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
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

// rescueKeepPerSession is how many archive attempts (one directory per
// teardown) a session's rescue directory keeps; older ones are pruned
// after a new one is written. Patch-bearing and evidence-only attempts
// share this boundary: every directory matching rescueArchiveDirRE under
// one session's root counts, whether it holds a patch, terminal evidence,
// or both. Archives of other sessions are never pruned: each holds work
// that exists nowhere else, so the directory is left to the operator
// rather than capped globally.
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

// reseedRescueBasesFromDeclaration repairs the rescue base for a re-entered
// session-root generation. Provision records HEAD as the base, but on
// re-entry HEAD already holds the earlier attempt's commits: a patch and
// unpushed-commit listing measured from it would miss work committed
// before the re-entry, and a failed session's teardown would then delete
// it. When the worktree manager reports the session re-entered an
// existing generation, each named checkout's base is reseeded from the
// generation's durable declaration record (the resolved ref each
// repository was provisioned at), verified to resolve in the checkout;
// anything unverifiable keeps the HEAD-measured base. Fresh generations
// and repository-free sessions are untouched, as is every checkout the
// record does not name.
func reseedRescueBasesFromDeclaration(ctx context.Context, res *Result, workareaRoot string, wt *worktree.Manager, sessionID string, logger *slog.Logger) {
	if res == nil || len(res.rescueTargets) == 0 || wt == nil || workareaRoot == "" {
		return
	}
	provisioned, err := wt.Result(sessionID)
	if err != nil || !provisioned.Reentered {
		return
	}
	declaration, err := workarea.ReadDeclaration(workarea.RootPath(workareaRoot))
	if err != nil {
		logger.Debug("re-entered session keeps HEAD-measured rescue base: declaration unreadable", "sessionId", sessionID, "err", err)
		return
	}
	resolved := make(map[string]string, len(declaration.Repositories))
	for _, repository := range declaration.Repositories {
		if repository.ResolvedRef != "" {
			resolved[repository.Name] = repository.ResolvedRef
		}
	}
	for i, target := range res.rescueTargets {
		want, ok := resolved[target.name]
		if !ok || want == target.base || target.name == "" {
			continue
		}
		// The resolved ref must name a commit the checkout holds;
		// otherwise the patch cannot be cut from it and the
		// HEAD-measured base stands.
		checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := gitStdout(checkCtx, target.path, nil, "rev-parse", "--verify", want+"^{commit}"); err != nil {
			checkCancel()
			logger.Debug("re-entered session keeps HEAD-measured rescue base: resolved ref unresolvable", "sessionId", sessionID, "repository", target.name, "err", err)
			continue
		}
		checkCancel()
		res.rescueTargets[i].base = want
		logger.Info("re-entered session reseeds rescue base from declared resolved ref", "sessionId", sessionID, "repository", target.name)
	}
}

// shouldPreserveTerminalEvidence reports whether a terminal status owns
// evidence preservation: failed and stopped sessions preserve their
// terminal transcript descriptors; every other status (including completed)
// stays evidence-free.
func shouldPreserveTerminalEvidence(status string) bool {
	return status == "failed" || status == "stopped"
}

// terminalEvidenceFileName is the sidecar written per evidence attempt. The
// "+" keeps it distinct from every per-repository "<label>.patch" /
// "<label>.json" pair: rescueSegment maps every character outside
// [A-Za-z0-9._-] to "_", so no repository name (even "transcripts") can
// sanitize to a label containing "+". An evidence-only attempt and a
// patch-bearing attempt therefore share the same directory naming and the
// same per-session retention boundary without overwriting each other.
const terminalEvidenceFileName = "transcripts+evidence.json"

// terminalEvidenceManifest is the local-only record of a session's terminal
// transcript descriptors. It is written under the rescue directory and never
// placed on any wire surface: the descriptors stay out of the terminal
// status body and the marshalled result exactly as before.
type terminalEvidenceManifest struct {
	SessionID   string   `json:"sessionId"`
	Status      string   `json:"status"`
	Transcripts []string `json:"transcripts,omitempty"`
	CreatedAt   string   `json:"createdAt"`
}

// teardownPreservation carries one teardown's archive directory across the
// patch phase (before the terminal lease decision) and the evidence phase
// (after the final status is known). Both phases share the same directory
// so evidence-only and patch-bearing attempts count together under
// rescueKeepPerSession as a single attempt per teardown.
type teardownPreservation struct {
	sessionRoot string
	archiveDir  string
	stamp       string
}

// newTeardownPreservation starts one teardown's preservation scope for the
// session. The stamp is fixed at creation so the patch and evidence phases
// share one directory even when the clock advances between them.
func (r *Runner) newTeardownPreservation(sessionID string) *teardownPreservation {
	return &teardownPreservation{
		sessionRoot: filepath.Join(r.rescueRoot(), rescueSegment(sessionID)),
		stamp:       r.now().UTC().Format("20060102T150405Z"),
	}
}

// dirFor creates this teardown's archive directory on first use, unique
// even when two teardowns of one session share a second.
func (p *teardownPreservation) dirFor() (string, error) {
	if p.archiveDir != "" {
		return p.archiveDir, nil
	}
	if err := os.MkdirAll(p.sessionRoot, 0o700); err != nil {
		return "", fmt.Errorf("create rescue directory: %w", err)
	}
	dir, err := os.MkdirTemp(p.sessionRoot, p.stamp+"-")
	if err != nil {
		return "", fmt.Errorf("create rescue directory: %w", err)
	}
	p.archiveDir = dir
	return dir, nil
}

// preservePatches archives every checkout's unpublished work into the
// scope's directory. It returns false when some checkout's work could not
// be preserved; the caller must then keep the workarea. Evidence is never
// archived here: the terminal status is not final until the lease decision
// runs, so evidence waits for preserveTerminalEvidenceFinal below.
func (r *Runner) preservePatches(qw QueuedWork, res *Result, pres *teardownPreservation) bool {
	if res == nil || pres == nil {
		return true
	}
	preserved := true
	dirFor := pres.dirFor
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
	return preserved
}

// preserveTerminalEvidenceFinal archives the session's terminal transcript
// descriptors based on the FINAL terminal status (after the lease decision
// may have flipped completed to failed). It shares the teardown's directory
// with any patch preservePatches archived, so one teardown still produces a
// single attempt. Evidence stays best-effort: failures are logged and never
// keep the workarea.
func (r *Runner) preserveTerminalEvidenceFinal(qw QueuedWork, res *Result, pres *teardownPreservation) {
	if res == nil || pres == nil {
		return
	}
	if err := r.archiveTerminalEvidence(qw.SessionID, res, pres.dirFor); err != nil {
		r.logger.Warn("could not preserve terminal evidence before teardown",
			"sessionId", qw.SessionID,
			"err", err,
		)
	}
}

// finalizeTeardownPreservation drops the teardown directory when it holds
// neither a patch nor terminal evidence, then prunes the session to the
// retention boundary. It runs once per teardown after both phases.
func (r *Runner) finalizeTeardownPreservation(qw QueuedWork, pres *teardownPreservation) {
	if pres == nil || pres.archiveDir == "" {
		return
	}
	// Removes the directory only when every checkout turned out to hold
	// nothing but non-work paths and no terminal evidence was archived.
	_ = os.Remove(pres.archiveDir)
	pruneRescueArchives(r, qw.SessionID, pres.sessionRoot)
}

// preserveUnpublishedWork archives every checkout's unpublished work before
// teardown and logs where it went. It returns false when some checkout's work
// could not be preserved; the caller must then keep the workarea.
//
// It also archives the session's terminal transcript descriptors as local
// evidence when the terminal status owns preservation (failed or stopped):
// the evidence shares this teardown's archive directory, so evidence-only
// and patch-bearing attempts count together under rescueKeepPerSession.
// Evidence is best-effort and never flips the return: a teardown with no
// unpublished work still tears down when the evidence cannot be written.
func (r *Runner) preserveUnpublishedWork(qw QueuedWork, res *Result) bool {
	if res == nil {
		return true
	}
	wantEvidence := shouldPreserveTerminalEvidence(res.Status) && len(res.terminalTranscripts) > 0
	if len(res.rescueTargets) == 0 && !wantEvidence {
		return true
	}
	pres := r.newTeardownPreservation(qw.SessionID)
	preserved := r.preservePatches(qw, res, pres)
	if wantEvidence {
		r.preserveTerminalEvidenceFinal(qw, res, pres)
	}
	r.finalizeTeardownPreservation(qw, pres)
	return preserved
}

// archiveTerminalEvidence writes the result's terminal transcript
// descriptors into this teardown's archive directory (created on first use
// via dirFor, shared with any patch this teardown archives). The manifest
// records only the descriptor paths; the transcript files themselves are
// never uploaded or placed on any wire surface, but readable ones are also
// copied next to the manifest as verified evidence-<index>-<base> files.
// A nil result, a status outside failed/stopped, or no descriptors is a
// no-op. Manifest failures are returned so the caller can log them; they
// never keep the workarea. Copy failures are best-effort per file and
// never fail the manifest or the teardown.
func (r *Runner) archiveTerminalEvidence(sessionID string, res *Result, dirFor func() (string, error)) error {
	if res == nil || !shouldPreserveTerminalEvidence(res.Status) || len(res.terminalTranscripts) == 0 {
		return nil
	}
	paths := make([]string, 0, len(res.terminalTranscripts))
	for _, desc := range res.terminalTranscripts {
		if desc.Path != "" {
			paths = append(paths, desc.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	dir, err := dirFor()
	if err != nil {
		return err
	}
	manifest := terminalEvidenceManifest{
		SessionID:   sessionID,
		Status:      res.Status,
		Transcripts: paths,
		CreatedAt:   r.now().UTC().Format(time.RFC3339),
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode terminal evidence: %w", err)
	}
	//nolint:gosec // G304: runner-owned path built from sanitized segments.
	if err := os.WriteFile(filepath.Join(dir, terminalEvidenceFileName), append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write terminal evidence: %w", err)
	}
	r.logger.Info("terminal evidence preserved before teardown",
		"sessionId", sessionID,
		"status", res.Status,
		"transcripts", len(paths),
	)
	// Best-effort content alongside the manifest, in the same directory:
	// readable transcripts are copied with verification, unreadable ones
	// are skipped with a log line. Copies never fail this function.
	r.copyTerminalEvidenceContents(sessionID, res, dir)
	return nil
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
