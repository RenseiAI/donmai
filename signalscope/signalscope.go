// Package signalscope owns the session-scoped process-signal policy.
//
// Host-mode sessions run as the operator's OS user, so an agent's pattern
// kill ("pkill -f <name>", "killall <name>") can match processes owned by
// other sessions or by the operator's own builds. This package is the single
// source of truth for what a session may signal:
//
//   - A process is in scope when its working directory is inside the
//     session's worktree (or workarea root), or when it belongs to the
//     session's recorded process group.
//   - Everything else is refused and reported, never silently skipped.
//
// The runner-provided pkill/killall guard scripts (runner/signal_guard.go)
// enforce this policy at exec time; this package holds the policy itself so
// the guard, the runner, and tests share one definition.
package signalscope

import (
	"os"
	"path/filepath"
	"strings"
)

// Scope describes the session-owned process set one pattern kill is
// checked against.
type Scope struct {
	// Worktree is the session's worktree path. Empty disables the
	// worktree leg (the process-group leg still applies).
	Worktree string
	// WorkareaRoot is the session's workarea root (the multi-repository
	// parent of Worktree). Empty disables the workarea leg.
	WorkareaRoot string
	// ProcessGroup is the session's recorded process-group id. Zero or
	// negative disables the process-group leg.
	ProcessGroup int
}

// memberOfGroup reports whether pid belongs to pgid. It is a seam so
// tests can drive group membership without real processes; production
// code passes a getpgid-backed implementation.
type memberOfGroup func(pid, pgid int) bool

// InScope reports whether the process described by (pid, cwd) may be
// signalled under scope. Either leg suffices: a process inside the
// session's worktree (or workarea root), or a member of the session's
// recorded process group. A process matching neither leg is out of
// scope and must be refused.
func InScope(scope Scope, pid int, cwd string, inGroup memberOfGroup) bool {
	if cwd != "" && (insideDir(cwd, scope.Worktree) || insideDir(cwd, scope.WorkareaRoot)) {
		return true
	}
	if scope.ProcessGroup > 0 && inGroup != nil && inGroup(pid, scope.ProcessGroup) {
		return true
	}
	return false
}

// insideDir reports whether path is root or a descendant of root.
// Both sides are cleaned; an empty root never matches. The match is
// path-segment aware so /tmp/sess-evil does not match /tmp/sess.
// Symlinked spellings of the same directory compare equal: both sides
// are evaluated (EvalSymlinks) before comparison, because on macOS /tmp
// and /var are symlinks into /private while process-table cwd reports
// the canonical spelling. An evaluation failure falls back to the
// cleaned-but-unevaluated path rather than refusing outright.
func insideDir(path, root string) bool {
	if root == "" || path == "" {
		return false
	}
	cleanPath := canonDir(path)
	cleanRoot := canonDir(root)
	if cleanPath == cleanRoot {
		return true
	}
	if !strings.HasSuffix(cleanRoot, string(filepath.Separator)) {
		cleanRoot += string(filepath.Separator)
	}
	return strings.HasPrefix(cleanPath, cleanRoot)
}

// canonDir cleans path and resolves symlinks in its existing prefix so
// two spellings of the same directory compare equal. Failures fall back
// to the cleaned path.
func canonDir(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	// The full path may not exist (a dead cwd, a not-yet-created dir):
	// resolve the deepest existing ancestor and re-append the rest.
	if abs, err := filepath.Abs(cleaned); err == nil {
		if resolved, err := evalExistingParent(abs); err == nil {
			return resolved
		}
	}
	return cleaned
}

// evalExistingParent resolves symlinks in the deepest existing ancestor
// of abs (which must be absolute and clean) and re-appends the
// non-existing tail.
func evalExistingParent(abs string) (string, error) {
	cur := abs
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}
