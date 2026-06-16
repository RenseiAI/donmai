package strategies

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCleanWorktreeState_CommandSequence(t *testing.T) {
	fr := &fakeRunner{}
	if err := cleanWorktreeState(context.Background(), fr, "/worktree"); err != nil {
		t.Fatalf("cleanWorktreeState error: %v", err)
	}
	want := []string{
		"git rebase --abort",
		"git merge --abort",
		"git cherry-pick --abort",
		"git reset --hard HEAD",
		"git clean -fd",
	}
	if got := fr.commandLines(); !reflect.DeepEqual(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
}

func TestCleanWorktreeState_UsesWorktreePathAsDir(t *testing.T) {
	fr := &fakeRunner{}
	const dir = "/some/custom/path"
	if err := cleanWorktreeState(context.Background(), fr, dir); err != nil {
		t.Fatalf("cleanWorktreeState error: %v", err)
	}
	for _, d := range fr.dirs() {
		if d != dir {
			t.Errorf("dir = %q, want %q", d, dir)
		}
	}
}

func TestCleanWorktreeState_NoThrowEvenWhenEveryCommandFails(t *testing.T) {
	fr := &fakeRunner{reply: func(string, []string) (string, error) {
		return "", errors.New("fail")
	}}
	if err := cleanWorktreeState(context.Background(), fr, "/worktree"); err != nil {
		t.Fatalf("cleanWorktreeState returned error despite swallow contract: %v", err)
	}
	// clean must still be attempted after earlier failures.
	if got := fr.commandLines(); len(got) != 5 || !strings.Contains(got[4], "git clean -fd") {
		t.Errorf("commands = %v, want clean -fd attempted last", got)
	}
}

// TestAddWorktree_PruneBeforeAdd verifies that addWorktree runs
// "git worktree remove --force" and "git worktree prune" BEFORE the actual
// "git worktree add". This is the crash-leak recovery path: if a prior run
// crashed before its defer-remove ran, git retains the stale path registration
// and a bare "worktree add" at the same deterministic path would fail
// non-retryably. The prune-before-add clears stale metadata so the subsequent
// add succeeds even when the path was previously registered.
func TestAddWorktree_PruneBeforeAdd(t *testing.T) {
	// Simulate a "stale" worktree by making "worktree add" fail the first
	// time it runs (as git would when the path is already registered), then
	// succeed. With the prune-before-add in place, the force-remove + prune
	// run BEFORE the add, so the add call itself should see a clean state and
	// succeed (here simulated by the default no-error fake reply).
	//
	// We verify the command ordering directly: remove → prune → add.
	fr := &fakeRunner{}
	if err := addWorktree(context.Background(), fr, "/repo", "/repo/.wt/p1", "main"); err != nil {
		t.Fatalf("addWorktree with prune-before-add returned error: %v", err)
	}
	got := fr.commandLines()
	// Expect exactly: [worktree remove --force, worktree prune, worktree add --detach]
	if len(got) != 3 {
		t.Fatalf("addWorktree ran %d commands, want 3 (remove + prune + add): %v", len(got), got)
	}
	if !strings.Contains(got[0], "worktree remove --force") {
		t.Errorf("cmd[0] = %q, want worktree remove --force", got[0])
	}
	if !strings.Contains(got[1], "worktree prune") {
		t.Errorf("cmd[1] = %q, want worktree prune", got[1])
	}
	if !strings.Contains(got[2], "worktree add --detach") {
		t.Errorf("cmd[2] = %q, want worktree add --detach", got[2])
	}
}

func TestAddWorktree_CommandAndDetach(t *testing.T) {
	fr := &fakeRunner{}
	if err := addWorktree(context.Background(), fr, "/repo", "/repo/.wt/p1", "main"); err != nil {
		t.Fatalf("addWorktree error: %v", err)
	}
	got := fr.commandLines()
	// addWorktree now runs remove --force + prune before the actual add (D2 prune-before-add).
	if len(got) != 3 {
		t.Fatalf("addWorktree ran %d commands, want 3 (remove + prune + add): %v", len(got), got)
	}
	want := "git worktree add --detach /repo/.wt/p1 main"
	if got[2] != want {
		t.Errorf("add command = %q, want %q", got[2], want)
	}
	// All commands must run from the repo root, not the worktree path.
	for i, d := range fr.dirs() {
		if d != "/repo" {
			t.Errorf("cmd[%d] dir = %q, want /repo", i, d)
		}
	}
}

func TestAddWorktree_PropagatesError(t *testing.T) {
	fr := &fakeRunner{reply: func(string, []string) (string, error) {
		return "", errors.New("fatal: invalid reference")
	}}
	err := addWorktree(context.Background(), fr, "/repo", "/repo/.wt/p1", "main")
	if err == nil {
		t.Fatal("addWorktree should return an error when git fails")
	}
	if !strings.Contains(err.Error(), "git worktree add") {
		t.Errorf("error = %v, want it to wrap the git worktree add failure", err)
	}
}

func TestRemoveWorktree_CommandAndForce(t *testing.T) {
	fr := &fakeRunner{}
	if err := removeWorktree(context.Background(), fr, "/repo", "/repo/.wt/p1"); err != nil {
		t.Fatalf("removeWorktree error: %v", err)
	}
	got := fr.commandLines()
	if len(got) != 1 {
		t.Fatalf("removeWorktree ran %d commands, want 1: %v", len(got), got)
	}
	// --force because a failed landing may leave staged/untracked changes.
	want := "git worktree remove --force /repo/.wt/p1"
	if got[0] != want {
		t.Errorf("command = %q, want %q", got[0], want)
	}
}

func TestRemoveWorktree_PropagatesError(t *testing.T) {
	fr := &fakeRunner{reply: func(string, []string) (string, error) {
		return "", errors.New("fatal: not a working tree")
	}}
	if err := removeWorktree(context.Background(), fr, "/repo", "/repo/.wt/p1"); err == nil {
		t.Fatal("removeWorktree should return an error when git fails")
	}
}

func TestCleanWorktreeState_NeverUsesCleanX(t *testing.T) {
	fr := &fakeRunner{}
	if err := cleanWorktreeState(context.Background(), fr, "/worktree"); err != nil {
		t.Fatalf("cleanWorktreeState error: %v", err)
	}
	// Guard: a `git clean` flag must never contain 'x' (e.g. -fdx / -x), which
	// would delete ignored files such as node_modules and slow every Prepare.
	for _, c := range fr.calls {
		if c.name != "git" || len(c.args) == 0 || c.args[0] != "clean" {
			continue
		}
		for _, flag := range c.args[1:] {
			if strings.HasPrefix(flag, "-") && strings.Contains(flag, "x") {
				t.Errorf("git clean uses an x flag (deletes ignored files): %v", c.args)
			}
		}
	}
}
