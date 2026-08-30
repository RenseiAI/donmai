package pi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The other half of the state-directory fix (statedir_guard.go carries the
// enforcement half): remove the MOTIVE.
//
// piStateDir is created inside the checkout on purpose — the worktree
// lifecycle owns its cleanup — but a checkout whose .gitignore does not
// mention it reports it as `?? .pi/` in `git status`. To a session (or a
// human) reading that output it is indistinguishable from build junk the run
// itself dropped, and the obvious tidy-up is to delete it. That is exactly
// how a live session's storage was removed on 2026-08-29.
//
// Nothing here touches the tracked .gitignore: excluding harness state is a
// property of THIS checkout being driven by the harness, not of the project,
// and a session must never produce a tracked-file diff nobody asked for. The
// per-checkout exclude file (`$(git rev-parse --git-path info/exclude)`) is
// git's own place for exactly that. Note that for a linked worktree git
// resolves that path into the SHARED git dir, so the entry is written once
// for the whole clone rather than per worktree — which is precisely the
// behaviour that makes `git status` quiet in every worktree of it.

const (
	// gitExcludeHeader precedes the entries this package appends, so a human
	// reading the exclude file later knows what wrote them and why.
	gitExcludeHeader = "# donmai: harness session state (created per session; the harness reads it for the session's life)"

	// gitProbeTimeout bounds the single `git rev-parse` this file runs. A
	// wedged git must never delay a spawn: the probe gives up and the write
	// is skipped, which costs a noisy `git status` and nothing else.
	gitProbeTimeout = 5 * time.Second
)

// piGitExcludeEntries are the entries materializeExtension keeps in the
// checkout's exclude file.
func piGitExcludeEntries() []string { return []string{piStateDir + "/"} }

// ensureGitExcluded appends any missing entries to the exclude file of the
// git checkout containing dir. It is idempotent: an entry already present —
// whether this call put it there or a previous one did — is not written
// again, so repeated session starts in one checkout leave exactly one line
// per entry.
//
// It no-ops silently when dir is not a git checkout (or git is unavailable):
// there is nothing to exclude from, and a non-git workarea is a perfectly
// ordinary way to run a session.
//
// Concurrency: two sessions starting in the same checkout at the same instant
// can both observe an entry as missing and both append it. The consequence is
// a duplicate line in a file where duplicates are meaningless to git, and it
// self-heals — every later session sees the entry and writes nothing. That is
// deliberately preferred to a lock file, whose stale-lock failure mode would
// be a silently un-excluded state dir.
func ensureGitExcluded(dir string, entries ...string) error {
	if strings.TrimSpace(dir) == "" || len(entries) == 0 {
		return nil
	}
	excludePath, ok := gitInfoExcludePath(dir)
	if !ok {
		return nil
	}

	existing, err := os.ReadFile(excludePath) //nolint:gosec // G304: the path comes from git itself (rev-parse --git-path), not from session or model input.
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pi: read git exclude file %s: %w", excludePath, err)
	}

	present := make(map[string]bool)
	for _, line := range strings.Split(string(existing), "\n") {
		present[strings.TrimSpace(line)] = true
	}

	var pending bytes.Buffer
	if !present[gitExcludeHeader] {
		pending.WriteString(gitExcludeHeader + "\n")
	}
	wrote := false
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || present[entry] {
			continue
		}
		present[entry] = true
		pending.WriteString(entry + "\n")
		wrote = true
	}
	if !wrote {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil { //nolint:gosec // G301: git's own info/ directory, created with the permissions git itself uses.
		return fmt.Errorf("pi: create git exclude directory: %w", err)
	}
	f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G302/G304: a git metadata file, world-readable exactly as git writes it.
	if err != nil {
		return fmt.Errorf("pi: open git exclude file %s: %w", excludePath, err)
	}
	// A pre-existing file that does not end in a newline would otherwise get
	// our first entry glued onto its last line.
	var out bytes.Buffer
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		out.WriteString("\n")
	}
	out.Write(pending.Bytes())
	if _, err := f.Write(out.Bytes()); err != nil {
		_ = f.Close()
		return fmt.Errorf("pi: write git exclude file %s: %w", excludePath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("pi: close git exclude file %s: %w", excludePath, err)
	}
	return nil
}

// gitInfoExcludePath asks git where dir's exclude file lives. `rev-parse
// --git-path info/exclude` is the only correct way to ask: in a linked
// worktree the answer is in the shared git dir, NOT `<dir>/.git/info/exclude`
// (there is no such file — `<dir>/.git` is a file, not a directory).
//
// ok is false whenever the question has no answer — dir is not a checkout,
// git is not installed, or the probe timed out.
func gitInfoExcludePath(dir string) (path string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()

	//nolint:gosec // G204: every argument but `dir` is a literal, and `dir` is the
	// session worktree path the harness itself was handed — never model or
	// request input. git is resolved from PATH exactly as every other git
	// invocation in this repo does.
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-path", "info/exclude")
	// Drop the ambient git location vars: an inherited GIT_DIR / GIT_WORK_TREE
	// (a hook, a wrapping git command) would answer for a DIFFERENT repository
	// than the one this session is running in.
	cmd.Env = gitLocationNeutralEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	answer := strings.TrimSpace(string(out))
	if answer == "" {
		return "", false
	}
	// git answers relative to the directory it ran in (`-C dir`) unless the
	// path is already absolute.
	if !filepath.IsAbs(answer) {
		answer = filepath.Join(dir, answer)
	}
	return answer, true
}

// gitLocationNeutralEnv returns env with the variables that would repoint git
// at another repository removed.
func gitLocationNeutralEnv(env []string) []string {
	drop := map[string]bool{
		"GIT_DIR":        true,
		"GIT_WORK_TREE":  true,
		"GIT_COMMON_DIR": true,
		"GIT_INDEX_FILE": true,
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, found := strings.Cut(kv, "=")
		if found && drop[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
