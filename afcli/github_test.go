package afcli

// Tests for the `af github` subcommand tree.
//
// Each test spins up an httptest.Server that serves a canned REST response,
// sets GITHUB_TOKEN to a dummy fixture key, points the github.Client at the
// test server URL, and asserts that the command produces the expected JSON on
// stdout.
//
// We exercise the command layer (flag wiring, env resolution, JSON shaping)
// rather than the REST client itself.
//
// NOTE: Tests that call t.Setenv() MUST NOT call t.Parallel() — the Go runtime
// panics if both are used together (t.Setenv modifies process-global env).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// setupGitHubTest sets GITHUB_TOKEN to a fixture value and overrides the
// client base URL to point at the test server.
// Must NOT be called from a parallel test.
func setupGitHubTest(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	t.Setenv("GITHUB_TOKEN", "ghp_test_fixture_not_a_secret") //nolint:gosec // dummy fixture
	t.Setenv("GITHUB_OWNER", "test-owner")
	t.Setenv("GITHUB_REPO", "test-repo")

	setGitHubTestBaseURL(srv.URL)
	t.Cleanup(func() { setGitHubTestBaseURL("") })

	return srv
}

// runGitHubCmd builds the `github <sub>` cobra tree, executes args,
// and returns captured stdout + any error.
func runGitHubCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newGitHubCmd(nil)
	root.SilenceErrors = true

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// issueResponseJSON returns a minimal GitHub issue REST JSON blob.
func issueResponseJSON(number int, title, state string) string {
	return fmt.Sprintf(`{
"number":%d,
"title":%q,
"body":"issue body",
"state":%q,
"html_url":"https://github.com/test-owner/test-repo/issues/%d",
"created_at":"2025-01-01T00:00:00Z",
"updated_at":"2025-01-02T00:00:00Z",
"labels":[{"id":1,"name":"bug","color":"d73a4a","description":""}],
"assignees":[{"login":"alice","name":"Alice","email":"","html_url":"","avatar_url":""}],
"user":{"login":"bob","name":"Bob","email":"","html_url":"","avatar_url":""},
"milestone":null,
"comments":3
}`, number, title, state, number)
}

// commentResponseJSON returns a minimal GitHub comment REST JSON blob.
func commentResponseJSON(id int64, body string) string {
	now := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(`{
"id":%d,
"body":%q,
"html_url":"https://github.com/test-owner/test-repo/issues/1#issuecomment-%d",
"created_at":%q,
"updated_at":%q,
"user":{"login":"alice","name":"Alice","email":"","html_url":"","avatar_url":""}
}`, id, body, id, now, now)
}

// ─── get-issue ────────────────────────────────────────────────────────────────

func TestGitHubGetIssue(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueResponseJSON(42, "Test Issue", "open"))
	})

	out, err := runGitHubCmd(t, "get-issue", "--number", "42")
	if err != nil {
		t.Fatalf("get-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["number"] != float64(42) {
		t.Errorf("number = %v, want 42", result["number"])
	}
	if result["title"] != "Test Issue" {
		t.Errorf("title = %v, want Test Issue", result["title"])
	}
	if result["state"] != "open" {
		t.Errorf("state = %v, want open", result["state"])
	}
	labels, ok := result["labels"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "bug" {
		t.Errorf("labels = %v, want [bug]", result["labels"])
	}
}

func TestGitHubGetIssueMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "get-issue")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

func TestGitHubGetIssueNotFound(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com"}`)
	})

	_, err := runGitHubCmd(t, "get-issue", "--number", "9999")
	if err == nil {
		t.Fatal("expected error for not-found issue")
	}
}

// ─── create-issue ─────────────────────────────────────────────────────────────

func TestGitHubCreateIssue(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, issueResponseJSON(101, "New Issue", "open"))
	})

	out, err := runGitHubCmd(t, "create-issue",
		"--title", "New Issue",
		"--body", "This is the body",
		"--labels", "bug",
	)
	if err != nil {
		t.Fatalf("create-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["title"] != "New Issue" {
		t.Errorf("title = %v, want New Issue", result["title"])
	}
	if result["number"] != float64(101) {
		t.Errorf("number = %v, want 101", result["number"])
	}
}

func TestGitHubCreateIssueMissingTitle(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "create-issue", "--body", "some body")
	if err == nil {
		t.Fatal("expected error when --title is missing")
	}
}

func TestGitHubCreateIssueBodyFile(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "body-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString("Body from file"); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, issueResponseJSON(102, "File Body Issue", "open"))
	})

	out, err := runGitHubCmd(t, "create-issue",
		"--title", "File Body Issue",
		"--body-file", tmpFile.Name(),
	)
	if err != nil {
		t.Fatalf("create-issue with --body-file failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if result["number"] != float64(102) {
		t.Errorf("number = %v, want 102", result["number"])
	}
}

// ─── update-issue ─────────────────────────────────────────────────────────────

func TestGitHubUpdateIssue(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueResponseJSON(42, "Updated Title", "open"))
	})

	out, err := runGitHubCmd(t, "update-issue",
		"--number", "42",
		"--title", "Updated Title",
	)
	if err != nil {
		t.Fatalf("update-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["title"] != "Updated Title" {
		t.Errorf("title = %v, want Updated Title", result["title"])
	}
}

func TestGitHubUpdateIssueMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "update-issue", "--title", "X")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

// ─── list-issues ──────────────────────────────────────────────────────────────

func TestGitHubListIssues(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[%s,%s]",
			issueResponseJSON(1, "First", "open"),
			issueResponseJSON(2, "Second", "open"),
		)
	})

	out, err := runGitHubCmd(t, "list-issues", "--state", "open", "--limit", "10")
	if err != nil {
		t.Fatalf("list-issues failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 2 {
		t.Fatalf("got %d issues, want 2", len(arr))
	}
	first := arr[0].(map[string]any)
	if first["title"] != "First" {
		t.Errorf("first title = %v, want First", first["title"])
	}
}

func TestGitHubListIssuesMissingOwner(t *testing.T) {
	// Clear env vars to trigger owner-missing error.
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	t.Setenv("GITHUB_OWNER", "")
	t.Setenv("GITHUB_REPO", "test-repo")
	setGitHubTestBaseURL("http://localhost:9") // won't be reached
	t.Cleanup(func() { setGitHubTestBaseURL("") })

	_, err := runGitHubCmd(t, "list-issues")
	if err == nil {
		t.Fatal("expected error when owner is missing")
	}
}

// ─── list-comments ────────────────────────────────────────────────────────────

func TestGitHubListComments(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[%s,%s]",
			commentResponseJSON(1001, "First comment"),
			commentResponseJSON(1002, "Second comment"),
		)
	})

	out, err := runGitHubCmd(t, "list-comments", "--number", "42")
	if err != nil {
		t.Fatalf("list-comments failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 2 {
		t.Fatalf("got %d comments, want 2", len(arr))
	}
	c0 := arr[0].(map[string]any)
	if c0["body"] != "First comment" {
		t.Errorf("comment[0].body = %v, want First comment", c0["body"])
	}
	if c0["author"] != "alice" {
		t.Errorf("comment[0].author = %v, want alice", c0["author"])
	}
}

func TestGitHubListCommentsMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "list-comments")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

// ─── create-comment ───────────────────────────────────────────────────────────

func TestGitHubCreateComment(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, commentResponseJSON(2001, "My comment"))
	})

	out, err := runGitHubCmd(t, "create-comment",
		"--number", "42",
		"--body", "My comment",
	)
	if err != nil {
		t.Fatalf("create-comment failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["body"] != "My comment" {
		t.Errorf("body = %v, want My comment", result["body"])
	}
	if result["id"] != float64(2001) {
		t.Errorf("id = %v, want 2001", result["id"])
	}
}

func TestGitHubCreateCommentMissingBody(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "create-comment", "--number", "42")
	if err == nil {
		t.Fatal("expected error when --body is missing")
	}
}

func TestGitHubCreateCommentMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "create-comment", "--body", "hello")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

func TestGitHubCreateCommentBodyFile(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "comment-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString("Comment from file"); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	setupGitHubTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, commentResponseJSON(2002, "Comment from file"))
	})

	out, err := runGitHubCmd(t, "create-comment",
		"--number", "42",
		"--body-file", tmpFile.Name(),
	)
	if err != nil {
		t.Fatalf("create-comment with --body-file failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if result["id"] != float64(2002) {
		t.Errorf("id = %v, want 2002", result["id"])
	}
}

// ─── add-labels ───────────────────────────────────────────────────────────────

func TestGitHubAddLabels(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `[{"id":1,"name":"bug","color":"d73a4a","description":""},{"id":2,"name":"enhancement","color":"a2eeef","description":""}]`)
	})

	out, err := runGitHubCmd(t, "add-labels",
		"--number", "42",
		"--labels", "bug,enhancement",
	)
	if err != nil {
		t.Fatalf("add-labels failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["number"] != float64(42) {
		t.Errorf("number = %v, want 42", result["number"])
	}
	labels, ok := result["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Errorf("labels = %v, want 2 entries", result["labels"])
	}
}

func TestGitHubAddLabelsMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "add-labels", "--labels", "bug")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

func TestGitHubAddLabelsMissingLabels(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "add-labels", "--number", "42")
	if err == nil {
		t.Fatal("expected error when --labels is missing")
	}
}

// ─── set-assignees ────────────────────────────────────────────────────────────

func TestGitHubSetAssignees(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueResponseJSON(42, "Issue", "open"))
	})

	out, err := runGitHubCmd(t, "set-assignees",
		"--number", "42",
		"--assignees", "alice",
	)
	if err != nil {
		t.Fatalf("set-assignees failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["number"] != float64(42) {
		t.Errorf("number = %v, want 42", result["number"])
	}
}

func TestGitHubSetAssigneesMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "set-assignees", "--assignees", "alice")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

// ─── close-issue ──────────────────────────────────────────────────────────────

func TestGitHubCloseIssue(t *testing.T) {
	var patchCount int
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, commentResponseJSON(3001, "closing"))
		case http.MethodPatch:
			patchCount++
			fmt.Fprint(w, issueResponseJSON(42, "Closed Issue", "closed"))
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	})

	out, err := runGitHubCmd(t, "close-issue",
		"--number", "42",
		"--comment", "Resolved in v2.0",
	)
	if err != nil {
		t.Fatalf("close-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["state"] != "closed" {
		t.Errorf("state = %v, want closed", result["state"])
	}
	if patchCount != 1 {
		t.Errorf("PATCH called %d times, want 1", patchCount)
	}
}

func TestGitHubCloseIssueMissingNumber(t *testing.T) {
	setupGitHubTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runGitHubCmd(t, "close-issue")
	if err == nil {
		t.Fatal("expected error when --number is missing")
	}
}

// ─── reopen-issue ─────────────────────────────────────────────────────────────

func TestGitHubReopenIssue(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			fmt.Fprint(w, issueResponseJSON(42, "Reopened Issue", "open"))
			return
		}
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	})

	out, err := runGitHubCmd(t, "reopen-issue", "--number", "42")
	if err != nil {
		t.Fatalf("reopen-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["state"] != "open" {
		t.Errorf("state = %v, want open", result["state"])
	}
}

// ─── list-labels ──────────────────────────────────────────────────────────────

func TestGitHubListLabels(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"id":1,"name":"bug","color":"d73a4a","description":"Something is broken"},
			{"id":2,"name":"enhancement","color":"a2eeef","description":"New feature"},
			{"id":3,"name":"documentation","color":"0075ca","description":"Docs only"}
		]`)
	})

	out, err := runGitHubCmd(t, "list-labels")
	if err != nil {
		t.Fatalf("list-labels failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 3 {
		t.Fatalf("got %d labels, want 3", len(arr))
	}
	first := arr[0].(map[string]any)
	if first["name"] != "bug" {
		t.Errorf("label[0].name = %v, want bug", first["name"])
	}
	if first["description"] != "Something is broken" {
		t.Errorf("label[0].description = %v, want 'Something is broken'", first["description"])
	}
}

// ─── get-repo ─────────────────────────────────────────────────────────────────

func TestGitHubGetRepo(t *testing.T) {
	setupGitHubTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
"full_name":"test-owner/test-repo",
"name":"test-repo",
"description":"A test repository",
"html_url":"https://github.com/test-owner/test-repo",
"private":false,
"open_issues_count":7
}`)
	})

	out, err := runGitHubCmd(t, "get-repo")
	if err != nil {
		t.Fatalf("get-repo failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["fullName"] != "test-owner/test-repo" {
		t.Errorf("fullName = %v, want test-owner/test-repo", result["fullName"])
	}
	if result["openIssues"] != float64(7) {
		t.Errorf("openIssues = %v, want 7", result["openIssues"])
	}
}

// ─── no token ─────────────────────────────────────────────────────────────────

func TestGitHubNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	setGitHubTestBaseURL("")
	t.Cleanup(func() { setGitHubTestBaseURL("") })

	root := newGitHubCmd(nil)
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "--number", "42", "--repo", "owner/repo"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when GITHUB_TOKEN is not set and no DataSource")
	}
	if !containsSubstring(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error should mention GITHUB_TOKEN, got: %v", err)
	}
}

// ─── owner/repo parsing ───────────────────────────────────────────────────────

func TestGitHubOwnerRepoCombinedFlag(t *testing.T) {
	// Tests that --repo owner/repo populates both owner and repo correctly.
	setupGitHubTest(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify the URL path contains the correct owner/repo.
		if r.URL.Path != "/repos/myorg/myapp/issues/5" {
			http.Error(w, fmt.Sprintf("unexpected path: %s", r.URL.Path), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueResponseJSON(5, "Org Issue", "open"))
	})

	// Override env owner/repo so they don't interfere.
	t.Setenv("GITHUB_OWNER", "")
	t.Setenv("GITHUB_REPO", "")

	out, err := runGitHubCmd(t, "get-issue", "--repo", "myorg/myapp", "--number", "5")
	if err != nil {
		t.Fatalf("get-issue with combined repo flag failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if result["number"] != float64(5) {
		t.Errorf("number = %v, want 5", result["number"])
	}
}

// ─── proxy path via DataSource ────────────────────────────────────────────────

func TestGitHubProxyModeViaDataSource(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	setGitHubTestBaseURL("")
	t.Cleanup(func() { setGitHubTestBaseURL("") })

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issueResponseJSON(1, "Proxied Issue", "open"))
	}))
	t.Cleanup(srv.Close)

	ds := func() afclient.DataSource {
		return afclient.NewAuthenticatedClient(srv.URL, "rsk_test_proxy_token")
	}

	root := newGitHubCmd(ds)
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "--repo", "owner/repo", "--number", "1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("proxy-mode get-issue failed: %v\nout: %s", err, buf.String())
	}

	if gotPath != "/api/cli/github/rest/repos/owner/repo/issues/1" {
		t.Errorf("proxy path = %q, want /api/cli/github/rest/repos/owner/repo/issues/1", gotPath)
	}
	if gotAuth != "Bearer rsk_test_proxy_token" {
		t.Errorf("Authorization = %q, want Bearer rsk_test_proxy_token", gotAuth)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
