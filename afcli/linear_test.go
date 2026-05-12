package afcli

// Tests for the `af linear` subcommand tree.
//
// Each test spins up an httptest.Server that serves a canned GraphQL response,
// sets LINEAR_API_KEY to a dummy fixture key, points the linear.Client at the
// test server URL, and asserts that the command produces the expected JSON on
// stdout.
//
// We exercise the command layer (flag wiring, env resolution, JSON shaping)
// rather than the GraphQL client itself — the client is tested separately in
// internal/linear/client_test.go.
//
// NOTE: Tests that call t.Setenv() MUST NOT call t.Parallel() — the Go runtime
// panics if both are used together (t.Setenv modifies process-global env).
// Tests that only use setTestBaseURL (process-global but test-safe via cleanup)
// also must not be parallel when they touch env vars.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// writeLinearGQLData writes a GraphQL success envelope wrapping the given JSON data.
func writeLinearGQLData(w http.ResponseWriter, dataJSON string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"data":%s}`, dataJSON)
}

// setupLinearTest sets up a test Linear server.
// It sets LINEAR_API_KEY to a fixture value, overrides the client base URL,
// and registers cleanup. Must NOT be called from a parallel test.
func setupLinearTest(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Set the API key env var. NOTE: t.Setenv requires non-parallel test.
	t.Setenv("LINEAR_API_KEY", "test-fixture-key-not-a-secret") //nolint:gosec // dummy fixture
	t.Setenv("LINEAR_ACCESS_TOKEN", "")

	// Override the client base URL.
	setTestBaseURL(srv.URL)
	t.Cleanup(func() { setTestBaseURL("") })

	return srv
}

// runLinearCmd builds the `linear <sub>` cobra tree, executes args,
// and returns captured stdout + any error.
func runLinearCmd(t *testing.T, _ string, args ...string) (string, error) {
	t.Helper()

	// Build a fresh linear command tree. The DataSource factory is nil for
	// these tests — they exercise the env-var path (setTestBaseURL +
	// LINEAR_API_KEY) which short-circuits before the DataSource branch.
	root := newLinearCmd(nil)
	root.SilenceErrors = true

	// Capture stdout.
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// decodeJSON decodes a JSON string into a map.
func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	// find the first `{` to skip any leading newline
	idx := strings.Index(s, "{")
	if idx < 0 {
		t.Fatalf("no JSON object found in output: %q", s)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s[idx:]), &out); err != nil {
		t.Fatalf("decode JSON: %v\nraw: %s", err, s)
	}
	return out
}

// decodeJSONArray decodes a JSON array string.
func decodeJSONArray(t *testing.T, s string) []any {
	t.Helper()
	idx := strings.Index(s, "[")
	if idx < 0 {
		t.Fatalf("no JSON array found in output: %q", s)
	}
	var out []any
	if err := json.Unmarshal([]byte(s[idx:]), &out); err != nil {
		t.Fatalf("decode JSON array: %v\nraw: %s", err, s)
	}
	return out
}

// ─── canned GraphQL response fixtures ────────────────────────────────────────

func issueNodeJSON(id, identifier, title, stateName, teamID, teamKey, teamName string) string {
	return fmt.Sprintf(`{
"id":%q,"identifier":%q,"title":%q,
"description":"desc","url":"https://linear.app/i/%s","priority":2,
"createdAt":"2025-01-01T00:00:00Z","updatedAt":"2025-01-02T00:00:00Z",
"state":{"id":"state-1","name":%q},
"team":{"id":%q,"key":%q,"name":%q},
"project":{"id":"proj-1","name":"TestProject"},
"labels":{"nodes":[{"id":"label-1","name":"Feature"}]},
"parent":null,"assignee":null}`,
		id, identifier, title, id, stateName, teamID, teamKey, teamName)
}

// A dispatcher that serves different responses based on what query is received.
type multiHandler struct {
	responses map[string]string // query substring → JSON data fragment
	fallback  string
}

func (h *multiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	for key, data := range h.responses {
		if strings.Contains(req.Query, key) {
			writeLinearGQLData(w, data)
			return
		}
	}
	if h.fallback != "" {
		writeLinearGQLData(w, h.fallback)
		return
	}
	writeLinearGQLData(w, `{}`)
}

// ─── get-issue ────────────────────────────────────────────────────────────────

func TestLinearGetIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "REN-1", "Test Issue", "In Progress", "team-1", "REN", "Rensei")
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
	})

	out, err := runLinearCmd(t, "", "get-issue", "REN-1")
	if err != nil {
		t.Fatalf("get-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "REN-1" {
		t.Errorf("identifier = %v, want REN-1", result["identifier"])
	}
	if result["title"] != "Test Issue" {
		t.Errorf("title = %v, want Test Issue", result["title"])
	}
	if result["status"] != "In Progress" {
		t.Errorf("status = %v, want In Progress", result["status"])
	}
	labels, ok := result["labels"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "Feature" {
		t.Errorf("labels = %v, want [Feature]", result["labels"])
	}
}

func TestLinearGetIssueNotFound(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, `{"issue":null}`)
	})

	_, err := runLinearCmd(t, "", "get-issue", "REN-999")
	if err == nil {
		t.Fatal("expected error for not-found issue, got nil")
	}
}

// ─── create-issue ─────────────────────────────────────────────────────────────

func TestLinearCreateIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-new", "REN-100", "New Issue", "Backlog", "team-1", "REN", "Rensei")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"REN","name":"Rensei"}]}}`

	handler := &multiHandler{
		responses: map[string]string{
			"ListTeams":   teamJSON,
			"CreateIssue": fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, issueJSON),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "create-issue",
		"--title", "New Issue",
		"--team", "Rensei",
	)
	if err != nil {
		t.Fatalf("create-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "REN-100" {
		t.Errorf("identifier = %v, want REN-100", result["identifier"])
	}
	if result["title"] != "New Issue" {
		t.Errorf("title = %v, want New Issue", result["title"])
	}
}

func TestLinearCreateIssueMissingTitle(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "create-issue", "--team", "Rensei")
	if err == nil {
		t.Fatal("expected error when --title is missing")
	}
}

func TestLinearCreateIssueMissingTeam(t *testing.T) {
	// Explicitly clear LINEAR_TEAM_NAME
	t.Setenv("LINEAR_TEAM_NAME", "")
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "create-issue", "--title", "Some Issue")
	if err == nil {
		t.Fatal("expected error when neither --team nor LINEAR_TEAM_NAME is set")
	}
}

func TestLinearCreateIssueFromEnvTeam(t *testing.T) {
	t.Setenv("LINEAR_TEAM_NAME", "Rensei")

	issueJSON := issueNodeJSON("issue-env", "REN-101", "Env Team Issue", "Backlog", "team-1", "REN", "Rensei")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"REN","name":"Rensei"}]}}`

	handler := &multiHandler{
		responses: map[string]string{
			"ListTeams":   teamJSON,
			"CreateIssue": fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, issueJSON),
		},
	}
	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "create-issue", "--title", "Env Team Issue")
	if err != nil {
		t.Fatalf("create-issue via LINEAR_TEAM_NAME failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if result["identifier"] != "REN-101" {
		t.Errorf("identifier = %v, want REN-101", result["identifier"])
	}
}

// TestLinearCreateIssueDescriptionFile tests --description-file flag.
func TestLinearCreateIssueDescriptionFile(t *testing.T) {
	// Write description to a temp file.
	// #nosec G306 — test fixture file, readable perms intentional
	tmpFile, err := os.CreateTemp(t.TempDir(), "desc-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString("This is the description from a file."); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	issueJSON := issueNodeJSON("issue-file", "REN-102", "File Desc Issue", "Backlog", "team-1", "REN", "Rensei")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"REN","name":"Rensei"}]}}`

	handler := &multiHandler{
		responses: map[string]string{
			"ListTeams": teamJSON,
		},
		fallback: fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, issueJSON),
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "create-issue",
		"--title", "File Desc Issue",
		"--team", "Rensei",
		"--description-file", tmpFile.Name(),
	)
	if err != nil {
		t.Fatalf("create-issue with --description-file failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "REN-102" {
		t.Errorf("identifier = %v, want REN-102", result["identifier"])
	}
}

// ─── update-issue ─────────────────────────────────────────────────────────────

func TestLinearUpdateIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "REN-1", "Updated Issue", "Finished", "team-1", "REN", "Rensei")

	handler := &multiHandler{
		responses: map[string]string{
			"GetIssue":           fmt.Sprintf(`{"issue":%s}`, issueJSON),
			"ListWorkflowStates": `{"workflowStates":{"nodes":[{"id":"state-done","name":"Finished","type":"completed"}]}}`,
			"UpdateIssue":        fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "update-issue", "REN-1", "--state", "Finished")
	if err != nil {
		t.Fatalf("update-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["status"] != "Finished" {
		t.Errorf("status = %v, want Finished", result["status"])
	}
}

// ─── list-comments ────────────────────────────────────────────────────────────

func TestLinearListComments(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	commentsData := fmt.Sprintf(`{"issue":{"comments":{"nodes":[
		{"id":"c-1","body":"Hello","createdAt":%q,"user":{"id":"u-1","name":"Alice"}},
		{"id":"c-2","body":"World","createdAt":%q,"user":null}
	]}}}`, now, now)

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, commentsData)
	})

	out, err := runLinearCmd(t, "", "list-comments", "REN-1")
	if err != nil {
		t.Fatalf("list-comments failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 2 {
		t.Fatalf("got %d comments, want 2", len(arr))
	}
	c0 := arr[0].(map[string]any)
	if c0["id"] != "c-1" {
		t.Errorf("comment[0].id = %v, want c-1", c0["id"])
	}
	if c0["body"] != "Hello" {
		t.Errorf("comment[0].body = %v, want Hello", c0["body"])
	}
}

// ─── create-comment ───────────────────────────────────────────────────────────

func TestLinearCreateComment(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	commentData := fmt.Sprintf(`{"commentCreate":{"success":true,"comment":{"id":"c-new","body":"My comment","createdAt":%q,"user":null}}}`, now)

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, commentData)
	})

	out, err := runLinearCmd(t, "", "create-comment", "REN-1", "--body", "My comment")
	if err != nil {
		t.Fatalf("create-comment failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["id"] != "c-new" {
		t.Errorf("id = %v, want c-new", result["id"])
	}
	if result["body"] != "My comment" {
		t.Errorf("body = %v, want My comment", result["body"])
	}
}

func TestLinearCreateCommentMissingBody(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "create-comment", "REN-1")
	if err == nil {
		t.Fatal("expected error when --body is missing")
	}
}

// TestLinearCreateCommentBodyFile tests --body-file flag.
func TestLinearCreateCommentBodyFile(t *testing.T) {
	// Write body to temp file.
	// #nosec G306 — test fixture file, readable perms intentional
	tmpFile, err := os.CreateTemp(t.TempDir(), "body-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpFile.WriteString("Comment from file"); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	commentData := fmt.Sprintf(`{"commentCreate":{"success":true,"comment":{"id":"c-file","body":"Comment from file","createdAt":%q,"user":null}}}`, now)

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, commentData)
	})

	out, err := runLinearCmd(t, "", "create-comment", "REN-1", "--body-file", tmpFile.Name())
	if err != nil {
		t.Fatalf("create-comment with --body-file failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["id"] != "c-file" {
		t.Errorf("id = %v, want c-file", result["id"])
	}
}

// ─── add-relation ─────────────────────────────────────────────────────────────

func TestLinearAddRelation(t *testing.T) {
	issue1 := issueNodeJSON("issue-1", "REN-1", "Issue 1", "Backlog", "team-1", "REN", "Rensei")
	issue2 := issueNodeJSON("issue-2", "REN-2", "Issue 2", "Backlog", "team-1", "REN", "Rensei")
	var callCount int

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if strings.Contains(req.Query, "GetIssue") {
			callCount++
			if callCount == 1 {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issue1))
			} else {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issue2))
			}
			return
		}
		writeLinearGQLData(w, `{"issueRelationCreate":{"success":true,"issueRelation":{"id":"rel-1","type":"blocks","relatedIssue":{"id":"issue-2","identifier":"REN-2"},"createdAt":"2025-01-01T00:00:00Z"}}}`)
	})

	out, err := runLinearCmd(t, "", "add-relation", "REN-1", "REN-2", "--type", "blocks")
	if err != nil {
		t.Fatalf("add-relation failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["success"] != true {
		t.Errorf("success = %v, want true", result["success"])
	}
	if result["type"] != "blocks" {
		t.Errorf("type = %v, want blocks", result["type"])
	}
}

func TestLinearAddRelationInvalidType(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "add-relation", "REN-1", "REN-2", "--type", "invalid")
	if err == nil {
		t.Fatal("expected error for invalid relation type")
	}
}

// ─── list-relations ───────────────────────────────────────────────────────────

func TestLinearListRelations(t *testing.T) {
	relData := `{"issue":{"relations":{"nodes":[
		{"id":"rel-1","type":"blocks","relatedIssue":{"id":"issue-2","identifier":"REN-2"},"createdAt":"2025-01-01T00:00:00Z"}
	]},"inverseRelations":{"nodes":[]}}}`

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, relData)
	})

	out, err := runLinearCmd(t, "", "list-relations", "REN-1")
	if err != nil {
		t.Fatalf("list-relations failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["issueId"] != "REN-1" {
		t.Errorf("issueId = %v, want REN-1", result["issueId"])
	}
	relations, ok := result["relations"].([]any)
	if !ok || len(relations) != 1 {
		t.Fatalf("relations = %v, want 1 entry", result["relations"])
	}
	rel := relations[0].(map[string]any)
	if rel["type"] != "blocks" {
		t.Errorf("type = %v, want blocks", rel["type"])
	}
	if rel["relatedIssue"] != "REN-2" {
		t.Errorf("relatedIssue = %v, want REN-2", rel["relatedIssue"])
	}
}

// ─── remove-relation ──────────────────────────────────────────────────────────

func TestLinearRemoveRelation(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, `{"issueRelationDelete":{"success":true}}`)
	})

	out, err := runLinearCmd(t, "", "remove-relation", "rel-123")
	if err != nil {
		t.Fatalf("remove-relation failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["success"] != true {
		t.Errorf("success = %v, want true", result["success"])
	}
	if result["relationId"] != "rel-123" {
		t.Errorf("relationId = %v, want rel-123", result["relationId"])
	}
}

// ─── list-sub-issues ──────────────────────────────────────────────────────────

func TestLinearListSubIssues(t *testing.T) {
	parent := issueNodeJSON("parent-1", "REN-1", "Parent", "In Progress", "team-1", "REN", "Rensei")
	child := issueNodeJSON("child-1", "REN-10", "Child", "Backlog", "team-1", "REN", "Rensei")

	var callCount int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if strings.Contains(req.Query, "GetIssue") && callCount == 0 {
			callCount++
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, parent))
			return
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, child))
	})

	out, err := runLinearCmd(t, "", "list-sub-issues", "REN-1")
	if err != nil {
		t.Fatalf("list-sub-issues failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["parentIdentifier"] != "REN-1" {
		t.Errorf("parentIdentifier = %v, want REN-1", result["parentIdentifier"])
	}
	if result["subIssueCount"] != float64(1) {
		t.Errorf("subIssueCount = %v, want 1", result["subIssueCount"])
	}
	subs := result["subIssues"].([]any)
	sub := subs[0].(map[string]any)
	if sub["identifier"] != "REN-10" {
		t.Errorf("sub[0].identifier = %v, want REN-10", sub["identifier"])
	}
}

// ─── list-sub-issue-statuses ──────────────────────────────────────────────────

func TestLinearListSubIssueStatuses(t *testing.T) {
	parent := issueNodeJSON("parent-1", "REN-1", "Parent", "In Progress", "team-1", "REN", "Rensei")
	child1 := issueNodeJSON("child-1", "REN-10", "Child Done", "Finished", "team-1", "REN", "Rensei")
	child2 := issueNodeJSON("child-2", "REN-11", "Child Todo", "Backlog", "team-1", "REN", "Rensei")

	var callCount int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if strings.Contains(req.Query, "GetIssue") && callCount == 0 {
			callCount++
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, parent))
			return
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s,%s]}}`, child1, child2))
	})

	out, err := runLinearCmd(t, "", "list-sub-issue-statuses", "REN-1")
	if err != nil {
		t.Fatalf("list-sub-issue-statuses failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["allFinishedOrLater"] != false {
		t.Errorf("allFinishedOrLater = %v, want false (one incomplete)", result["allFinishedOrLater"])
	}
	incomplete := result["incomplete"].([]any)
	if len(incomplete) != 1 {
		t.Errorf("incomplete count = %d, want 1", len(incomplete))
	}
}

// ─── update-sub-issue ─────────────────────────────────────────────────────────

func TestLinearUpdateSubIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "REN-1", "Sub Issue", "In Progress", "team-1", "REN", "Rensei")
	finishedJSON := issueNodeJSON("issue-1", "REN-1", "Sub Issue", "Finished", "team-1", "REN", "Rensei")

	var callCount int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch {
		case strings.Contains(req.Query, "GetIssue"):
			callCount++
			if callCount >= 2 {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, finishedJSON))
			} else {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
			}
		case strings.Contains(req.Query, "ListWorkflowStates"):
			writeLinearGQLData(w, `{"workflowStates":{"nodes":[{"id":"state-done","name":"Finished","type":"completed"}]}}`)
		case strings.Contains(req.Query, "UpdateIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, finishedJSON))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "update-sub-issue", "REN-1", "--state", "Finished")
	if err != nil {
		t.Fatalf("update-sub-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["status"] != "Finished" {
		t.Errorf("status = %v, want Finished", result["status"])
	}
}

func TestLinearUpdateSubIssueMissingArgs(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "update-sub-issue", "REN-1")
	if err == nil {
		t.Fatal("expected error when neither --state nor --comment is provided")
	}
}

// ─── list-issues ──────────────────────────────────────────────────────────────

func TestLinearListIssues(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue1 := issueNodeJSON("issue-1", "REN-1", "First", "Backlog", "team-1", "REN", "Rensei")
	issue2 := issueNodeJSON("issue-2", "REN-2", "Second", "Started", "team-1", "REN", "Rensei")

	handler := &multiHandler{
		responses: map[string]string{
			"ListProjects": proj,
			"ListIssues":   fmt.Sprintf(`{"issues":{"nodes":[%s,%s]}}`, issue1, issue2),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "list-issues", "--project", "TestProject")
	if err != nil {
		t.Fatalf("list-issues failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 2 {
		t.Fatalf("got %d issues, want 2", len(arr))
	}
}

// ─── check-blocked ────────────────────────────────────────────────────────────

func TestLinearCheckBlockedNotBlocked(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "REN-1", "Issue", "Backlog", "team-1", "REN", "Rensei")

	handler := &multiHandler{
		responses: map[string]string{
			"GetIssue":      fmt.Sprintf(`{"issue":%s}`, issueJSON),
			"ListRelations": `{"issue":{"relations":{"nodes":[]},"inverseRelations":{"nodes":[]}}}`,
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "check-blocked", "REN-1")
	if err != nil {
		t.Fatalf("check-blocked failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["blocked"] != false {
		t.Errorf("blocked = %v, want false", result["blocked"])
	}
	blockedBy, ok := result["blockedBy"].([]any)
	if !ok || len(blockedBy) != 0 {
		t.Errorf("blockedBy = %v, want []", result["blockedBy"])
	}
}

func TestLinearCheckBlockedIsBlocked(t *testing.T) {
	mainIssue := issueNodeJSON("issue-1", "REN-1", "Issue", "Backlog", "team-1", "REN", "Rensei")
	blockerIssue := issueNodeJSON("blocker-1", "REN-99", "Blocker", "In Progress", "team-1", "REN", "Rensei")

	// inverse relation: REN-99 blocks REN-1
	relData := `{"issue":{"relations":{"nodes":[]},"inverseRelations":{"nodes":[
		{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"REN-99"},"createdAt":"2025-01-01T00:00:00Z"}
	]}}}`

	var callCount int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if strings.Contains(req.Query, "GetIssue") {
			callCount++
			if callCount == 1 {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, mainIssue))
			} else {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, blockerIssue))
			}
			return
		}
		writeLinearGQLData(w, relData)
	})

	out, err := runLinearCmd(t, "", "check-blocked", "REN-1")
	if err != nil {
		t.Fatalf("check-blocked failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["blocked"] != true {
		t.Errorf("blocked = %v, want true", result["blocked"])
	}
	blockedBy, ok := result["blockedBy"].([]any)
	if !ok || len(blockedBy) != 1 {
		t.Fatalf("blockedBy = %v, want 1 entry", result["blockedBy"])
	}
	b := blockedBy[0].(map[string]any)
	if b["identifier"] != "REN-99" {
		t.Errorf("blocker identifier = %v, want REN-99", b["identifier"])
	}
}

// ─── list-backlog-issues ──────────────────────────────────────────────────────

func TestLinearListBacklogIssues(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "REN-1", "Backlog Issue", "Backlog", "team-1", "REN", "Rensei")

	handler := &multiHandler{
		responses: map[string]string{
			"ListProjects":      proj,
			"ListBacklogIssues": fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "list-backlog-issues", "--project", "TestProject")
	if err != nil {
		t.Fatalf("list-backlog-issues failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 1 {
		t.Fatalf("got %d issues, want 1", len(arr))
	}
	item := arr[0].(map[string]any)
	if item["identifier"] != "REN-1" {
		t.Errorf("identifier = %v, want REN-1", item["identifier"])
	}
}

func TestLinearListBacklogIssuesMissingProject(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "list-backlog-issues")
	if err == nil {
		t.Fatal("expected error when --project is missing")
	}
}

// ─── list-unblocked-backlog ───────────────────────────────────────────────────

func TestLinearListUnblockedBacklog(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue1 := issueNodeJSON("issue-1", "REN-1", "Unblocked Issue", "Backlog", "team-1", "REN", "Rensei")

	handler := &multiHandler{
		responses: map[string]string{
			"ListProjects":      proj,
			"ListBacklogIssues": fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue1),
			"ListRelations":     `{"issue":{"relations":{"nodes":[]},"inverseRelations":{"nodes":[]}}}`,
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
	if err != nil {
		t.Fatalf("list-unblocked-backlog failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 1 {
		t.Fatalf("got %d unblocked issues, want 1", len(arr))
	}
	item := arr[0].(map[string]any)
	if item["blocked"] != false {
		t.Errorf("blocked = %v, want false", item["blocked"])
	}
}

// ─── no API key ───────────────────────────────────────────────────────────────

// TestLinearProxyModeViaDataSource pins the platform-proxy path added by
// ADR-2026-05-12-cli-linear-proxy: when no LINEAR_API_KEY is set and the
// DataSource factory returns an authenticated *afclient.Client, the linear
// subcommand routes its GraphQL through `/api/cli/linear/graphql` with a
// `Bearer <rsk_*>` auth header.
func TestLinearProxyModeViaDataSource(t *testing.T) {
	// No env vars — force the proxy path.
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		// Mimic Linear's GraphQL envelope for a successful get-issue.
		writeLinearGQLData(w, `{"issue":{"id":"id-1","identifier":"REN-1","title":"Hello","state":{"name":"Backlog"},"team":{"name":"Rensei"},"project":null,"labels":{"nodes":[]}}}`)
	}))
	t.Cleanup(srv.Close)

	// The DataSource factory returns an *afclient.Client pointed at our
	// test server with an rsk_-style token. CredentialsFromDataSource()
	// extracts (BaseURL, APIToken) and feeds them to linear.NewProxiedClient.
	ds := func() afclient.DataSource {
		return afclient.NewAuthenticatedClient(srv.URL, "rsk_test_token")
	}

	root := newLinearCmd(ds)
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "REN-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("proxy-mode get-issue failed: %v\nout: %s", err, buf.String())
	}

	if gotPath != "/api/cli/linear/graphql" {
		t.Errorf("path = %q, want %q", gotPath, "/api/cli/linear/graphql")
	}
	if gotAuth != "Bearer rsk_test_token" {
		t.Errorf("Authorization = %q, want Bearer rsk_test_token", gotAuth)
	}
}

// TestLinearEnvWinsOverDataSource pins the precedence rule from the ADR:
// when both `LINEAR_API_KEY` env AND an authenticated DataSource are
// available, the env var wins. Preserves the worker-fleet path semantics.
func TestLinearEnvWinsOverDataSource(t *testing.T) {
	// Set the env var.
	t.Setenv("LINEAR_API_KEY", "lin_api_envvar")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeLinearGQLData(w, `{"issue":{"id":"id-1","identifier":"REN-1","title":"X","state":{"name":"Backlog"},"team":{"name":"Rensei"},"project":null,"labels":{"nodes":[]}}}`)
	}))
	t.Cleanup(srv.Close)

	// Direct-path uses the package-level baseURL hook.
	setTestBaseURL(srv.URL)
	t.Cleanup(func() { setTestBaseURL("") })

	// DataSource is non-nil + authenticated, but should be ignored because
	// the env var takes precedence.
	ds := func() afclient.DataSource {
		return afclient.NewAuthenticatedClient("https://app.rensei.ai", "rsk_should_not_be_used")
	}

	root := newLinearCmd(ds)
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "REN-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("env-precedence get-issue failed: %v\nout: %s", err, buf.String())
	}

	// Direct mode → raw header value (no Bearer prefix) per Linear's API.
	if gotAuth != "lin_api_envvar" {
		t.Errorf("Authorization = %q, want env-var raw value (no Bearer prefix)", gotAuth)
	}
}

func TestLinearNoAPIKey(t *testing.T) {
	// Remove both key env vars.
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")

	// Ensure setTestBaseURL is also reset so no client gets built.
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	// Pass a nil DataSource factory — exercises path 3 (no env, no
	// authenticated DataSource → friendly error).
	root := newLinearCmd(nil)
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "REN-1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when neither env key nor authenticated DataSource is available")
	}
	if !strings.Contains(err.Error(), "LINEAR_API_KEY") {
		t.Errorf("error should mention LINEAR_API_KEY, got: %v", err)
	}
}

// ─── create-blocker ───────────────────────────────────────────────────────────

func TestLinearCreateBlocker(t *testing.T) {
	sourceIssue := issueNodeJSON("source-1", "REN-50", "Source Issue", "In Progress", "team-1", "REN", "Rensei")
	blockerIssue := issueNodeJSON("blocker-1", "REN-99", "Blocker Issue", "Icebox", "team-1", "REN", "Rensei")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"REN","name":"Rensei"}]}}`

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, sourceIssue))
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`)
		case strings.Contains(req.Query, "ListIssues"):
			// Dedup check — no duplicates
			writeLinearGQLData(w, `{"issues":{"nodes":[]}}`)
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, teamJSON)
		case strings.Contains(req.Query, "ListWorkflowStates"):
			writeLinearGQLData(w, `{"workflowStates":{"nodes":[{"id":"state-icebox","name":"Icebox","type":"triage"}]}}`)
		case strings.Contains(req.Query, "issueLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-nh","name":"Needs Human"}]}}`)
		case strings.Contains(req.Query, "CreateIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, blockerIssue))
		case strings.Contains(req.Query, "CreateRelation"):
			writeLinearGQLData(w, `{"issueRelationCreate":{"success":true,"issueRelation":{"id":"rel-1","type":"blocks","relatedIssue":{"id":"source-1","identifier":"REN-50"},"createdAt":"2025-01-01T00:00:00Z"}}}`)
		case strings.Contains(req.Query, "CreateComment"):
			writeLinearGQLData(w, `{"commentCreate":{"success":true,"comment":{"id":"c-1","body":"blocker created","createdAt":"2025-01-01T00:00:00Z"}}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "create-blocker", "REN-50", "--title", "Blocker Issue")
	if err != nil {
		t.Fatalf("create-blocker failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "REN-99" {
		t.Errorf("identifier = %v, want REN-99", result["identifier"])
	}
	if result["sourceIssue"] != "REN-50" {
		t.Errorf("sourceIssue = %v, want REN-50", result["sourceIssue"])
	}
	if result["relation"] != "blocks" {
		t.Errorf("relation = %v, want blocks", result["relation"])
	}
	if result["deduplicated"] != false {
		t.Errorf("deduplicated = %v, want false", result["deduplicated"])
	}
}

func TestLinearCreateBlockerMissingTitle(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "create-blocker", "REN-50")
	if err == nil {
		t.Fatal("expected error when --title is missing")
	}
}

// ─── linear.Linear interface compliance ──────────────────────────────────────

// TestLinearClientImplementsInterface verifies *linear.Client satisfies the full interface.
func TestLinearClientImplementsInterface(_ *testing.T) {
	var _ linear.Linear = (*linear.Client)(nil)
}
