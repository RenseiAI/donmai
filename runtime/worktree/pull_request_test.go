package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// pullRequestFixture is a bare origin shaped like a forge serving an open pull
// request. Its defining property is that the pull request's head commit is
// reachable ONLY through refs/pull/7/head: the contributing branch is deleted
// after the bare repo is built, so a plain `git clone` — which fetches
// refs/heads/* — cannot produce that commit. Every "was the head actually
// fetched?" assertion below rests on that, and so does the control case: if the
// commit turns up in a workarea provisioned without a pull-request record, the
// fixture, not the code, is what proved it.
//
// Likewise baseSHA lives on a `release` branch that is not an ancestor of
// main, so a single-branch clone of main genuinely lacks it.
type pullRequestFixture struct {
	// origin is a file:// URL, never a bare path. Cloning a local PATH
	// short-circuits the transport and hardlinks the entire object store, so
	// every commit — including the refs/pull/* head this fixture exists to
	// withhold — would arrive without anyone fetching it, and both the control
	// case and the base-absent cases would assert nothing.
	origin    string
	owner     string
	repo      string
	mainSHA   string
	prHeadSHA string
	baseSHA   string
}

// bind returns a record already bound to the fixture's repository, so each
// case below can vary only the field it is actually about.
func (f pullRequestFixture) bind(pr *workarea.PullRequestV1) *workarea.PullRequestV1 {
	pr.Owner, pr.Repo = f.owner, f.repo
	return pr
}

func newPullRequestFixture(t *testing.T) pullRequestFixture {
	t.Helper()
	requireGit(t)

	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "origin.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		//nolint:gosec // test fixture uses the git binary selected by PATH.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(name, body string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git(work, "add", name)
		git(work, "commit", "-m", "add "+name)
		return git(work, "rev-parse", "HEAD")
	}

	git(work, "init", "-b", "main")
	git(work, "config", "user.email", "test@example.com")
	git(work, "config", "user.name", "test")
	mainSHA := commit("README.md", "base")

	git(work, "checkout", "-b", "contributor")
	prHeadSHA := commit("feature.txt", "the change under review")

	git(work, "checkout", "main")
	git(work, "checkout", "-b", "release")
	baseSHA := commit("release.txt", "a commit main cannot reach")

	git(work, "checkout", "main")
	git(work, "clone", "--bare", work, bare)
	// Publish the head the way a forge does — a read-only refs/pull/<n>/head —
	// and remove the branch, so nothing but that ref can supply the commit.
	git(bare, "update-ref", "refs/pull/7/head", prHeadSHA)
	git(bare, "branch", "-D", "contributor")

	origin := "file://" + bare
	owner, repo, ok := workarea.RepositoryIdentity(origin)
	if !ok {
		t.Fatalf("fixture origin %q has no owner/name identity", origin)
	}
	return pullRequestFixture{
		origin: origin, owner: owner, repo: repo,
		mainSHA: mainSHA, prHeadSHA: prHeadSHA, baseSHA: baseSHA,
	}
}

// recordingRunner delegates to the real git binary while keeping every argv, so
// a test can assert what was NOT run — the control case's whole claim.
type recordingRunner struct {
	mu   sync.Mutex
	args [][]string
}

func (r *recordingRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.args = append(r.args, append([]string(nil), args...))
	r.mu.Unlock()
	//nolint:gosec // test runner executes the git binary selected by PATH.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (r *recordingRunner) sawArgContaining(fragment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, argv := range r.args {
		for _, a := range argv {
			if strings.Contains(a, fragment) {
				return true
			}
		}
	}
	return false
}

// gitQuery runs a read-only git query inside a provisioned workarea, returning
// the error rather than failing the test: several assertions below are about a
// lookup that must FAIL.
func gitQuery(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	//nolint:gosec // test helper uses the git binary selected by PATH.
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestProvisionFetchesDispatchedPullRequestHead is the end-to-end contract: a
// session dispatched with a pull-request record gets that pull request's head
// in its workarea, or the session fails with a reason that says why. Each case
// runs a real `git clone` through Provision, so the assertion covers the wiring
// (clone step → fetch → verify), not just the helper.
func TestProvisionFetchesDispatchedPullRequestHead(t *testing.T) {
	t.Parallel()
	fixture := newPullRequestFixture(t)

	tests := []struct {
		name string
		// record builds the dispatched pull request; nil means "no record",
		// the control.
		record func(f pullRequestFixture) *workarea.PullRequestV1
		// wantErr is the sentinel the session's failure must classify as.
		wantErr error
		// wantReason are substrings the stated failure reason must contain —
		// a mismatch is only actionable if it names both commits.
		wantReason []string
		// wantLocalRefAt is the commit the agent-facing ref must resolve to.
		wantLocalRefAt func(f pullRequestFixture) string
		// wantHeadAbsent asserts the pull request's commit never entered the
		// clone — the control's proof that nothing was fetched.
		wantHeadAbsent bool
	}{
		{
			name: "record present fetches the head and verifies it",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				return f.bind(&workarea.PullRequestV1{
					Number:  7,
					URL:     "https://example.test/acme/widget/pull/7",
					HeadSHA: f.prHeadSHA, HeadRef: "contributor",
					BaseSHA: f.mainSHA, BaseRef: "main",
					Title: "the change under review", State: "open",
					LocalRef: "refs/pull/7/head",
				})
			},
			wantLocalRefAt: func(f pullRequestFixture) string { return f.prHeadSHA },
		},
		{
			name: "empty localRef defaults to the conventional ref name",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				return f.bind(&workarea.PullRequestV1{Number: 7, HeadSHA: f.prHeadSHA})
			},
			wantLocalRefAt: func(f pullRequestFixture) string { return f.prHeadSHA },
		},
		{
			name: "head moved after dispatch fails naming both commits",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				// The recorded head is a real commit — just not the one the
				// ref points at any more, which is exactly what a push or a
				// force-push between dispatch and provisioning produces.
				return f.bind(&workarea.PullRequestV1{Number: 7, HeadSHA: f.mainSHA, LocalRef: "refs/pull/7/head"})
			},
			wantErr:    worktree.ErrPullRequestHeadMismatch,
			wantReason: []string{"head moved after dispatch", "refs/pull/7/head"},
		},
		{
			name: "missing pull request ref fails as a fetch failure",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				return f.bind(&workarea.PullRequestV1{Number: 99, HeadSHA: f.prHeadSHA})
			},
			wantErr:    worktree.ErrPullRequestFetch,
			wantReason: []string{"pull request #99", "refs/pull/99/head"},
		},
		{
			name: "malformed record is refused before any git invocation",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				return f.bind(&workarea.PullRequestV1{Number: 7, HeadSHA: "not-a-sha"})
			},
			wantErr:        worktree.ErrPullRequestFetch,
			wantReason:     []string{"headSha"},
			wantHeadAbsent: true,
		},
		{
			name: "record without an owner or repo cannot be bound and is refused",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				return &workarea.PullRequestV1{Number: 7, HeadSHA: f.prHeadSHA}
			},
			wantErr:        worktree.ErrPullRequestFetch,
			wantReason:     []string{"owner and repo are required"},
			wantHeadAbsent: true,
		},
		{
			name: "record naming another repository is refused before any fetch",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				pr := f.bind(&workarea.PullRequestV1{Number: 7, HeadSHA: f.prHeadSHA})
				pr.Owner = "somebody-else"
				return pr
			},
			wantErr:        worktree.ErrPullRequestRepositoryMismatch,
			wantReason:     []string{"names a different repository", "somebody-else"},
			wantHeadAbsent: true,
		},
		{
			name: "record naming another repository name is refused before any fetch",
			record: func(f pullRequestFixture) *workarea.PullRequestV1 {
				pr := f.bind(&workarea.PullRequestV1{Number: 7, HeadSHA: f.prHeadSHA})
				pr.Repo = "a-different-repo"
				return pr
			},
			wantErr:        worktree.ErrPullRequestRepositoryMismatch,
			wantReason:     []string{"names a different repository", "a-different-repo"},
			wantHeadAbsent: true,
		},
		{
			name:           "no record leaves the clone untouched",
			record:         func(pullRequestFixture) *workarea.PullRequestV1 { return nil },
			wantHeadAbsent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runner := &recordingRunner{}
			m, err := worktree.NewManager(worktree.Options{
				ParentDir:     t.TempDir(),
				CommandRunner: runner.run,
				RetryDelay:    time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			spec := worktree.ProvisionSpec{
				SessionID:   "sess-pr",
				RepoURL:     fixture.origin,
				Branch:      "main",
				Strategy:    worktree.StrategyClone,
				PullRequest: tt.record(fixture),
			}
			path, err := m.Provision(context.Background(), spec)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("Provision succeeded at %s, want %v", path, tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Provision error = %v, want errors.Is %v", err, tt.wantErr)
				}
				// The runner copies err.Error() verbatim into the session's
				// stated failure reason, so the reason IS this string.
				for _, want := range tt.wantReason {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("failure reason %q does not mention %q", err.Error(), want)
					}
				}
				if tt.wantErr == worktree.ErrPullRequestHeadMismatch {
					// Both commits, so an operator can see what moved.
					for _, sha := range []string{fixture.mainSHA, fixture.prHeadSHA} {
						if !strings.Contains(err.Error(), sha) {
							t.Errorf("mismatch reason %q does not name commit %s", err.Error(), sha)
						}
					}
				}
				if tt.wantHeadAbsent && runner.sawArgContaining("refs/pull/") {
					t.Error("a git invocation referenced refs/pull/ despite the record being unusable")
				}
				return
			}

			if err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if tt.wantHeadAbsent {
				if runner.sawArgContaining("refs/pull/") {
					t.Error("clone step touched refs/pull/ with no pull-request record on the spec")
				}
				if out, err := gitQuery(t, path, "cat-file", "-e", fixture.prHeadSHA+"^{commit}"); err == nil {
					t.Errorf("pull request head %s present in a workarea provisioned without a record (%s)", fixture.prHeadSHA, out)
				}
				return
			}
			want := tt.wantLocalRefAt(fixture)
			got, err := gitQuery(t, path, "rev-parse", "--verify", spec.PullRequest.ResolvedLocalRef()+"^{commit}")
			if err != nil {
				t.Fatalf("resolve %s in workarea: %v (%s)", spec.PullRequest.ResolvedLocalRef(), err, got)
			}
			if got != want {
				t.Errorf("%s = %s, want %s", spec.PullRequest.ResolvedLocalRef(), got, want)
			}
		})
	}
}

// TestPullRequestBaseCommitMustBeReachable covers the base-commit half, which a
// Provision-level test cannot reach: `git clone` fetches every head, so a
// provisioned workarea always already holds the base. These cases clone
// single-branch by hand to make the absence real, and assert the absence is
// REFUSED rather than repaired — fetching the base branch's current tip
// instead would substitute a different comparison point while looking like
// success.
func TestPullRequestBaseCommitMustBeReachable(t *testing.T) {
	t.Parallel()
	fixture := newPullRequestFixture(t)

	tests := []struct {
		name string
		// baseSHA is the commit the dispatch recorded as the base.
		baseSHA func(f pullRequestFixture) string
		baseRef string
		wantErr error
		// wantReason are substrings the stated failure reason must contain.
		wantReason []string
	}{
		{
			name:    "base commit reachable in the clone provisions",
			baseSHA: func(f pullRequestFixture) string { return f.mainSHA },
			baseRef: "main",
		},
		{
			name:    "no base commit recorded provisions",
			baseSHA: func(pullRequestFixture) string { return "" },
		},
		{
			name:       "unreachable base commit fails naming the commit",
			baseSHA:    func(f pullRequestFixture) string { return f.baseSHA },
			baseRef:    "release",
			wantErr:    worktree.ErrPullRequestFetch,
			wantReason: []string{"not reachable in the clone", "baseRef release"},
		},
		{
			name:       "unreachable base commit with no base branch says so",
			baseSHA:    func(f pullRequestFixture) string { return f.baseSHA },
			baseRef:    "",
			wantErr:    worktree.ErrPullRequestFetch,
			wantReason: []string{"not reachable in the clone", "no baseRef recorded"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dst := filepath.Join(t.TempDir(), "clone")
			//nolint:gosec // test fixture uses the git binary selected by PATH.
			out, err := exec.Command("git", "clone", "--single-branch", "--branch", "main", fixture.origin, dst).CombinedOutput()
			if err != nil {
				t.Fatalf("clone single-branch: %v\n%s", err, out)
			}
			// Precondition: the "unreachable" commit really is absent, and the
			// "reachable" one really is present. Without this the cases would
			// assert nothing about a clone that happened to hold both.
			if _, err := gitQuery(t, dst, "cat-file", "-e", fixture.baseSHA+"^{commit}"); err == nil {
				t.Fatalf("fixture broken: %s already present in a single-branch clone of main", fixture.baseSHA)
			}

			runner := &recordingRunner{}
			m, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir(), CommandRunner: runner.run})
			if err != nil {
				t.Fatal(err)
			}
			spec := worktree.ProvisionSpec{
				SessionID: "sess-base",
				RepoURL:   fixture.origin,
				Strategy:  worktree.StrategyClone,
				PullRequest: fixture.bind(&workarea.PullRequestV1{
					Number: 7, HeadSHA: fixture.prHeadSHA,
					BaseSHA: tt.baseSHA(fixture), BaseRef: tt.baseRef,
				}),
			}
			err = m.FetchPullRequestHead(context.Background(), dst, spec)

			// Whatever the outcome, the base branch is never fetched: the only
			// ref this step is allowed to bring in is the pull request head.
			if runner.sawArgContaining("release") || runner.sawArgContaining("refs/heads/") {
				t.Error("the base branch was fetched; the base must be required, never repaired")
			}

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("FetchPullRequestHead succeeded, want %v", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want errors.Is %v", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.baseSHA(fixture)) {
					t.Errorf("failure reason %q does not name the base commit %s", err.Error(), tt.baseSHA(fixture))
				}
				for _, want := range tt.wantReason {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("failure reason %q does not mention %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchPullRequestHead: %v", err)
			}
		})
	}
}

// TestPullRequestRecordBindsToRepository pins the identity check that runs
// before any git command. A record is only actionable against the repository
// it names, and the comparison has to survive the several spellings a work
// source may use for one repository without ever treating an unreadable
// spelling as a match.
func TestPullRequestRecordBindsToRepository(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		owner      string
		repo       string
		repository string
		wantErr    bool
	}{
		{name: "https url", owner: "acme", repo: "widget", repository: "https://github.test/acme/widget.git"},
		{name: "https url without .git", owner: "acme", repo: "widget", repository: "https://github.test/acme/widget"},
		{name: "https url with userinfo", owner: "acme", repo: "widget", repository: "https://x-token:secret@github.test/acme/widget.git"},
		{name: "scp style ssh remote", owner: "acme", repo: "widget", repository: "git@github.test:acme/widget.git"},
		{name: "bare slug", owner: "acme", repo: "widget", repository: "acme/widget"},
		{name: "case differs only", owner: "ACME", repo: "Widget", repository: "https://github.test/acme/widget.git"},
		{name: "trailing slash", owner: "acme", repo: "widget", repository: "https://github.test/acme/widget/"},
		{name: "nested path keeps the last two segments", owner: "group", repo: "widget", repository: "https://forge.test/org/group/widget.git"},
		{name: "different owner", owner: "evil", repo: "widget", repository: "https://github.test/acme/widget.git", wantErr: true},
		{name: "different repo", owner: "acme", repo: "other", repository: "https://github.test/acme/widget.git", wantErr: true},
		{name: "both different", owner: "evil", repo: "other", repository: "https://github.test/acme/widget.git", wantErr: true},
		{name: "single segment repository cannot be verified", owner: "acme", repo: "widget", repository: "widget", wantErr: true},
		{name: "empty repository cannot be verified", owner: "acme", repo: "widget", repository: "", wantErr: true},
		{name: "record with no owner cannot match", owner: "", repo: "widget", repository: "https://github.test/acme/widget.git", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record := &workarea.PullRequestV1{Owner: tt.owner, Repo: tt.repo, Number: 7}
			err := record.BindsToRepository(tt.repository)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BindsToRepository(%q) = nil, want an error", tt.repository)
				}
				if !errors.Is(err, workarea.ErrPullRequestRepositoryMismatch) {
					t.Errorf("BindsToRepository(%q) = %v, want errors.Is ErrPullRequestRepositoryMismatch", tt.repository, err)
				}
				return
			}
			if err != nil {
				t.Errorf("BindsToRepository(%q) = %v, want nil", tt.repository, err)
			}
		})
	}
}

// TestPullRequestRecordValidation pins the deterministic input checks. They run
// before any git invocation, so a malformed record can never reach a refspec.
func TestPullRequestRecordValidation(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	// bound is the minimum a record needs to be actionable at all.
	bound := func(pr workarea.PullRequestV1) workarea.PullRequestV1 {
		if pr.Owner == "" && pr.Repo == "" {
			pr.Owner, pr.Repo = "acme", "widget"
		}
		return pr
	}

	tests := []struct {
		name    string
		record  workarea.PullRequestV1
		wantErr bool
	}{
		{name: "missing owner", record: workarea.PullRequestV1{Repo: "widget", Number: 7, HeadSHA: sha}, wantErr: true},
		{name: "missing repo", record: workarea.PullRequestV1{Owner: "acme", Number: 7, HeadSHA: sha}, wantErr: true},
		{name: "minimal record", record: workarea.PullRequestV1{Number: 7, HeadSHA: sha}},
		{name: "sha-256 head", record: workarea.PullRequestV1{Number: 7, HeadSHA: strings.Repeat("a", 64)}},
		{name: "zero number", record: workarea.PullRequestV1{HeadSHA: sha}, wantErr: true},
		{name: "negative number", record: workarea.PullRequestV1{Number: -1, HeadSHA: sha}, wantErr: true},
		{name: "missing head", record: workarea.PullRequestV1{Number: 7}, wantErr: true},
		{name: "abbreviated head", record: workarea.PullRequestV1{Number: 7, HeadSHA: sha[:12]}, wantErr: true},
		{name: "non-hex head", record: workarea.PullRequestV1{Number: 7, HeadSHA: strings.Repeat("z", 40)}, wantErr: true},
		{
			name:    "unqualified local ref",
			record:  workarea.PullRequestV1{Number: 7, HeadSHA: sha, LocalRef: "pull/7/head"},
			wantErr: true,
		},
		{
			name:    "option-shaped local ref",
			record:  workarea.PullRequestV1{Number: 7, HeadSHA: sha, LocalRef: "--upload-pack=touch /tmp/pwn"},
			wantErr: true,
		},
		{
			name:    "second refspec component in local ref",
			record:  workarea.PullRequestV1{Number: 7, HeadSHA: sha, LocalRef: "refs/pull/7/head:refs/heads/main"},
			wantErr: true,
		},
		{
			name:    "path escape in local ref",
			record:  workarea.PullRequestV1{Number: 7, HeadSHA: sha, LocalRef: "refs/../../etc/passwd"},
			wantErr: true,
		},
		{
			name:    "malformed base sha",
			record:  workarea.PullRequestV1{Number: 7, HeadSHA: sha, BaseSHA: "nope"},
			wantErr: true,
		},
		{
			// baseRef is never fetched, only reported, so its spelling cannot
			// reach a refspec and is not constrained.
			name:   "arbitrary base ref is accepted because it is never fetched",
			record: workarea.PullRequestV1{Number: 7, HeadSHA: sha, BaseSHA: sha, BaseRef: "--upload-pack=x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record := bound(tt.record)
			err := record.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want an error")
				}
				if !errors.Is(err, workarea.ErrPullRequestInvalid) {
					t.Errorf("Validate() = %v, want errors.Is ErrPullRequestInvalid", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestPullRequestRecordIsAbsentByDefault pins the wire tolerance the platform
// contract depends on: a work item that carries no pullRequest object decodes
// to a nil record, and a nil record re-encodes to nothing at all. A daemon and
// a platform on different versions therefore stay byte-compatible.
func TestPullRequestRecordIsAbsentByDefault(t *testing.T) {
	t.Parallel()
	var record *workarea.PullRequestV1
	if err := record.Validate(); err != nil {
		t.Errorf("Validate() on a nil record = %v, want nil", err)
	}
	if got := (&workarea.PullRequestV1{Number: 7}).ResolvedLocalRef(); got != "refs/pull/7/head" {
		t.Errorf("ResolvedLocalRef() = %q, want refs/pull/7/head", got)
	}
	if got := (&workarea.PullRequestV1{Number: 7}).RemoteHeadRef(); got != "refs/pull/7/head" {
		t.Errorf("RemoteHeadRef() = %q, want refs/pull/7/head", got)
	}
}
