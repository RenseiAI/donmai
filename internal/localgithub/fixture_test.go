package localgithub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

type fixtureIssue struct {
	Number int
	Title  string
	Body   string
	State  string
	Labels []string
	PR     bool
}

type fixtureComment struct {
	ID           int64
	Body         string
	AuthorID     int64
	TargetNumber int
	IssueURL     string
	HTMLURL      string
}

type githubFixture struct {
	mu           sync.Mutex
	server       *httptest.Server
	repositoryID int64
	fullName     string
	branchName   string
	branchSHA    string
	branchGone   bool
	issues       []fixtureIssue
	comments     []fixtureComment
	actorID      int64
	prNumber     int
	prURL        string
	prHead       string
	prBranch     string
	prBaseRef    string
	prBaseID     int64
	prHeadID     int64
	prAfterPost  func(*githubFixture)
	prReads      int
	postCalls    int
	postTargets  []int
	userCalls    int
	paths        []string
	postMode     string // normal, lose-before, lose-after
}

func newGitHubFixture(t *testing.T) *githubFixture {
	t.Helper()
	f := &githubFixture{
		repositoryID: 1234, fullName: "acme/repo", branchName: "main", branchSHA: strings.Repeat("b", 40), actorID: 42,
		issues:   []fixtureIssue{{Number: 7, Title: "Fix fixture", Body: "Bounded authored task", State: "open", Labels: []string{"donmai"}}},
		prNumber: 9, prURL: "https://github.com/acme/repo/pull/9",
		prHead: strings.Repeat("a", 40), prBranch: "agent/session-seven", prBaseRef: "main", prBaseID: 1234, prHeadID: 1234,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			f.userCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.actorID})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.repositoryID, "full_name": f.fullName})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/repo/branches/"):
			if f.branchGone {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": f.branchName, "commit": map[string]any{"sha": f.branchSHA}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/issues":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			start := (page - 1) * itemsPerPage
			end := min(start+itemsPerPage, len(f.issues))
			rows := make([]any, 0)
			if start >= 0 && start < len(f.issues) {
				for _, issue := range f.issues[start:end] {
					rows = append(rows, fixtureIssueJSON(issue))
				}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodGet && fixtureCommentCollectionTarget(r.URL.Path) != 0:
			targetNumber := fixtureCommentCollectionTarget(r.URL.Path)
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			start := (page - 1) * itemsPerPage
			var matching []fixtureComment
			for _, comment := range f.comments {
				if fixtureCommentTarget(comment) == targetNumber {
					matching = append(matching, comment)
				}
			}
			end := min(start+itemsPerPage, len(matching))
			rows := make([]any, 0)
			if start >= 0 && start < len(matching) {
				for _, comment := range matching[start:end] {
					rows = append(rows, fixtureCommentJSON(comment))
				}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodPost && fixtureCommentCollectionTarget(r.URL.Path) != 0:
			targetNumber := fixtureCommentCollectionTarget(r.URL.Path)
			f.postCalls++
			f.postTargets = append(f.postTargets, targetNumber)
			var request struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f.postMode != "lose-before" {
				f.comments = append(f.comments, fixtureComment{ID: int64(100 + f.postCalls), Body: request.Body, AuthorID: f.actorID, TargetNumber: targetNumber})
				if targetNumber == f.prNumber && f.prAfterPost != nil {
					f.prAfterPost(f)
				}
			}
			if f.postMode == "lose-before" || f.postMode == "lose-after" {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(fixtureCommentJSON(f.comments[len(f.comments)-1]))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/repo/issues/comments/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/issues/comments/"), 10, 64)
			for _, comment := range f.comments {
				if comment.ID == id {
					_ = json.NewEncoder(w).Encode(fixtureCommentJSON(comment))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/issues/7":
			for _, issue := range f.issues {
				if issue.Number == 7 {
					_ = json.NewEncoder(w).Encode(fixtureIssueJSON(issue))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/pulls/9":
			f.prReads++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": f.prNumber, "html_url": f.prURL,
				"head": map[string]any{"sha": f.prHead, "ref": f.prBranch, "repo": map[string]any{"id": f.prHeadID}},
				"base": map[string]any{"ref": f.prBaseRef, "repo": map[string]any{"id": f.prBaseID}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func fixtureIssueJSON(issue fixtureIssue) map[string]any {
	labels := make([]map[string]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, map[string]string{"name": label})
	}
	row := map[string]any{
		"number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State,
		"html_url": "https://github.com/acme/repo/issues/" + strconv.Itoa(issue.Number), "labels": labels,
	}
	if issue.PR {
		row["pull_request"] = map[string]any{"url": "synthetic"}
	}
	return row
}

func fixtureCommentJSON(comment fixtureComment) map[string]any {
	target := fixtureCommentTarget(comment)
	htmlURL := "https://github.com/acme/repo/issues/" + strconv.Itoa(target) + "#issuecomment-" + strconv.FormatInt(comment.ID, 10)
	if target == 9 {
		htmlURL = "https://github.com/acme/repo/pull/" + strconv.Itoa(target) + "#issuecomment-" + strconv.FormatInt(comment.ID, 10)
	}
	if comment.HTMLURL != "" {
		htmlURL = comment.HTMLURL
	}
	issueAPIURL := "https://api.github.com/repos/acme/repo/issues/" + strconv.Itoa(target)
	if comment.IssueURL != "" {
		issueAPIURL = comment.IssueURL
	}
	return map[string]any{
		"id": comment.ID, "body": comment.Body,
		"html_url": htmlURL, "issue_url": issueAPIURL,
		"user": map[string]any{"id": comment.AuthorID},
	}
}

func fixtureCommentTarget(comment fixtureComment) int {
	if comment.TargetNumber == 0 {
		return 7
	}
	return comment.TargetNumber
}

func fixtureCommentCollectionTarget(path string) int {
	const prefix = "/repos/acme/repo/issues/"
	const suffix = "/comments"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	if err != nil || number != 7 && number != 9 {
		return 0
	}
	return number
}

func (f *githubFixture) repository() daemon.LocalGitHubRepository {
	return daemon.LocalGitHubRepository{RepositoryID: 1234, OwnerRepo: "acme/repo", Label: "donmai", Ref: "main"}
}

func (f *githubFixture) source(t *testing.T, publicationRoot string) *Source {
	t.Helper()
	source, err := New(Options{
		Repositories: []daemon.LocalGitHubRepository{f.repository()}, Token: "synthetic-token",
		HTTPClient: f.server.Client(), BaseURL: f.server.URL, PublicationRoot: publicationRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func fixtureIntake() daemon.LocalIntakeRequest {
	return daemon.LocalIntakeRequest{
		RepositoryID: 1234, OwnerRepo: "acme/repo", IssueNumber: 7,
		IssueURL: "https://github.com/acme/repo/issues/7", Title: "Fix fixture", Body: "Bounded authored task",
	}
}

func fixtureTask(kind string) daemon.LocalPublicationTask {
	task := daemon.LocalPublicationTask{
		Key: "publish:session-seven:result", Kind: kind, SessionID: "session-seven",
		Source: daemon.LocalIntakeRequest{RepositoryID: 1234, OwnerRepo: "acme/repo", IssueNumber: 7, IssueURL: "https://github.com/acme/repo/issues/7"},
		Status: "failed", ResultSHA256: strings.Repeat("b", 64),
		PullRequestURL: "https://github.com/acme/repo/pull/9", CommitSHA: strings.Repeat("a", 40), Branch: "agent/session-seven",
	}
	if kind == publicationPR {
		task.BaseRef = "main"
	}
	return task
}
