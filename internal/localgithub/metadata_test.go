package localgithub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveRepositoryReadsCanonicalIdentityAndDefaultRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/repo" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Errorf("unexpected metadata request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":1234,"full_name":"Acme/Repo","default_branch":"main"}`))
	}))
	defer server.Close()
	got, err := ResolveRepository(context.Background(), "acme/repo", Options{Token: "synthetic-token", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil || got.RepositoryID != 1234 || got.OwnerRepo != "Acme/Repo" || got.Ref != "main" {
		t.Fatalf("repository metadata = %+v, %v", got, err)
	}
}

func TestResolveRepositoryRejectsNamesRedirectsAndOversize(t *testing.T) {
	requests := 0
	redirects := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirects++; w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/repos/acme/redirect":
			http.Redirect(w, r, destination.URL, http.StatusFound)
		case "/repos/acme/oversize":
			_, _ = fmt.Fprint(w, strings.Repeat("x", maxAPIResponse+1))
		case "/repos/acme/mismatch":
			_, _ = w.Write([]byte(`{"id":1234,"full_name":"other/repo","default_branch":"main"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	opts := Options{Token: "synthetic-token", BaseURL: server.URL, HTTPClient: server.Client()}
	for _, name := range []string{"acme/../secret", "acme/repo?x=1", "https://evil.invalid/repo", "acme/%2f"} {
		if _, err := ResolveRepository(context.Background(), name, opts); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("malicious name %q accepted: %v", name, err)
		}
	}
	if requests != 0 {
		t.Fatalf("malicious names made %d HTTP requests", requests)
	}
	for _, name := range []string{"acme/redirect", "acme/oversize", "acme/mismatch"} {
		if _, err := ResolveRepository(context.Background(), name, opts); err == nil || strings.Contains(err.Error(), "synthetic-token") {
			t.Fatalf("unsafe metadata accepted or token exposed for %q: %v", name, err)
		}
	}
	if redirects != 0 {
		t.Fatalf("redirect delivered bearer to destination %d times", redirects)
	}
}

func TestVerifyRepositoryBranchRequiresExactImmutableRepositoryAndNamedBranch(t *testing.T) {
	fixture := newGitHubFixture(t)
	opts := Options{Token: "synthetic-token", BaseURL: fixture.server.URL, HTTPClient: fixture.server.Client()}
	repository := fixture.repository()
	if err := VerifyRepositoryBranch(context.Background(), repository, opts); err != nil {
		t.Fatalf("valid configured branch: %v", err)
	}
	fixture.mu.Lock()
	paths := append([]string(nil), fixture.paths...)
	fixture.paths = nil
	fixture.mu.Unlock()
	if len(paths) != 2 || paths[0] != "GET /repos/acme/repo" || paths[1] != "GET /repos/acme/repo/branches/main" {
		t.Fatalf("branch verification request sequence = %v", paths)
	}
	for _, tc := range []struct {
		name   string
		change func()
		want   error
	}{
		{name: "tag-only-no-branch", want: ErrRemote, change: func() { fixture.branchGone = true }},
		{name: "renamed-branch", want: ErrScopeMismatch, change: func() { fixture.branchName = "release" }},
		{name: "missing-commit", want: ErrScopeMismatch, change: func() { fixture.branchSHA = "" }},
		{name: "repository-id-drift", want: ErrScopeMismatch, change: func() { fixture.repositoryID = 4321 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.mu.Lock()
			fixture.branchGone = false
			fixture.branchName = "main"
			fixture.branchSHA = strings.Repeat("b", 40)
			fixture.repositoryID = 1234
			tc.change()
			fixture.paths = nil
			fixture.mu.Unlock()
			if err := VerifyRepositoryBranch(context.Background(), repository, opts); !errors.Is(err, tc.want) {
				t.Fatalf("unverified branch accepted: %v, want %v", err, tc.want)
			}
		})
	}
	fixture.mu.Lock()
	fixture.branchGone = false
	fixture.branchName = "feature/new"
	fixture.repositoryID = 1234
	fixture.branchSHA = strings.Repeat("b", 40)
	fixture.paths = nil
	fixture.mu.Unlock()
	repository.Ref = "feature/new"
	if err := VerifyRepositoryBranch(context.Background(), repository, opts); err != nil {
		t.Fatalf("slash-named branch: %v", err)
	}
	fixture.mu.Lock()
	paths = append([]string(nil), fixture.paths...)
	fixture.mu.Unlock()
	if len(paths) != 2 || paths[1] != "GET /repos/acme/repo/branches/feature%2Fnew" {
		t.Fatalf("slash branch was not escaped exactly: %v", paths)
	}
}
