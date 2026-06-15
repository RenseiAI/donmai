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
