package localgithub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

func TestPollPaginatesExactLabeledIssuesAndExcludesPRRows(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.mu.Lock()
	fixture.issues = make([]fixtureIssue, 0, 101)
	for number := 1; number <= 101; number++ {
		fixture.issues = append(fixture.issues, fixtureIssue{
			Number: number, Title: "Issue " + strconv.Itoa(number), Body: "authored", State: "open", Labels: []string{"donmai"},
		})
	}
	fixture.issues[17].PR = true
	fixture.issues[82].Labels = []string{"other"}
	fixture.mu.Unlock()
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	got, err := source.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 99 {
		t.Fatalf("paginated candidates = %d, want 99", len(got))
	}
	if got[0].IssueNumber != 1 || got[len(got)-1].IssueNumber != 101 {
		t.Fatal("poll lost the first or final pagination result")
	}
	for _, issue := range got {
		if issue.IssueNumber == 18 || issue.IssueNumber == 83 || issue.RepositoryID != 1234 ||
			issue.OwnerRepo != "acme/repo" || issue.IssueURL != issueURL(fixture.repository(), issue.IssueNumber) {
			t.Fatal("poll included a PR/wrong-label row or changed source identity")
		}
	}
	fixture.mu.Lock()
	paths := append([]string(nil), fixture.paths...)
	fixture.mu.Unlock()
	if !strings.Contains(strings.Join(paths, "\n"), "page=2") || !strings.Contains(strings.Join(paths, "\n"), "labels=donmai") ||
		!strings.Contains(strings.Join(paths, "\n"), "per_page=100") {
		t.Fatalf("poll did not issue bounded filtered second page: %v", paths)
	}
}

func TestPollAndEligibleRefuseImmutableRepoMismatchAndIssueDrift(t *testing.T) {
	fixture := newGitHubFixture(t)
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	if eligible, err := source.Eligible(context.Background(), fixtureIntake()); err != nil || !eligible {
		t.Fatalf("fresh eligible issue = %v, %v", eligible, err)
	}
	for _, mutate := range []func(*fixtureIssue){
		func(issue *fixtureIssue) { issue.Labels = []string{"other"} },
		func(issue *fixtureIssue) { issue.State = "closed" },
		func(issue *fixtureIssue) { issue.Title = "Changed title" },
		func(issue *fixtureIssue) { issue.Body = "Changed body" },
		func(issue *fixtureIssue) { issue.PR = true },
	} {
		fixture.mu.Lock()
		original := fixture.issues[0]
		mutate(&fixture.issues[0])
		fixture.mu.Unlock()
		eligible, err := source.Eligible(context.Background(), fixtureIntake())
		if err != nil || eligible {
			t.Fatalf("changed issue remained eligible: eligible=%v err=%v", eligible, err)
		}
		fixture.mu.Lock()
		fixture.issues[0] = original
		fixture.mu.Unlock()
	}
	fixture.mu.Lock()
	fixture.repositoryID = 4321
	fixture.mu.Unlock()
	if _, err := source.Poll(context.Background()); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("changed immutable repo ID was polled: %v", err)
	}
	if _, err := source.Eligible(context.Background(), fixtureIntake()); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("changed immutable repo ID was eligible: %v", err)
	}
	fixture.mu.Lock()
	fixture.repositoryID = 1234
	fixture.fullName = "acme/renamed"
	fixture.mu.Unlock()
	if _, err := source.Poll(context.Background()); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("renamed repo was polled: %v", err)
	}
}

func TestPollAndEligibleRefuseMissingOrRenamedConfiguredBranch(t *testing.T) {
	for _, tc := range []struct {
		name, branchName, branchSHA string
		gone                        bool
		want                        error
	}{
		{name: "branch-missing-tag-only", gone: true, want: ErrRemote},
		{name: "branch-renamed", branchName: "release", want: ErrScopeMismatch},
		{name: "branch-without-commit", branchSHA: "not-a-commit", want: ErrScopeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubFixture(t)
			fixture.mu.Lock()
			fixture.branchGone = tc.gone
			if tc.branchName != "" {
				fixture.branchName = tc.branchName
			}
			if tc.branchSHA != "" {
				fixture.branchSHA = tc.branchSHA
			}
			fixture.mu.Unlock()
			source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
			if got, err := source.Poll(context.Background()); !errors.Is(err, tc.want) || got != nil {
				t.Fatalf("unverified branch polled: count=%d err=%v", len(got), err)
			}
			if eligible, err := source.Eligible(context.Background(), fixtureIntake()); eligible || !errors.Is(err, tc.want) {
				t.Fatalf("unverified branch eligible: eligible=%t err=%v", eligible, err)
			}
			fixture.mu.Lock()
			paths := append([]string(nil), fixture.paths...)
			fixture.mu.Unlock()
			for _, path := range paths {
				if strings.Contains(path, "/issues") {
					t.Fatalf("issue read preceded branch verification: %v", paths)
				}
			}
		})
	}
}

func TestPollResultBudgetRefusesWithoutPartialCandidates(t *testing.T) {
	fixture := newGitHubFixture(t)
	fixture.mu.Lock()
	fixture.issues = make([]fixtureIssue, 130)
	for i := range fixture.issues {
		fixture.issues[i] = fixtureIssue{
			Number: i + 1, Title: "B", Body: strings.Repeat("x", maxIssueBody),
			State: "open", Labels: []string{"donmai"},
		}
	}
	fixture.mu.Unlock()
	source := fixture.source(t, filepath.Join(t.TempDir(), "publication"))
	got, err := source.Poll(context.Background())
	if !errors.Is(err, ErrRemote) || got != nil {
		t.Fatalf("oversized poll returned partial candidates: count=%d err=%v", len(got), err)
	}
}

func TestNewRejectsUnconfiguredOriginsAndCredentials(t *testing.T) {
	fixture := newGitHubFixture(t)
	base := Options{
		Repositories: []daemon.LocalGitHubRepository{fixture.repository()}, Token: "synthetic-token",
		HTTPClient: fixture.server.Client(), BaseURL: fixture.server.URL, PublicationRoot: filepath.Join(t.TempDir(), "publication"),
	}
	for _, bad := range []func(*Options){
		func(options *Options) { options.Token = "" },
		func(options *Options) { options.Token = "synthetic\nother" },
		func(options *Options) { options.Repositories[0].RepositoryID = 0 },
		func(options *Options) { options.Repositories[0].OwnerRepo = "../repo" },
		func(options *Options) { options.Repositories[0].OwnerRepo = "acme/.." },
		func(options *Options) {
			options.Repositories[0].OwnerRepo = "other/repo"
			options.Repositories[0].RepositoryID = -1
		},
		func(options *Options) { options.Repositories[0].Label = "" },
		func(options *Options) { options.BaseURL = "https://evil.example" },
		func(options *Options) { options.BaseURL = "http://localhost:1234" },
		func(options *Options) { options.BaseURL = "http://127.0.0.1:1234/path" },
		func(options *Options) { options.BaseURL = fixture.server.URL; options.HTTPClient = nil },
		func(options *Options) { options.PublicationRoot = "relative-root" },
	} {
		options := base
		options.Repositories = append([]daemon.LocalGitHubRepository(nil), base.Repositories...)
		bad(&options)
		if source, err := New(options); err == nil {
			_ = source.Close()
			t.Fatal("invalid source configuration was accepted")
		}
	}
}

func TestHTTPRedirectCannotCarryBearerToAnotherOrigin(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer first.Close()
	source, err := New(Options{
		Repositories: []daemon.LocalGitHubRepository{{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "donmai", Ref: "main"}},
		Token:        "synthetic-token", HTTPClient: first.Client(), BaseURL: first.URL,
		PublicationRoot: filepath.Join(t.TempDir(), "publication"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if _, err := source.Poll(context.Background()); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect followed or bearer sent to second origin: err=%v visits=%d", err, redirected.Load())
	}
}
