package signalscope

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInScope(t *testing.T) {
	t.Parallel()
	inGroup := func(pid, pgid int) bool { return pid == 100 && pgid == 7 }
	full := Scope{Worktree: "/tmp/sess/wt", WorkareaRoot: "/tmp/sess", ProcessGroup: 7}
	noRoot := Scope{Worktree: "/tmp/sess/wt", ProcessGroup: 7}
	for _, tc := range []struct {
		name  string
		scope Scope
		pid   int
		cwd   string
		want  bool
	}{
		{name: "process inside the worktree is in scope", scope: full, pid: 1, cwd: "/tmp/sess/wt/sub", want: true},
		{name: "process at the worktree root is in scope", scope: full, pid: 1, cwd: "/tmp/sess/wt", want: true},
		{name: "process inside the workarea root is in scope", scope: full, pid: 1, cwd: "/tmp/sess/other", want: true},
		{name: "process group member outside the worktree is in scope", scope: full, pid: 100, cwd: "/var/tmp/elsewhere", want: true},
		{name: "sibling of the session root does not match", scope: full, pid: 1, cwd: "/tmp/sess-evil", want: false},
		{name: "sibling of the worktree does not match the worktree leg", scope: noRoot, pid: 1, cwd: "/tmp/sess/wt-evil", want: false},
		{name: "outside process outside group is refused", scope: full, pid: 2, cwd: "/var/tmp/elsewhere", want: false},
		{name: "unknown cwd with no group is refused", scope: full, pid: 2, cwd: "", want: false},
		{name: "empty scope refuses everything", scope: Scope{}, pid: 100, cwd: "/tmp/sess/wt", want: false},
		{name: "zero process group disables the group leg", scope: Scope{Worktree: "/tmp/sess/wt"}, pid: 100, cwd: "/elsewhere", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := InScope(tc.scope, tc.pid, tc.cwd, inGroup); got != tc.want {
				t.Fatalf("InScope(pid=%d, cwd=%q) = %v, want %v", tc.pid, tc.cwd, got, tc.want)
			}
		})
	}
}

func TestInsideDirCanonicalisesSymlinks(t *testing.T) {
	t.Parallel()
	// A symlinked dir and its canonical target compare equal in both
	// directions: this is the macOS /tmp -> /private/tmp shape that
	// first broke the guard scoping (lsof reports the canonical cwd).
	parent := t.TempDir()
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	link := parent + "-link"
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if !insideDir(canonical, link) || !insideDir(link, canonical) {
		t.Fatalf("symlinked spellings do not compare equal: %q vs %q", canonical, link)
	}
	if !insideDir(filepath.Join(link, "sub"), parent) {
		t.Fatalf("descendant via symlink not matched")
	}
	if insideDir(parent+"-evil", parent) {
		t.Fatalf("sibling prefix matched")
	}
}
