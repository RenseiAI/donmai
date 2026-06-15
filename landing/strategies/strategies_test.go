package strategies

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// defaultContext mirrors the legacy default MergeContext used across the suite.
func defaultContext() Context {
	return Context{
		RepoPath:     "/repo",
		WorktreePath: "/worktree",
		SourceBranch: "feature/test",
		TargetBranch: "main",
		Proposal:     42,
		Remote:       "origin",
	}
}

// The branch-conflict error strings (both git phrasings) — strategies must mark
// these retryable on Prepare. Generic paths, no private refs.
const (
	branchConflictUsedBy     = "Command failed: git checkout feature/test\nfatal: 'feature/test' is already used by worktree at '/home/u/repo.wt/feature-test-AC'"
	branchConflictCheckedOut = "fatal: 'feature/test' is already checked out at '/home/u/repo/.worktrees/feature-test-DEV'"
	missingSourceRef         = "Command failed: git fetch origin main feature/test\nfatal: couldn't find remote ref feature/test\n"
)

// strategyCalls returns only the strategy's own git calls, dropping the five
// cleanWorktreeState commands that every Prepare runs first.
func strategyCalls(fr *fakeRunner) []string {
	all := fr.commandLines()
	cleanup := []string{
		"git rebase --abort", "git merge --abort", "git cherry-pick --abort",
		"git reset --hard HEAD", "git clean -fd",
	}
	if len(all) < len(cleanup) {
		return all
	}
	// Cleanup always runs first in Prepare; drop the prefix only if it matches.
	for i, c := range cleanup {
		if all[i] != c {
			return all
		}
	}
	return all[len(cleanup):]
}

func TestRebaseStrategy_Name(t *testing.T) {
	if got := NewRebaseStrategy().Name(); got != NameRebase {
		t.Errorf("Name() = %q, want %q", got, NameRebase)
	}
}

func TestRebaseStrategy_Prepare(t *testing.T) {
	tests := []struct {
		name          string
		reply         func(string, []string) (string, error)
		wantSuccess   bool
		wantHeadSHA   string
		wantRetryable bool
		wantMerged    bool
		wantErrSub    string
	}{
		{
			name:        "fetch + detached source checkout + rev-parse",
			reply:       seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "abc123"}}),
			wantSuccess: true,
			wantHeadSHA: "abc123",
		},
		{
			name:       "fetch failure (non-conflict) is a hard failure",
			reply:      seqReply([]seqEntry{{match: "git fetch", errMsg: "fatal: could not read from remote"}}),
			wantErrSub: "could not read from remote",
		},
		{
			name:          "branch conflict (used by) is retryable",
			reply:         seqReply([]seqEntry{{match: "checkout --detach", errMsg: branchConflictUsedBy}}),
			wantRetryable: true,
			wantErrSub:    "already used by worktree",
		},
		{
			name:          "branch conflict (checked out) is retryable",
			reply:         seqReply([]seqEntry{{match: "checkout --detach", errMsg: branchConflictCheckedOut}}),
			wantRetryable: true,
		},
		{
			name:       "missing source ref ⇒ alreadyMerged",
			reply:      seqReply([]seqEntry{{match: "git fetch", errMsg: missingSourceRef}}),
			wantMerged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{reply: tt.reply}
			s := &RebaseStrategy{runner: fr}
			res, err := s.Prepare(context.Background(), defaultContext())
			if err != nil {
				t.Fatalf("Prepare error: %v", err)
			}
			if res.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v", res.Success, tt.wantSuccess)
			}
			if res.HeadSHA != tt.wantHeadSHA {
				t.Errorf("HeadSHA = %q, want %q", res.HeadSHA, tt.wantHeadSHA)
			}
			if res.Retryable != tt.wantRetryable {
				t.Errorf("Retryable = %v, want %v", res.Retryable, tt.wantRetryable)
			}
			if res.AlreadyMerged != tt.wantMerged {
				t.Errorf("AlreadyMerged = %v, want %v", res.AlreadyMerged, tt.wantMerged)
			}
			if tt.wantErrSub != "" && !strings.Contains(res.Error, tt.wantErrSub) {
				t.Errorf("Error = %q, want it to contain %q", res.Error, tt.wantErrSub)
			}
			if tt.wantSuccess {
				calls := strategyCalls(fr)
				if len(calls) < 2 {
					t.Fatalf("expected fetch+checkout calls, got %v", calls)
				}
				if calls[0] != "git fetch origin main feature/test" {
					t.Errorf("fetch = %q", calls[0])
				}
				if calls[1] != "git checkout --detach origin/feature/test" {
					t.Errorf("checkout = %q, want detached source checkout", calls[1])
				}
			}
		})
	}
}

func TestRebaseStrategy_NeverNonDetachedCheckout(t *testing.T) {
	fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "abc123"}})}
	s := &RebaseStrategy{runner: fr}
	if _, err := s.Prepare(context.Background(), defaultContext()); err != nil {
		t.Fatalf("Prepare error: %v", err)
	}
	assertNoPlainCheckout(t, fr)
}

func TestRebaseStrategy_Execute(t *testing.T) {
	tests := []struct {
		name        string
		reply       func(string, []string) (string, error)
		wantStatus  string
		wantSHA     string
		wantFiles   []string
		wantDetails string
		wantErrSub  string
	}{
		{
			name:       "success returns merged SHA",
			reply:      seqReply([]seqEntry{{match: "git rebase origin/main", stdout: ""}, {match: "rev-parse HEAD", stdout: "def456"}}),
			wantStatus: StatusSuccess,
			wantSHA:    "def456",
		},
		{
			name: "conflict aborts and returns files",
			reply: seqReply([]seqEntry{
				{match: "git rebase origin/main", errMsg: "CONFLICT (content): Merge conflict in file.ts"},
				{match: "diff --name-only --diff-filter=U", stdout: "src/file.ts\nsrc/other.ts"},
			}),
			wantStatus:  StatusConflict,
			wantFiles:   []string{"src/file.ts", "src/other.ts"},
			wantDetails: "Rebase conflict in 2 file(s)",
		},
		{
			name: "non-conflict failure aborts and returns error",
			reply: seqReply([]seqEntry{
				{match: "git rebase origin/main", errMsg: "fatal: invalid upstream"},
				// diff returns nothing ⇒ not a conflict
			}),
			wantStatus: StatusError,
			wantErrSub: "invalid upstream",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{reply: tt.reply}
			s := &RebaseStrategy{runner: fr}
			res, err := s.Execute(context.Background(), defaultContext())
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", res.Status, tt.wantStatus)
			}
			if res.MergedSHA != tt.wantSHA {
				t.Errorf("MergedSHA = %q, want %q", res.MergedSHA, tt.wantSHA)
			}
			if tt.wantFiles != nil && !reflect.DeepEqual(res.ConflictFiles, tt.wantFiles) {
				t.Errorf("ConflictFiles = %v, want %v", res.ConflictFiles, tt.wantFiles)
			}
			if tt.wantDetails != "" && res.ConflictDetails != tt.wantDetails {
				t.Errorf("ConflictDetails = %q, want %q", res.ConflictDetails, tt.wantDetails)
			}
			if tt.wantErrSub != "" && !strings.Contains(res.Error, tt.wantErrSub) {
				t.Errorf("Error = %q, want it to contain %q", res.Error, tt.wantErrSub)
			}
		})
	}
}

func TestRebaseStrategy_Finalize(t *testing.T) {
	t.Run("pushes source force-with-lease then fast-forwards target with rebased SHA", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "rev-parse HEAD", stdout: "abc123"}, // prepare
			{match: "git rebase origin/main", stdout: ""},
			{match: "rev-parse HEAD", stdout: "def456"}, // execute → rebasedSha
		})}
		s := &RebaseStrategy{runner: fr}
		ctx := context.Background()
		if _, err := s.Prepare(ctx, defaultContext()); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if _, err := s.Execute(ctx, defaultContext()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		// Reset recorded calls before finalize for a clean assertion.
		fr.mu.Lock()
		fr.calls = nil
		fr.mu.Unlock()
		if err := s.Finalize(ctx, defaultContext()); err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		got := fr.commandLines()
		want := []string{
			"git push origin HEAD:feature/test --force-with-lease=feature/test",
			"git push origin def456:main",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("finalize commands = %v, want %v", got, want)
		}
	})

	t.Run("falls back to current HEAD when no rebased SHA captured", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "fallback-sha"}})}
		s := &RebaseStrategy{runner: fr}
		if err := s.Finalize(context.Background(), defaultContext()); err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		got := fr.commandLines()
		if len(got) != 3 || got[0] != "git rev-parse HEAD" {
			t.Fatalf("commands = %v, want rev-parse fallback first", got)
		}
		if got[2] != "git push origin fallback-sha:main" {
			t.Errorf("target push = %q, want fallback-sha", got[2])
		}
	})

	t.Run("push failure surfaces as error", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "rev-parse HEAD", stdout: "abc"},
			{match: "push origin HEAD:feature/test", errMsg: "rejected: non-fast-forward"},
		})}
		s := &RebaseStrategy{runner: fr}
		err := s.Finalize(context.Background(), defaultContext())
		if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
			t.Errorf("Finalize err = %v, want non-fast-forward", err)
		}
	})
}

func TestMergeCommitStrategy_Prepare(t *testing.T) {
	fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "aaa111"}})}
	s := &MergeCommitStrategy{runner: fr}
	res, err := s.Prepare(context.Background(), defaultContext())
	if err != nil {
		t.Fatalf("Prepare error: %v", err)
	}
	if !res.Success || res.HeadSHA != "aaa111" {
		t.Fatalf("res = %+v, want success aaa111", res)
	}
	calls := strategyCalls(fr)
	if calls[0] != "git fetch origin main feature/test" {
		t.Errorf("fetch = %q", calls[0])
	}
	if calls[1] != "git checkout --detach origin/main" {
		t.Errorf("checkout = %q, want detached TARGET checkout", calls[1])
	}
	assertNoPlainCheckout(t, fr)
}

func TestMergeCommitStrategy_Prepare_Errors(t *testing.T) {
	t.Run("branch conflicts (both phrasings) are retryable", func(t *testing.T) {
		for _, msg := range []string{branchConflictUsedBy, branchConflictCheckedOut} {
			fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "checkout --detach", errMsg: msg}})}
			s := &MergeCommitStrategy{runner: fr}
			res, _ := s.Prepare(context.Background(), defaultContext())
			if res.Success || !res.Retryable {
				t.Errorf("for %q: res = %+v, want retryable failure", msg, res)
			}
		}
	})
	t.Run("missing source ref ⇒ alreadyMerged", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "git fetch", errMsg: missingSourceRef}})}
		s := &MergeCommitStrategy{runner: fr}
		res, _ := s.Prepare(context.Background(), defaultContext())
		if res.Success || !res.AlreadyMerged {
			t.Errorf("res = %+v, want alreadyMerged failure", res)
		}
	})
	t.Run("plain error is a hard failure", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "git fetch", errMsg: "error: pathspec did not match"}})}
		s := &MergeCommitStrategy{runner: fr}
		res, _ := s.Prepare(context.Background(), defaultContext())
		if res.Success || res.Retryable || res.AlreadyMerged {
			t.Errorf("res = %+v, want plain failure", res)
		}
		if !strings.Contains(res.Error, "pathspec did not match") {
			t.Errorf("Error = %q", res.Error)
		}
	})
}

func TestMergeCommitStrategy_Execute(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --no-ff", stdout: ""},
			{match: "rev-parse HEAD", stdout: "bbb222"},
		})}
		s := &MergeCommitStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusSuccess || res.MergedSHA != "bbb222" {
			t.Fatalf("res = %+v, want success bbb222", res)
		}
		mergeLine := fr.commandLines()[0]
		want := `git merge --no-ff origin/feature/test -m Merge PR #42 from feature/test`
		if mergeLine != want {
			t.Errorf("merge command = %q, want %q", mergeLine, want)
		}
	})
	t.Run("conflict aborts and returns files", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --no-ff", errMsg: "CONFLICT"},
			{match: "diff --name-only --diff-filter=U", stdout: "src/conflict.ts"},
		})}
		s := &MergeCommitStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusConflict {
			t.Fatalf("Status = %q, want conflict", res.Status)
		}
		if !reflect.DeepEqual(res.ConflictFiles, []string{"src/conflict.ts"}) {
			t.Errorf("ConflictFiles = %v", res.ConflictFiles)
		}
		if res.ConflictDetails != "Merge conflict in 1 file(s)" {
			t.Errorf("ConflictDetails = %q", res.ConflictDetails)
		}
	})
	t.Run("non-conflict error", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --no-ff", errMsg: "fatal: refusing to merge unrelated histories"},
		})}
		s := &MergeCommitStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusError || !strings.Contains(res.Error, "unrelated histories") {
			t.Errorf("res = %+v, want error unrelated histories", res)
		}
	})
}

func TestMergeCommitStrategy_Finalize(t *testing.T) {
	fr := &fakeRunner{}
	s := &MergeCommitStrategy{runner: fr}
	if err := s.Finalize(context.Background(), defaultContext()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got := fr.commandLines()
	if len(got) != 1 || got[0] != "git push origin HEAD:main" {
		t.Errorf("finalize commands = %v, want [git push origin HEAD:main]", got)
	}
	assertNoPlainCheckout(t, fr)
}

func TestSquashStrategy_Prepare(t *testing.T) {
	fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "ccc333"}})}
	s := &SquashStrategy{runner: fr}
	res, err := s.Prepare(context.Background(), defaultContext())
	if err != nil {
		t.Fatalf("Prepare error: %v", err)
	}
	if !res.Success || res.HeadSHA != "ccc333" {
		t.Fatalf("res = %+v, want success ccc333", res)
	}
	calls := strategyCalls(fr)
	if calls[0] != "git fetch origin main feature/test" {
		t.Errorf("fetch = %q", calls[0])
	}
	if calls[1] != "git checkout --detach origin/main" {
		t.Errorf("checkout = %q, want detached TARGET checkout", calls[1])
	}
	assertNoPlainCheckout(t, fr)
}

func TestSquashStrategy_Prepare_Errors(t *testing.T) {
	t.Run("branch conflicts retryable", func(t *testing.T) {
		for _, msg := range []string{branchConflictUsedBy, branchConflictCheckedOut} {
			fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "checkout --detach", errMsg: msg}})}
			s := &SquashStrategy{runner: fr}
			res, _ := s.Prepare(context.Background(), defaultContext())
			if res.Success || !res.Retryable {
				t.Errorf("for %q: res = %+v, want retryable", msg, res)
			}
		}
	})
	t.Run("missing source ref ⇒ alreadyMerged", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "git fetch", errMsg: missingSourceRef}})}
		s := &SquashStrategy{runner: fr}
		res, _ := s.Prepare(context.Background(), defaultContext())
		if res.Success || !res.AlreadyMerged {
			t.Errorf("res = %+v, want alreadyMerged", res)
		}
	})
}

func TestSquashStrategy_Execute(t *testing.T) {
	t.Run("success runs merge --squash then commit", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --squash", stdout: ""},
			{match: "git commit", stdout: ""},
			{match: "rev-parse HEAD", stdout: "ddd444"},
		})}
		s := &SquashStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusSuccess || res.MergedSHA != "ddd444" {
			t.Fatalf("res = %+v, want success ddd444", res)
		}
		lines := fr.commandLines()
		if lines[0] != "git merge --squash origin/feature/test" {
			t.Errorf("merge = %q", lines[0])
		}
		if lines[1] != `git commit -m Squash merge PR #42 from feature/test` {
			t.Errorf("commit = %q", lines[1])
		}
	})
	t.Run("conflict aborts and returns files", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --squash", errMsg: "CONFLICT"},
			{match: "diff --name-only --diff-filter=U", stdout: "src/a.ts\nsrc/b.ts"},
		})}
		s := &SquashStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusConflict {
			t.Fatalf("Status = %q, want conflict", res.Status)
		}
		if !reflect.DeepEqual(res.ConflictFiles, []string{"src/a.ts", "src/b.ts"}) {
			t.Errorf("ConflictFiles = %v", res.ConflictFiles)
		}
		if res.ConflictDetails != "Squash merge conflict in 2 file(s)" {
			t.Errorf("ConflictDetails = %q", res.ConflictDetails)
		}
	})
	t.Run("non-conflict error", func(t *testing.T) {
		fr := &fakeRunner{reply: seqReply([]seqEntry{
			{match: "git merge --squash", errMsg: "fatal: not something we can merge"},
		})}
		s := &SquashStrategy{runner: fr}
		res, _ := s.Execute(context.Background(), defaultContext())
		if res.Status != StatusError || !strings.Contains(res.Error, "not something we can merge") {
			t.Errorf("res = %+v", res)
		}
	})
}

func TestSquashStrategy_Finalize(t *testing.T) {
	fr := &fakeRunner{}
	s := &SquashStrategy{runner: fr}
	if err := s.Finalize(context.Background(), defaultContext()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got := fr.commandLines()
	if len(got) != 1 || got[0] != "git push origin HEAD:main" {
		t.Errorf("finalize commands = %v, want [git push origin HEAD:main]", got)
	}
}

func TestAllStrategies_UseWorktreePathAsDir(t *testing.T) {
	const dir = "/custom/worktree/path"
	lc := Context{RepoPath: "/repo", WorktreePath: dir, SourceBranch: "feature/test", TargetBranch: "main", Proposal: 42, Remote: "origin"}

	for _, name := range []string{NameRebase, NameMerge, NameSquash} {
		fr := &fakeRunner{reply: seqReply([]seqEntry{{match: "rev-parse HEAD", stdout: "x"}})}
		var s Strategy
		switch name {
		case NameRebase:
			s = &RebaseStrategy{runner: fr}
		case NameMerge:
			s = &MergeCommitStrategy{runner: fr}
		case NameSquash:
			s = &SquashStrategy{runner: fr}
		}
		if _, err := s.Prepare(context.Background(), lc); err != nil {
			t.Fatalf("%s Prepare: %v", s.Name(), err)
		}
		for _, d := range fr.dirs() {
			if d != dir {
				t.Errorf("%s: dir = %q, want %q", s.Name(), d, dir)
			}
		}
	}
}

// assertNoPlainCheckout fails if any recorded git checkout omits --detach.
func assertNoPlainCheckout(t *testing.T, fr *fakeRunner) {
	t.Helper()
	for _, c := range fr.calls {
		if c.name != "git" || len(c.args) == 0 || c.args[0] != "checkout" {
			continue
		}
		detached := false
		for _, a := range c.args[1:] {
			if a == "--detach" {
				detached = true
				break
			}
		}
		if !detached {
			t.Errorf("non-detached checkout issued: %v", c.args)
		}
	}
}
