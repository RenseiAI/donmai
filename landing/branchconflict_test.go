package landing

import "testing"

func TestIsBranchConflictError(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "matches the used-by-worktree phrasing",
			msg:  "Command failed: git checkout feature/x\nfatal: 'feature/x' is already used by worktree at '/home/u/repo.wt/feature-x-AC'",
			want: true,
		},
		{
			name: "matches the checked-out-at phrasing",
			msg:  "fatal: 'feature/x' is already checked out at '/home/u/repo/.worktrees/feature-x'",
			want: true,
		},
		{
			name: "unrelated remote error does not match",
			msg:  "fatal: could not read from remote repository",
			want: false,
		},
		{
			name: "merge conflict text does not match",
			msg:  "CONFLICT (content): Merge conflict in foo.ts",
			want: false,
		},
		{
			name: "pathspec error does not match",
			msg:  `error: pathspec "X" did not match any file(s) known to git`,
			want: false,
		},
		{
			name: "empty input does not match",
			msg:  "",
			want: false,
		},
		{
			// git always emits lowercase; the classifier must stay case-sensitive
			// so downstream retry logic keeps firing on the real message only.
			name: "uppercase variant does not match (case sensitive)",
			msg:  "'X' IS ALREADY USED BY WORKTREE AT '/p'",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBranchConflictError(tt.msg); got != tt.want {
				t.Errorf("IsBranchConflictError(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestParseConflictingWorktreePath(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "extracts path from used-by-worktree phrasing",
			msg:  "fatal: 'feature/x' is already used by worktree at '/home/u/repo.wt/feature-x-AC'",
			want: "/home/u/repo.wt/feature-x-AC",
		},
		{
			name: "extracts path from checked-out-at phrasing",
			msg:  "fatal: 'feature/x' is already checked out at '/home/u/.worktrees/feature-x'",
			want: "/home/u/.worktrees/feature-x",
		},
		{
			name: "handles paths with spaces",
			msg:  "fatal: 'X' is already used by worktree at '/home/My User/repo/wt/X'",
			want: "/home/My User/repo/wt/X",
		},
		{
			name: "non-matching message yields empty",
			msg:  "fatal: not a git repository",
			want: "",
		},
		{
			name: "empty input yields empty",
			msg:  "",
			want: "",
		},
		{
			name: "unquoted path is not matched",
			msg:  "is already used by worktree at /no/quotes/here",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseConflictingWorktreePath(tt.msg); got != tt.want {
				t.Errorf("ParseConflictingWorktreePath(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

func TestIsMissingRemoteRefError(t *testing.T) {
	tests := []struct {
		name         string
		msg          string
		sourceBranch string
		want         bool
	}{
		{
			name:         "matches missing source ref for the named branch",
			msg:          "Command failed: git fetch origin main feature/x\nfatal: couldn't find remote ref feature/x\n",
			sourceBranch: "feature/x",
			want:         true,
		},
		{
			// A missing target ref is a real config error worth surfacing — must
			// not be swallowed as already-merged.
			name:         "missing target ref does not match",
			msg:          "fatal: couldn't find remote ref main",
			sourceBranch: "feature/x",
			want:         false,
		},
		{
			name:         "different missing ref does not match",
			msg:          "fatal: couldn't find remote ref some-other-branch",
			sourceBranch: "feature/x",
			want:         false,
		},
		{
			name:         "unrelated repo error does not match",
			msg:          "fatal: not a git repository",
			sourceBranch: "X",
			want:         false,
		},
		{
			name:         "conflict text does not match",
			msg:          "CONFLICT (content): Merge conflict",
			sourceBranch: "X",
			want:         false,
		},
		{
			name:         "empty input does not match",
			msg:          "",
			sourceBranch: "X",
			want:         false,
		},
		{
			name:         "handles slashed branch names",
			msg:          "fatal: couldn't find remote ref feature/nested-name",
			sourceBranch: "feature/nested-name",
			want:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMissingRemoteRefError(tt.msg, tt.sourceBranch); got != tt.want {
				t.Errorf("IsMissingRemoteRefError(%q,%q) = %v, want %v", tt.msg, tt.sourceBranch, got, tt.want)
			}
		})
	}
}
