package linearcmd

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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/linear"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// writeLinearGQLData writes a GraphQL success envelope wrapping the given JSON data.
func writeLinearGQLData(w http.ResponseWriter, dataJSON string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"data":%s}`, dataJSON)
}

// writeLinearGQLError writes a GraphQL error envelope with no data payload.
func writeLinearGQLError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"message": message}},
	})
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
	root := New(nil, "donmai")
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

func projectFilterEqIgnoreCase(variables map[string]any, field string) string {
	filter, _ := variables["filter"].(map[string]any)
	or, _ := filter["or"].([]any)
	for _, clause := range or {
		entry, _ := clause.(map[string]any)
		comparison, _ := entry[field].(map[string]any)
		if value, ok := comparison["eqIgnoreCase"].(string); ok {
			return value
		}
	}
	return ""
}

// ─── canned GraphQL response fixtures ────────────────────────────────────────

func issueNodeJSON(id, identifier, title, stateName, teamID, teamKey, teamName string) string {
	return fmt.Sprintf(`{
"id":%q,"identifier":%q,"title":%q,
"description":"desc","url":"https://issues.example.com/i/%s","priority":2,
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

type labelListStub struct {
	labels map[string]string
	err    error
}

func (s labelListStub) ListLabels(context.Context) (map[string]string, error) {
	return s.labels, s.err
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
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Test Issue", "In Progress", "team-1", "ENG", "Engineering")
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
	})

	out, err := runLinearCmd(t, "", "get-issue", "ENG-1")
	if err != nil {
		t.Fatalf("get-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "ENG-1" {
		t.Errorf("identifier = %v, want ENG-1", result["identifier"])
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

	_, err := runLinearCmd(t, "", "get-issue", "ENG-999")
	if err == nil {
		t.Fatal("expected error for not-found issue, got nil")
	}
}

// ─── create-issue ─────────────────────────────────────────────────────────────

func TestLinearCreateIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-new", "ENG-100", "New Issue", "Backlog", "team-1", "ENG", "Engineering")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}`

	handler := &multiHandler{
		responses: map[string]string{
			"ListTeams":   teamJSON,
			"CreateIssue": fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, issueJSON),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(
		t, "", "create-issue",
		"--title", "New Issue",
		"--team", "Engineering",
	)
	if err != nil {
		t.Fatalf("create-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "ENG-100" {
		t.Errorf("identifier = %v, want ENG-100", result["identifier"])
	}
	if result["title"] != "New Issue" {
		t.Errorf("title = %v, want New Issue", result["title"])
	}
}

func TestResolveLabelIDs(t *testing.T) {
	tests := []struct {
		name      string
		labels    map[string]string
		listErr   error
		requested []string
		wantIDs   []string
		wantErr   string
	}{
		{
			name:      "all known",
			labels:    map[string]string{"Bug": "label-bug", "Feature": "label-feature"},
			requested: []string{"Bug", "Feature"},
			wantIDs:   []string{"label-bug", "label-feature"},
		},
		{
			name:      "partial unknown fails without partial IDs",
			labels:    map[string]string{"Bug": "label-bug"},
			requested: []string{"Bug", "Missing"},
			wantErr:   "unknown label names: Missing",
		},
		{
			name:      "all unknown names are reported",
			labels:    map[string]string{"Bug": "label-bug"},
			requested: []string{"Missing", "Absent"},
			wantErr:   "unknown label names: Missing, Absent",
		},
		{
			name:      "case insensitive match",
			labels:    map[string]string{"Needs Human": "label-human"},
			requested: []string{"needs human"},
			wantIDs:   []string{"label-human"},
		},
		{
			name:      "unicode simple fold match",
			labels:    map[string]string{"Σ": "label-sigma"},
			requested: []string{"ς"},
			wantIDs:   []string{"label-sigma"},
		},
		{
			name:      "duplicate requested names are de-duplicated case insensitively",
			labels:    map[string]string{"Bug": "label-bug"},
			requested: []string{"Bug", "bug", "BUG"},
			wantIDs:   []string{"label-bug"},
		},
		{
			name:      "label list failure",
			listErr:   errors.New("upstream unavailable"),
			requested: []string{"Bug"},
			wantErr:   "list labels: upstream unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLabelIDs(context.Background(), labelListStub{labels: tt.labels, err: tt.listErr}, tt.requested)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("resolveLabelIDs error = %v, want %q", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("resolveLabelIDs returned partial IDs: %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLabelIDs: %v", err)
			}
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("resolved IDs = %v, want %v", got, tt.wantIDs)
			}
			for i := range tt.wantIDs {
				if got[i] != tt.wantIDs[i] {
					t.Fatalf("resolved IDs = %v, want %v", got, tt.wantIDs)
				}
			}
		})
	}
}

func TestLinearCreateIssueDoesNotCreateWithUnknownLabels(t *testing.T) {
	var createCalls int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}`)
		case strings.Contains(req.Query, "ListLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-bug","name":"Bug"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "CreateIssue"):
			createCalls++
			writeLinearGQLData(w, `{}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "create-issue", "--title", "New Issue", "--team", "ENG", "--labels", "Bug,Missing")
	if err == nil || !strings.Contains(err.Error(), "unknown label names: Missing") {
		t.Fatalf("create-issue error = %v, want unknown label failure (out: %q)", err, out)
	}
	if createCalls != 0 {
		t.Fatalf("CreateIssue was called %d times after failed label resolution", createCalls)
	}
	if out != "" {
		t.Fatalf("create-issue wrote output after failed label resolution: %q", out)
	}
}

func TestLinearUpdateIssueDoesNotUpdateWithUnknownLabels(t *testing.T) {
	var updateCalls int
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Existing Issue", "Backlog", "team-1", "ENG", "Engineering")
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-bug","name":"Bug"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "UpdateIssue"):
			updateCalls++
			writeLinearGQLData(w, `{}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--labels", "Bug,Missing")
	if err == nil || !strings.Contains(err.Error(), "unknown label names: Missing") {
		t.Fatalf("update-issue error = %v, want unknown label failure (out: %q)", err, out)
	}
	if updateCalls != 0 {
		t.Fatalf("UpdateIssue was called %d times after failed label resolution", updateCalls)
	}
	if out != "" {
		t.Fatalf("update-issue wrote output after failed label resolution: %q", out)
	}
}

func TestLinearCreateIssueMissingTitle(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "create-issue", "--team", "Engineering")
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
	t.Setenv("LINEAR_TEAM_NAME", "Engineering")

	issueJSON := issueNodeJSON("issue-env", "ENG-101", "Env Team Issue", "Backlog", "team-1", "ENG", "Engineering")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}`

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
	if result["identifier"] != "ENG-101" {
		t.Errorf("identifier = %v, want ENG-101", result["identifier"])
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

	issueJSON := issueNodeJSON("issue-file", "ENG-102", "File Desc Issue", "Backlog", "team-1", "ENG", "Engineering")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}`

	handler := &multiHandler{
		responses: map[string]string{
			"ListTeams": teamJSON,
		},
		fallback: fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, issueJSON),
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(
		t, "", "create-issue",
		"--title", "File Desc Issue",
		"--team", "Engineering",
		"--description-file", tmpFile.Name(),
	)
	if err != nil {
		t.Fatalf("create-issue with --description-file failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "ENG-102" {
		t.Errorf("identifier = %v, want ENG-102", result["identifier"])
	}
}

// ─── update-issue ─────────────────────────────────────────────────────────────

func TestLinearUpdateIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Updated Issue", "Finished", "team-1", "ENG", "Engineering")

	handler := &multiHandler{
		responses: map[string]string{
			"GetIssue":           fmt.Sprintf(`{"issue":%s}`, issueJSON),
			"ListWorkflowStates": `{"workflowStates":{"nodes":[{"id":"state-done","name":"Finished","type":"completed"}]}}`,
			"UpdateIssue":        fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON),
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--state", "Finished")
	if err != nil {
		t.Fatalf("update-issue failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["status"] != "Finished" {
		t.Errorf("status = %v, want Finished", result["status"])
	}
}

func TestLinearUpdateIssue_ProjectOnlyResolvesSlugAndMutatesOnce(t *testing.T) {
	original := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ENG", "Engineering")
	updated := strings.Replace(original, `"name":"TestProject"`, `"name":"Destination"`, 1)
	var updateCalls atomic.Int32
	var updateInput map[string]any

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, original))
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-dest","name":"Destination","slugId":"destination-slug"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "UpdateIssue"):
			updateCalls.Add(1)
			updateInput, _ = req.Variables["input"].(map[string]any)
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, updated))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--project", "destination-slug")
	if err != nil {
		t.Fatalf("update-issue --project failed: %v\nout: %s", err, out)
	}
	if got := updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateIssue calls = %d, want 1", got)
	}
	if len(updateInput) != 1 || updateInput["projectId"] != "proj-dest" {
		t.Fatalf("mutation input = %#v, want only projectId=proj-dest", updateInput)
	}
	result := decodeJSON(t, out)
	if result["project"] != "Destination" {
		t.Errorf("project = %v, want Destination", result["project"])
	}
}

func TestLinearUpdateIssue_ProjectAndStatusAreOneMutation(t *testing.T) {
	original := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ENG", "Engineering")
	updated := strings.Replace(original, `"name":"Backlog"`, `"name":"Finished"`, 1)
	updated = strings.Replace(updated, `"name":"TestProject"`, `"name":"Destination"`, 1)
	var updateCalls atomic.Int32
	var updateInput map[string]any

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, original))
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-dest","name":"Destination","slugId":"destination-slug"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListWorkflowStates"):
			writeLinearGQLData(w, `{"workflowStates":{"nodes":[{"id":"state-done","name":"Finished","type":"completed"}]}}`)
		case strings.Contains(req.Query, "UpdateIssue"):
			updateCalls.Add(1)
			updateInput, _ = req.Variables["input"].(map[string]any)
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, updated))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--project", "Destination", "--status", "Finished")
	if err != nil {
		t.Fatalf("update-issue project+status failed: %v\nout: %s", err, out)
	}
	if got := updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateIssue calls = %d, want 1", got)
	}
	if len(updateInput) != 2 || updateInput["projectId"] != "proj-dest" || updateInput["stateId"] != "state-done" {
		t.Fatalf("mutation input = %#v, want projectId+stateId only", updateInput)
	}
	result := decodeJSON(t, out)
	if result["project"] != "Destination" || result["status"] != "Finished" {
		t.Fatalf("result = %#v, want Destination/Finished", result)
	}
}

func TestLinearUpdateIssue_ProjectResolutionFailureDoesNotMutate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projects   string
		wantErr    string
		projectRef string
	}{
		{name: "unknown", projects: `{"projects":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, wantErr: "not found", projectRef: "missing"},
		{name: "ambiguous", projects: `{"projects":{"nodes":[{"id":"p1","name":"Same","slugId":"one"},{"id":"p2","name":"Same","slugId":"two"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, wantErr: "ambiguous", projectRef: "Same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ENG", "Engineering")
			var updateCalls atomic.Int32
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query string `json:"query"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch {
				case strings.Contains(req.Query, "GetIssue"):
					writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, original))
				case strings.Contains(req.Query, "ListProjects"):
					writeLinearGQLData(w, tc.projects)
				case strings.Contains(req.Query, "UpdateIssue"):
					updateCalls.Add(1)
					writeLinearGQLData(w, `{"issueUpdate":{"success":true,"issue":null}}`)
				default:
					writeLinearGQLData(w, `{}`)
				}
			})

			out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--project", tc.projectRef, "--status", "Finished")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantErr) {
				t.Fatalf("error = %v, want %q (out: %q)", err, tc.wantErr, out)
			}
			if got := updateCalls.Load(); got != 0 {
				t.Fatalf("UpdateIssue calls after resolution failure = %d, want 0", got)
			}
		})
	}
}

func TestLinearUpdateIssue_ProjectHelpNamesMutation(t *testing.T) {
	out, err := runLinearCmd(t, "", "update-issue", "--help")
	if err != nil {
		t.Fatalf("update-issue --help: %v", err)
	}
	if !strings.Contains(out, "Move issue to a Linear project name, slug, or UUID (mutation, not CLI context)") {
		t.Fatalf("help does not distinguish project mutation from CLI context:\n%s", out)
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

	out, err := runLinearCmd(t, "", "list-comments", "ENG-1")
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

	out, err := runLinearCmd(t, "", "create-comment", "ENG-1", "--body", "My comment")
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
	_, err := runLinearCmd(t, "", "create-comment", "ENG-1")
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

	out, err := runLinearCmd(t, "", "create-comment", "ENG-1", "--body-file", tmpFile.Name())
	if err != nil {
		t.Fatalf("create-comment with --body-file failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["id"] != "c-file" {
		t.Errorf("id = %v, want c-file", result["id"])
	}
}

// ─── add-relation ─────────────────────────────────────────────────────────────

func TestLinearAddRelationAcceptsEveryKnownType(t *testing.T) {
	for _, relationType := range linear.KnownIssueRelationTypes() {
		t.Run(relationType, func(t *testing.T) {
			issue1 := issueNodeJSON("issue-1", "ENG-1", "Issue 1", "Backlog", "team-1", "ENG", "Engineering")
			issue2 := issueNodeJSON("issue-2", "ENG-2", "Issue 2", "Backlog", "team-1", "ENG", "Engineering")
			var callCount int

			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
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
				if got := req.Variables["type"]; got != relationType {
					t.Fatalf("mutation relation type = %v, want %q", got, relationType)
				}
				writeLinearGQLData(w, `{"issueRelationCreate":{"success":true,"issueRelation":{"id":"rel-1"}}}`)
			})

			out, err := runLinearCmd(t, "", "add-relation", "ENG-1", "ENG-2", "--type", relationType)
			if err != nil {
				t.Fatalf("add-relation failed: %v\nout: %s", err, out)
			}

			result := decodeJSON(t, out)
			if result["success"] != true {
				t.Errorf("success = %v, want true", result["success"])
			}
			if result["type"] != relationType {
				t.Errorf("type = %v, want %s", result["type"], relationType)
			}
		})
	}
}

func TestLinearAddRelationInvalidType(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "add-relation", "ENG-1", "ENG-2", "--type", "invalid")
	if err == nil {
		t.Fatal("expected error for invalid relation type")
	}
}

// ─── list-relations ───────────────────────────────────────────────────────────

func TestLinearListRelations(t *testing.T) {
	relData := `{"issue":{"relations":{"nodes":[
		{"id":"rel-1","type":"blocks","relatedIssue":{"id":"issue-2","identifier":"ENG-2"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, relData)
	})

	out, err := runLinearCmd(t, "", "list-relations", "ENG-1")
	if err != nil {
		t.Fatalf("list-relations failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["issueId"] != "ENG-1" {
		t.Errorf("issueId = %v, want ENG-1", result["issueId"])
	}
	relations, ok := result["relations"].([]any)
	if !ok || len(relations) != 1 {
		t.Fatalf("relations = %v, want 1 entry", result["relations"])
	}
	rel := relations[0].(map[string]any)
	if rel["type"] != "blocks" {
		t.Errorf("type = %v, want blocks", rel["type"])
	}
	if rel["relatedIssue"] != "ENG-2" {
		t.Errorf("relatedIssue = %v, want ENG-2", rel["relatedIssue"])
	}
}

func TestLinearListRelationsAcceptsEveryKnownRelationType(t *testing.T) {
	relData := `{"issue":{"relations":{"nodes":[
		{"id":"rel-1","type":"related","relatedIssue":{"id":"issue-2","identifier":"ENG-2"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"rel-2","type":"blocks","relatedIssue":{"id":"issue-3","identifier":"ENG-3"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"rel-3","type":"duplicate","relatedIssue":{"id":"issue-4","identifier":"ENG-4"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"rel-4","type":"similar","relatedIssue":{"id":"issue-5","identifier":"ENG-5"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
		{"id":"inverse-1","type":"related","issue":{"id":"issue-6","identifier":"ENG-6"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"inverse-2","type":"blocks","issue":{"id":"issue-7","identifier":"ENG-7"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"inverse-3","type":"duplicate","issue":{"id":"issue-8","identifier":"ENG-8"},"createdAt":"2025-01-01T00:00:00Z"},
		{"id":"inverse-4","type":"similar","issue":{"id":"issue-9","identifier":"ENG-9"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, relData)
	})

	out, err := runLinearCmd(t, "", "list-relations", "ENG-1")
	if err != nil {
		t.Fatalf("list-relations failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	relations := result["relations"].([]any)
	inverseRelations := result["inverseRelations"].([]any)
	wantTypes := linear.KnownIssueRelationTypes()
	if len(relations) != len(wantTypes) {
		t.Fatalf("relations = %v, want all %d known types", relations, len(wantTypes))
	}
	if len(inverseRelations) != len(wantTypes) {
		t.Fatalf("inverseRelations = %v, want all %d known types", inverseRelations, len(wantTypes))
	}
	for i, want := range wantTypes {
		if got := relations[i].(map[string]any)["type"]; got != want {
			t.Errorf("relations[%d].type = %v, want %q", i, got, want)
		}
		if got := inverseRelations[i].(map[string]any)["type"]; got != want {
			t.Errorf("inverseRelations[%d].type = %v, want %q", i, got, want)
		}
	}
}

func TestLinearListRelationsAcceptsExplicitEmptyArrays(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	out, err := runLinearCmd(t, "", "list-relations", "ENG-1")
	if err != nil {
		t.Fatalf("list-relations failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if relations, ok := result["relations"].([]any); !ok || len(relations) != 0 {
		t.Fatalf("relations = %v, want []", result["relations"])
	}
	if inverse, ok := result["inverseRelations"].([]any); !ok || len(inverse) != 0 {
		t.Fatalf("inverseRelations = %v, want []", result["inverseRelations"])
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
	parent := issueNodeJSON("parent-1", "ENG-1", "Parent", "In Progress", "team-1", "ENG", "Engineering")
	child1 := issueNodeJSON("child-1", "ENG-10", "First child", "Backlog", "team-1", "ENG", "Engineering")
	child2 := issueNodeJSON("child-2", "ENG-11", "Second child", "Started", "team-1", "ENG", "Engineering")

	var subIssueRequests int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if strings.Contains(req.Query, "GetIssue") {
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, parent))
			return
		}
		if !strings.Contains(req.Query, "ListSubIssues") {
			t.Fatalf("unexpected query: %s", req.Query)
		}
		subIssueRequests++
		if subIssueRequests == 1 {
			if req.Variables["after"] != nil {
				t.Fatalf("first page after = %#v, want nil", req.Variables["after"])
			}
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":true,"endCursor":"page-1"}}}`, child1))
			return
		}
		if req.Variables["after"] != "page-1" {
			t.Fatalf("second page after = %#v, want page-1", req.Variables["after"])
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, child2))
	})

	out, err := runLinearCmd(t, "", "list-sub-issues", "ENG-1")
	if err != nil {
		t.Fatalf("list-sub-issues failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["parentIdentifier"] != "ENG-1" {
		t.Errorf("parentIdentifier = %v, want ENG-1", result["parentIdentifier"])
	}
	if result["subIssueCount"] != float64(2) {
		t.Fatalf("subIssueCount = %v, want 2", result["subIssueCount"])
	}
	subs, ok := result["subIssues"].([]any)
	if !ok || len(subs) != 2 {
		t.Fatalf("subIssues = %#v, want 2 entries", result["subIssues"])
	}
	first := subs[0].(map[string]any)
	second := subs[1].(map[string]any)
	if first["identifier"] != "ENG-10" || second["identifier"] != "ENG-11" {
		t.Errorf("sub-issue order = [%v %v], want [ENG-10 ENG-11]", first["identifier"], second["identifier"])
	}
	if subIssueRequests != 2 {
		t.Errorf("ListSubIssues requests = %d, want 2", subIssueRequests)
	}
}

// ─── list-sub-issue-statuses ──────────────────────────────────────────────────

func TestLinearListSubIssueStatuses(t *testing.T) {
	parent := issueNodeJSON("parent-1", "ENG-1", "Parent", "In Progress", "team-1", "ENG", "Engineering")
	child1 := issueNodeJSON("child-1", "ENG-10", "Child Done", "Finished", "team-1", "ENG", "Engineering")
	child2 := issueNodeJSON("child-2", "ENG-11", "Child Todo", "Backlog", "team-1", "ENG", "Engineering")

	var subIssueRequests int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if strings.Contains(req.Query, "GetIssue") {
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, parent))
			return
		}
		if !strings.Contains(req.Query, "ListSubIssues") {
			t.Fatalf("unexpected query: %s", req.Query)
		}
		subIssueRequests++
		if subIssueRequests == 1 {
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":true,"endCursor":"page-1"}}}`, child1))
			return
		}
		if req.Variables["after"] != "page-1" {
			t.Fatalf("second page after = %#v, want page-1", req.Variables["after"])
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, child2))
	})

	out, err := runLinearCmd(t, "", "list-sub-issue-statuses", "ENG-1")
	if err != nil {
		t.Fatalf("list-sub-issue-statuses failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["allFinishedOrLater"] != false {
		t.Errorf("allFinishedOrLater = %v, want false (one incomplete)", result["allFinishedOrLater"])
	}
	if result["subIssueCount"] != float64(2) {
		t.Fatalf("subIssueCount = %v, want 2", result["subIssueCount"])
	}
	incomplete := result["incomplete"].([]any)
	if len(incomplete) != 1 {
		t.Fatalf("incomplete count = %d, want 1", len(incomplete))
	}
	if incomplete[0].(map[string]any)["identifier"] != "ENG-11" {
		t.Errorf("incomplete identifier = %v, want ENG-11", incomplete[0].(map[string]any)["identifier"])
	}
	if subIssueRequests != 2 {
		t.Errorf("ListSubIssues requests = %d, want 2", subIssueRequests)
	}
}

// ─── update-sub-issue ─────────────────────────────────────────────────────────

func TestLinearUpdateSubIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Sub Issue", "In Progress", "team-1", "ENG", "Engineering")
	finishedJSON := issueNodeJSON("issue-1", "ENG-1", "Sub Issue", "Finished", "team-1", "ENG", "Engineering")

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

	out, err := runLinearCmd(t, "", "update-sub-issue", "ENG-1", "--state", "Finished")
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
	_, err := runLinearCmd(t, "", "update-sub-issue", "ENG-1")
	if err == nil {
		t.Fatal("expected error when neither --state nor --comment is provided")
	}
}

// ─── list-issues ──────────────────────────────────────────────────────────────

func TestLinearListIssues(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue1 := issueNodeJSON("issue-1", "ENG-1", "First", "Backlog", "team-1", "ENG", "Engineering")
	issue2 := issueNodeJSON("issue-2", "ENG-2", "Second", "Started", "team-1", "ENG", "Engineering")

	handler := &multiHandler{
		responses: map[string]string{
			"ListProjects": proj,
			"ListIssues":   fmt.Sprintf(`{"issues":{"nodes":[%s,%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, issue1, issue2),
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
	first := arr[0].(map[string]any)
	second := arr[1].(map[string]any)
	if first["id"] != "ENG-1" || second["id"] != "ENG-2" {
		t.Fatalf("equal-priority issue order changed: %#v", arr)
	}
}

func TestLinearListIssuesPaginatesLargeLimitWithoutChangingJSONArray(t *testing.T) {
	request := 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "ListIssues") {
			writeLinearGQLData(w, `{}`)
			return
		}
		request++
		if request == 1 {
			if req.Variables["first"] != float64(250) || req.Variables["after"] != nil {
				t.Fatalf("first-page variables = %#v", req.Variables)
			}
			cursor := "page-1"
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":true,"endCursor":%q}}}`,
				issueNodeJSON("issue-1", "ENG-1", "First", "Backlog", "team-1", "ENG", "Engineering"), cursor))
			return
		}
		if req.Variables["first"] != float64(250) || req.Variables["after"] != "page-1" {
			t.Fatalf("second-page variables = %#v", req.Variables)
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
			issueNodeJSON("issue-2", "ENG-2", "Second", "Backlog", "team-1", "ENG", "Engineering")))
	})

	out, err := runLinearCmd(t, "", "list-issues", "--limit", "300")
	if err != nil {
		t.Fatalf("list-issues failed: %v\nout: %s", err, out)
	}
	arr := decodeJSONArray(t, out)
	if len(arr) != 2 || request != 2 {
		t.Fatalf("output/request count = %d/%d, want 2/2", len(arr), request)
	}
}

func TestLinearListIssuesRejectsInvalidLimit(t *testing.T) {
	requests := 0
	setupLinearTest(t, func(http.ResponseWriter, *http.Request) { requests++ })
	for _, limit := range []string{"0", "-1", "25001"} {
		_, err := runLinearCmd(t, "", "list-issues", "--limit", limit, "--project", "would-require-network")
		if err == nil || !strings.Contains(err.Error(), "issue list limit") {
			t.Fatalf("limit %s: error = %v, want issue-list-limit error", limit, err)
		}
	}
	if requests != 0 {
		t.Fatalf("network requests = %d, want 0", requests)
	}
}

// ─── check-blocked ────────────────────────────────────────────────────────────

func TestLinearCheckBlockedNotBlocked(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")

	handler := &multiHandler{
		responses: map[string]string{
			"GetIssue":      fmt.Sprintf(`{"issue":%s}`, issueJSON),
			"ListRelations": `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
	}

	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
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

func TestLinearCheckBlockedAcceptsKnownNonBlockingRelationTypes(t *testing.T) {
	for _, relationType := range linear.KnownIssueRelationTypes() {
		if relationType == "blocks" {
			continue
		}
		t.Run(relationType, func(t *testing.T) {
			issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
			relData := fmt.Sprintf(`{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
				{"id":"rel-1","type":%q,"issue":{"id":"related-1","identifier":"ENG-2"},"createdAt":"2025-01-01T00:00:00Z"}
			],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`, relationType)
			handler := &multiHandler{
				responses: map[string]string{
					"GetIssue":      fmt.Sprintf(`{"issue":%s}`, issueJSON),
					"ListRelations": relData,
				},
			}
			setupLinearTest(t, handler.ServeHTTP)

			out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
			if err != nil {
				t.Fatalf("check-blocked failed: %v\nout: %s", err, out)
			}
			result := decodeJSON(t, out)
			if result["blocked"] != false {
				t.Errorf("blocked = %v, want false for %s relation", result["blocked"], relationType)
			}
		})
	}
}

func TestLinearCheckBlockedIsBlocked(t *testing.T) {
	mainIssue := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	blockerIssue := issueNodeJSON("blocker-1", "ENG-99", "Blocker", "In Progress", "team-1", "ENG", "Engineering")

	// inverse relation: ENG-99 blocks ENG-1
	relData := `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
		{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"ENG-99"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

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

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
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
	if b["identifier"] != "ENG-99" {
		t.Errorf("blocker identifier = %v, want ENG-99", b["identifier"])
	}
}

func TestLinearCheckBlockedFailsClosedOnRelationReadError(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "GetIssue") {
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
			return
		}
		writeLinearGQLError(w, "simulated relation read failure")
	})

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
	if err == nil {
		t.Fatalf("expected relation read error; out: %s", out)
	}
	if !strings.Contains(err.Error(), "check blockers: graphql error: simulated relation read failure") {
		t.Fatalf("error = %q, want contextual relation read failure", err)
	}
	if strings.Contains(out, `"blocked":false`) {
		t.Fatalf("check-blocked emitted a false unblocked result: %s", out)
	}
}

func TestLinearCheckBlockedFailsClosedOnBlockerReadError(t *testing.T) {
	mainIssue := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	relData := `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
		{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"ENG-99"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	var getIssueCalls int
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			getIssueCalls++
			if getIssueCalls == 1 {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, mainIssue))
				return
			}
			writeLinearGQLError(w, "simulated blocker read failure")
		case strings.Contains(req.Query, "ListRelations"):
			writeLinearGQLData(w, relData)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
	if err == nil {
		t.Fatalf("expected blocker read error; out: %s", out)
	}
	if !strings.Contains(err.Error(), `check blockers: get blocking issue "blocker-1": graphql error: simulated blocker read failure`) {
		t.Fatalf("error = %q, want contextual blocker read failure", err)
	}
	if strings.Contains(out, `"blocked":false`) {
		t.Fatalf("check-blocked emitted a false unblocked result: %s", out)
	}
}

type incompleteRelationPayload struct {
	name string
	data string
}

func incompleteRelationPayloads() []incompleteRelationPayload {
	const createdAt = `"createdAt":"2025-01-01T00:00:00Z"`
	return []incompleteRelationPayload{
		{name: "null data", data: `null`},
		{name: "empty data", data: `{}`},
		{name: "null issue", data: `{"issue":null}`},
		{
			name: "missing relations connection",
			data: `{"issue":{"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing inverse relations connection",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing relations nodes",
			data: `{"issue":{"relations":{"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null relations nodes",
			data: `{"issue":{"relations":{"nodes":null,"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing inverse relations nodes",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null inverse relations nodes",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":null,"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null relation node",
			data: `{"issue":{"relations":{"nodes":[null],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "empty relation node",
			data: `{"issue":{"relations":{"nodes":[{}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing relation type",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","relatedIssue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null relation type",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":null,"relatedIssue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "empty relation type",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"","relatedIssue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "unknown relation type",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"future","relatedIssue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing relation id",
			data: `{"issue":{"relations":{"nodes":[{"type":"blocks","relatedIssue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing relation createdAt",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"blocks","relatedIssue":{"id":"issue-2"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "blocks with missing relatedIssue",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"blocks",` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "blocks with null relatedIssue",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"blocks","relatedIssue":null,` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "blocks with missing relatedIssue id",
			data: `{"issue":{"relations":{"nodes":[{"id":"rel-1","type":"blocks","relatedIssue":{"identifier":"ENG-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null inverse relation node",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[null],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "empty inverse relation node",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing inverse relation type",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","issue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "null inverse relation type",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":null,"issue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "empty inverse relation type",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"","issue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "unknown inverse relation type",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"future","issue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing inverse relation id",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"issue-2"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "missing inverse relation createdAt",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks","issue":{"id":"issue-2"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "inverse blocks with missing issue",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks",` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "inverse blocks with null issue",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks","issue":null,` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
		{
			name: "inverse blocks with missing issue id",
			data: `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks","issue":{"identifier":"ENG-99"},` + createdAt + `}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		},
	}
}

func TestLinearListRelationsFailsClosedOnIncompleteRelationPayloads(t *testing.T) {
	for _, tt := range incompleteRelationPayloads() {
		t.Run(tt.name, func(t *testing.T) {
			setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
				writeLinearGQLData(w, tt.data)
			})

			out, err := runLinearCmd(t, "", "list-relations", "ENG-1")
			if err == nil {
				t.Fatalf("expected incomplete relation response error; out: %s", out)
			}
			if !strings.Contains(err.Error(), "incomplete relations response") {
				t.Fatalf("error = %q, want incomplete response context", err)
			}
			if strings.Contains(out, `"relations"`) {
				t.Fatalf("list-relations emitted an incomplete result: %s", out)
			}
		})
	}
}

func TestLinearCheckBlockedFailsClosedOnIncompleteRelationPayloads(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	tests := incompleteRelationPayloads()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query string `json:"query"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if strings.Contains(req.Query, "GetIssue") {
					writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
					return
				}
				writeLinearGQLData(w, tt.data)
			})

			out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
			if err == nil {
				t.Fatalf("expected incomplete relation response error; out: %s", out)
			}
			if !strings.Contains(err.Error(), "incomplete relations response") {
				t.Fatalf("error = %q, want incomplete response context", err)
			}
			if strings.Contains(out, `"blocked":false`) {
				t.Fatalf("check-blocked emitted a false unblocked result: %s", out)
			}
		})
	}
}

func TestLinearCheckBlockedFindsBlockerOnLaterRelationPage(t *testing.T) {
	mainIssue := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	blockerIssue := issueNodeJSON("blocker-1", "ENG-99", "Blocker", "In Progress", "team-1", "ENG", "Engineering")
	var relationCalls, getIssueCalls int

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			getIssueCalls++
			if getIssueCalls == 1 {
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, mainIssue))
				return
			}
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, blockerIssue))
		case strings.Contains(req.Query, "ListRelations"):
			relationCalls++
			if req.Variables["inverseRelationsAfter"] == nil {
				writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"inverse-page-1"}}}}`)
				return
			}
			if req.Variables["inverseRelationsAfter"] != "inverse-page-1" {
				t.Fatalf("inverseRelationsAfter = %v, want inverse-page-1", req.Variables["inverseRelationsAfter"])
			}
			writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"ENG-99"},"createdAt":"2025-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
	if err != nil {
		t.Fatalf("check-blocked failed: %v\nout: %s", err, out)
	}
	result := decodeJSON(t, out)
	if result["blocked"] != true {
		t.Fatalf("blocked = %v, want true", result["blocked"])
	}
	if relationCalls != 2 {
		t.Fatalf("relation page calls = %d, want 2", relationCalls)
	}
}

func TestLinearCheckBlockedFailsClosedOnTruncatedRelationPage(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "GetIssue") {
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
			return
		}
		writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`)
	})

	out, err := runLinearCmd(t, "", "check-blocked", "ENG-1")
	if err == nil {
		t.Fatalf("expected truncated relation response error; out: %s", out)
	}
	if !strings.Contains(err.Error(), "inverseRelations hasNextPage without endCursor") {
		t.Fatalf("error = %q, want unusable cursor context", err)
	}
	if strings.Contains(out, `"blocked":false`) {
		t.Fatalf("check-blocked emitted a false unblocked result: %s", out)
	}
}

// ─── list-backlog-issues ──────────────────────────────────────────────────────

func TestLinearListBacklogIssues(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Backlog Issue", "Backlog", "team-1", "ENG", "Engineering")

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
	if item["identifier"] != "ENG-1" {
		t.Errorf("identifier = %v, want ENG-1", item["identifier"])
	}
}

func TestLinearListBacklogIssuesMissingProject(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "list-backlog-issues")
	if err == nil {
		t.Fatal("expected error when --project is missing")
	}
}

// TestLinearListBacklogIssues_FlagsAndOutput asserts the grooming flags
// propagate into the GraphQL query/variables and that parentID is
// projected into the JSON output so a caller can assert it is empty
// (top-level) on a parents-only listing.
func TestLinearListBacklogIssues_FlagsAndOutput(t *testing.T) {
	const teamUUID = "11111111-2222-3333-4444-555555555555"

	// Capture the backlog-issues request body for assertion.
	var sawStates []any
	var sawParentNull bool
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			sawStates, _ = req.Variables["states"].([]any)
			sawParentNull = strings.Contains(req.Query, "parent: { null: true }")
			issue := `{"id":"issue-1","identifier":"ENG-1","title":"Top","state":{"name":"Icebox"},"parent":null}`
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(
		t, "", "list-backlog-issues",
		"--project", "TestProject",
		"--team", teamUUID,
		"--statuses", "Icebox,Backlog",
		"--parents-only",
	)
	if err != nil {
		t.Fatalf("list-backlog-issues failed: %v\nout: %s", err, out)
	}

	if len(sawStates) != 2 || sawStates[0] != "Icebox" || sawStates[1] != "Backlog" {
		t.Errorf("query states = %v; want [Icebox Backlog]", sawStates)
	}
	if !sawParentNull {
		t.Error("parents-only query missing parent-null clause")
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 1 {
		t.Fatalf("got %d issues; want 1", len(arr))
	}
	item := arr[0].(map[string]any)
	if _, has := item["parentID"]; !has {
		t.Errorf("output item missing parentID field: %#v", item)
	}
	if item["parentID"] != "" {
		t.Errorf("top-level issue parentID = %v; want empty", item["parentID"])
	}
}

// TestLinearListBacklogIssues_DefaultStatusIsIcebox confirms the
// command defaults --statuses to Icebox (the grooming target) when the
// flag is omitted.
func TestLinearListBacklogIssues_DefaultStatusIsIcebox(t *testing.T) {
	var sawStates []any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			sawStates, _ = req.Variables["states"].([]any)
			writeLinearGQLData(w, `{"issues":{"nodes":[]}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	if _, err := runLinearCmd(t, "", "list-backlog-issues", "--project", "TestProject"); err != nil {
		t.Fatalf("list-backlog-issues failed: %v", err)
	}
	if len(sawStates) != 1 || sawStates[0] != "Icebox" {
		t.Errorf("default states = %v; want [Icebox]", sawStates)
	}
}

// ─── list-unblocked-backlog ───────────────────────────────────────────────────

func TestLinearListUnblockedBacklog(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue1 := issueNodeJSON("issue-1", "ENG-1", "Unblocked Issue", "Backlog", "team-1", "ENG", "Engineering")

	handler := &multiHandler{
		responses: map[string]string{
			"ListProjects":      proj,
			"ListBacklogIssues": fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue1),
			"ListRelations":     `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
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

func TestLinearListUnblockedBacklogAcceptsKnownNonBlockingRelationTypes(t *testing.T) {
	for _, relationType := range linear.KnownIssueRelationTypes() {
		if relationType == "blocks" {
			continue
		}
		t.Run(relationType, func(t *testing.T) {
			proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
			issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")
			relData := fmt.Sprintf(`{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
				{"id":"rel-1","type":%q,"issue":{"id":"related-1","identifier":"ENG-2"},"createdAt":"2025-01-01T00:00:00Z"}
			],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`, relationType)
			handler := &multiHandler{
				responses: map[string]string{
					"ListProjects":      proj,
					"ListBacklogIssues": fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue),
					"ListRelations":     relData,
				},
			}
			setupLinearTest(t, handler.ServeHTTP)

			out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
			if err != nil {
				t.Fatalf("list-unblocked-backlog failed: %v\nout: %s", err, out)
			}
			arr := decodeJSONArray(t, out)
			if len(arr) != 1 || arr[0].(map[string]any)["identifier"] != "ENG-1" {
				t.Fatalf("output = %v, want the candidate preserved", arr)
			}
		})
	}
}

func TestLinearListUnblockedBacklogFailsClosedOnRelationReadError(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, proj)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
		case strings.Contains(req.Query, "ListRelations"):
			writeLinearGQLError(w, "simulated relation read failure")
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
	if err == nil {
		t.Fatalf("expected relation read error; out: %s", out)
	}
	if !strings.Contains(err.Error(), "resolve blockers for issue ENG-1: graphql error: simulated relation read failure") {
		t.Fatalf("error = %q, want contextual relation read failure", err)
	}
	if strings.Contains(out, "ENG-1") {
		t.Fatalf("list-unblocked-backlog emitted an unchecked candidate: %s", out)
	}
}

func TestLinearListUnblockedBacklogFailsClosedOnBlockerReadError(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")
	relData := `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[
		{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"ENG-99"},"createdAt":"2025-01-01T00:00:00Z"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, proj)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
		case strings.Contains(req.Query, "ListRelations"):
			writeLinearGQLData(w, relData)
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLError(w, "simulated blocker read failure")
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
	if err == nil {
		t.Fatalf("expected blocker read error; out: %s", out)
	}
	if !strings.Contains(err.Error(), `resolve blockers for issue ENG-1: get blocking issue "blocker-1": graphql error: simulated blocker read failure`) {
		t.Fatalf("error = %q, want contextual blocker read failure", err)
	}
	if strings.Contains(out, "ENG-1") {
		t.Fatalf("list-unblocked-backlog emitted an unchecked candidate: %s", out)
	}
}

func TestLinearListUnblockedBacklogFailsClosedOnIncompleteRelationPayloads(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")
	tests := incompleteRelationPayloads()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query string `json:"query"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch {
				case strings.Contains(req.Query, "ListProjects"):
					writeLinearGQLData(w, proj)
				case strings.Contains(req.Query, "ListBacklogIssues"):
					writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
				case strings.Contains(req.Query, "ListRelations"):
					writeLinearGQLData(w, tt.data)
				default:
					writeLinearGQLData(w, `{}`)
				}
			})

			out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
			if err == nil {
				t.Fatalf("expected incomplete relation response error; out: %s", out)
			}
			if !strings.Contains(err.Error(), "incomplete relations response") {
				t.Fatalf("error = %q, want incomplete response context", err)
			}
			if strings.Contains(out, "ENG-1") {
				t.Fatalf("list-unblocked-backlog emitted an unchecked candidate: %s", out)
			}
		})
	}
}

func TestLinearListUnblockedBacklogFindsBlockerOnLaterRelationPage(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")
	blockerIssue := issueNodeJSON("blocker-1", "ENG-99", "Blocker", "In Progress", "team-1", "ENG", "Engineering")
	var relationCalls int

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, proj)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
		case strings.Contains(req.Query, "ListRelations"):
			relationCalls++
			if req.Variables["inverseRelationsAfter"] == nil {
				writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"inverse-page-1"}}}}`)
				return
			}
			writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[{"id":"rel-1","type":"blocks","issue":{"id":"blocker-1","identifier":"ENG-99"},"createdAt":"2025-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, blockerIssue))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
	if err != nil {
		t.Fatalf("list-unblocked-backlog failed: %v\nout: %s", err, out)
	}
	if arr := decodeJSONArray(t, out); len(arr) != 0 {
		t.Fatalf("got unchecked candidate output %v, want []", arr)
	}
	if relationCalls != 2 {
		t.Fatalf("relation page calls = %d, want 2", relationCalls)
	}
}

func TestLinearListUnblockedBacklogFailsClosedOnTruncatedRelationPage(t *testing.T) {
	proj := `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`
	issue := issueNodeJSON("issue-1", "ENG-1", "Candidate", "Backlog", "team-1", "ENG", "Engineering")
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			writeLinearGQLData(w, proj)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s]}}`, issue))
		case strings.Contains(req.Query, "ListRelations"):
			writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "list-unblocked-backlog", "--project", "TestProject")
	if err == nil {
		t.Fatalf("expected truncated relation response error; out: %s", out)
	}
	if !strings.Contains(err.Error(), "inverseRelations hasNextPage without endCursor") {
		t.Fatalf("error = %q, want unusable cursor context", err)
	}
	if strings.Contains(out, "ENG-1") {
		t.Fatalf("list-unblocked-backlog emitted an unchecked candidate: %s", out)
	}
}

// ─── no API key ───────────────────────────────────────────────────────────────

// TestLinearProxyModeViaDataSource pins the platform-proxy path added by
// ADR-2026-05-12-cli-linear-proxy: when no LINEAR_API_KEY is set and the
// DataSource factory returns an authenticated *afclient.Client, the linear
// subcommand routes its GraphQL through `/api/cli/linear/graphql` with a
// `Bearer <rsk_*>` auth header.
func TestLinearProxyModeViaDataSource(t *testing.T) {
	// No env vars — force the proxy path. WORKER_AUTH_TOKEN is cleared too:
	// path 1.5 (in-box worker JWT) sits AHEAD of the DataSource path, so an
	// ambient token on the developer's shell would silently route this
	// assertion through the wrong tier.
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	t.Setenv("WORKER_AUTH_TOKEN", "")
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		// Mimic Linear's GraphQL envelope for a successful get-issue.
		writeLinearGQLData(w, `{"issue":{"id":"id-1","identifier":"ENG-1","title":"Hello","state":{"name":"Backlog"},"team":{"name":"Engineering"},"project":null,"labels":{"nodes":[]}}}`)
	}))
	t.Cleanup(srv.Close)

	// The DataSource factory returns an *afclient.Client pointed at our
	// test server with an rsk_-style token. CredentialsFromDataSource()
	// extracts (BaseURL, APIToken) and feeds them to linear.NewProxiedClient.
	ds := func() afclient.DataSource {
		return afclient.NewAuthenticatedClient(srv.URL, "rsk_test_token")
	}

	root := New(ds, "donmai")
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "ENG-1"})
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

// TestLinearWorkerAuthTokenProxyMode verifies the in-box cloud path (GAP 1): with no LINEAR_API_KEY and no authenticated DataSource, but with the
// runner-exported WORKER_AUTH_TOKEN (runtime JWT) + DONMAI_API_URL, the linear
// subcommand proxies GraphQL through /api/cli/linear/graphql with a
// `Bearer <jwt>` auth header.
func TestLinearWorkerAuthTokenProxyMode(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		writeLinearGQLData(w, `{"issue":{"id":"id-1","identifier":"ENG-1","title":"Hello","state":{"name":"Backlog"},"team":{"name":"Engineering"},"project":null,"labels":{"nodes":[]}}}`)
	}))
	t.Cleanup(srv.Close)

	// Runner-exported env: worker runtime JWT + platform base URL. No
	// DataSource (nil) — exercises Path 1.5 ahead of the rsk_ DataSource path.
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ3b3JrZXIifQ.sig"
	t.Setenv("WORKER_AUTH_TOKEN", jwt)
	t.Setenv("DONMAI_API_URL", srv.URL)

	root := New(nil, "donmai")
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "ENG-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("worker-auth get-issue failed: %v\nout: %s", err, buf.String())
	}

	if gotPath != "/api/cli/linear/graphql" {
		t.Errorf("path = %q, want %q", gotPath, "/api/cli/linear/graphql")
	}
	if gotAuth != "Bearer "+jwt {
		t.Errorf("Authorization = %q, want Bearer <jwt>", gotAuth)
	}
}

// TestLinearWorkerAuthTokenRejectsMalformedBaseURL is the fail-closed half of
// the in-box tier: when the box was provisioned with a malformed platform
// origin (here a trailing delimiter, the shape that broke a live headless
// dispatch), the command must fail BEFORE any HTTP with an actionable,
// value-free configuration error — not fail late and opaquely inside GraphQL.
func TestLinearWorkerAuthTokenRejectsMalformedBaseURL(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ3b3JrZXIifQ.sig"
	malformed := srv.URL + ";"
	t.Setenv("WORKER_AUTH_TOKEN", jwt)
	t.Setenv("DONMAI_API_URL", malformed)

	root := New(nil, "donmai")
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "ENG-1"})
	err := root.Execute()
	if err == nil {
		t.Fatalf("expected a configuration error for a malformed platform origin, got nil\nout: %s", buf.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("malformed platform origin produced %d HTTP request(s); want 0", hits.Load())
	}
	message := err.Error()
	if !strings.Contains(message, "DONMAI_API_URL") {
		t.Errorf("error should name the env var that supplied the origin, got: %v", err)
	}
	if !strings.Contains(message, "trailing delimiter") {
		t.Errorf("error should be actionable about the required origin shape, got: %v", err)
	}
	if strings.Contains(message, malformed) || strings.Contains(message, jwt) {
		t.Errorf("error leaked the rejected origin or the worker token: %v", err)
	}
}

// TestLinearWorkerAuthTokenRequiresBaseURL pins that WORKER_AUTH_TOKEN alone
// (no DONMAI_API_URL / AGENTFACTORY_API_URL) does NOT activate the in-box tier
// — it falls through to the no-credentials error rather than building a client
// with an empty base URL.
func TestLinearWorkerAuthTokenRequiresBaseURL(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	t.Setenv("WORKER_AUTH_TOKEN", "eyJhbGciOiJIUzI1NiJ9.e30.sig")
	t.Setenv("DONMAI_API_URL", "")
	t.Setenv("AGENTFACTORY_API_URL", "")
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	if _, err := newLinearClient(nil, "donmai"); err == nil {
		t.Fatal("expected error when WORKER_AUTH_TOKEN is set but no platform URL, got nil")
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
		writeLinearGQLData(w, `{"issue":{"id":"id-1","identifier":"ENG-1","title":"X","state":{"name":"Backlog"},"team":{"name":"Engineering"},"project":null,"labels":{"nodes":[]}}}`)
	}))
	t.Cleanup(srv.Close)

	// Direct-path uses the package-level baseURL hook.
	setTestBaseURL(srv.URL)
	t.Cleanup(func() { setTestBaseURL("") })

	// DataSource is non-nil + authenticated, but should be ignored because
	// the env var takes precedence.
	ds := func() afclient.DataSource {
		return afclient.NewAuthenticatedClient("https://platform.example.com", "rsk_should_not_be_used")
	}

	root := New(ds, "donmai")
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "ENG-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("env-precedence get-issue failed: %v\nout: %s", err, buf.String())
	}

	// Direct mode → raw header value (no Bearer prefix) per Linear's API.
	if gotAuth != "lin_api_envvar" {
		t.Errorf("Authorization = %q, want env-var raw value (no Bearer prefix)", gotAuth)
	}
}

func TestLinearNoAPIKey(t *testing.T) {
	// Remove both key env vars, plus the in-box worker credentials that
	// activate path 1.5 ahead of the no-credentials error.
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	t.Setenv("WORKER_AUTH_TOKEN", "")

	// Ensure setTestBaseURL is also reset so no client gets built.
	setTestBaseURL("")
	t.Cleanup(func() { setTestBaseURL("") })

	// Pass a nil DataSource factory — exercises path 3 (no env, no
	// authenticated DataSource → friendly error).
	root := New(nil, "donmai")
	root.SilenceErrors = true
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"get-issue", "ENG-1"})
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
	sourceIssue := issueNodeJSON("source-1", "ENG-50", "Source Issue", "In Progress", "team-1", "ENG", "Engineering")
	blockerIssue := issueNodeJSON("blocker-1", "ENG-99", "Blocker Issue", "Icebox", "team-1", "ENG", "Engineering")
	teamJSON := `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}`

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
			writeLinearGQLData(w, `{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, teamJSON)
		case strings.Contains(req.Query, "ListWorkflowStates"):
			writeLinearGQLData(w, `{"workflowStates":{"nodes":[{"id":"state-icebox","name":"Icebox","type":"triage"}]}}`)
		case strings.Contains(req.Query, "issueLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-nh","name":"Needs Human"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "CreateIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issueCreate":{"success":true,"issue":%s}}`, blockerIssue))
		case strings.Contains(req.Query, "CreateRelation"):
			writeLinearGQLData(w, `{"issueRelationCreate":{"success":true,"issueRelation":{"id":"rel-1","type":"blocks","relatedIssue":{"id":"source-1","identifier":"ENG-50"},"createdAt":"2025-01-01T00:00:00Z"}}}`)
		case strings.Contains(req.Query, "CreateComment"):
			writeLinearGQLData(w, `{"commentCreate":{"success":true,"comment":{"id":"c-1","body":"blocker created","createdAt":"2025-01-01T00:00:00Z"}}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "create-blocker", "ENG-50", "--title", "Blocker Issue")
	if err != nil {
		t.Fatalf("create-blocker failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["identifier"] != "ENG-99" {
		t.Errorf("identifier = %v, want ENG-99", result["identifier"])
	}
	if result["sourceIssue"] != "ENG-50" {
		t.Errorf("sourceIssue = %v, want ENG-50", result["sourceIssue"])
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
	_, err := runLinearCmd(t, "", "create-blocker", "ENG-50")
	if err == nil {
		t.Fatal("expected error when --title is missing")
	}
}

// ─── update-issue --priority / --estimate (C3) ───────────────────────────────

// TestLinearUpdateIssuePriority verifies --priority is forwarded to the
// update mutation. We capture the GraphQL variables and confirm `priority` is set.
func TestLinearUpdateIssuePriority(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Prioritised Issue", "Backlog", "team-1", "ENG", "Engineering")

	var capturedInput map[string]any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "UpdateIssue"):
			inp, _ := req.Variables["input"].(map[string]any)
			capturedInput = inp
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--priority", "1")
	if err != nil {
		t.Fatalf("update-issue --priority failed: %v\nout: %s", err, out)
	}
	// JSON numbers decode as float64.
	if p, ok := capturedInput["priority"].(float64); !ok || int(p) != 1 {
		t.Errorf("update input priority = %v; want 1", capturedInput["priority"])
	}
	// estimate must NOT be present when --estimate is not passed.
	if _, has := capturedInput["estimate"]; has {
		t.Errorf("update input should not have estimate when flag omitted: %#v", capturedInput)
	}
}

func TestLinearUpdateIssueEstimate(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Estimated Issue", "Backlog", "team-1", "ENG", "Engineering")

	var capturedInput map[string]any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "UpdateIssue"):
			inp, _ := req.Variables["input"].(map[string]any)
			capturedInput = inp
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--estimate", "3")
	if err != nil {
		t.Fatalf("update-issue --estimate failed: %v\nout: %s", err, out)
	}
	if e, ok := capturedInput["estimate"].(float64); !ok || int(e) != 3 {
		t.Errorf("update input estimate = %v; want 3", capturedInput["estimate"])
	}
	if _, has := capturedInput["priority"]; has {
		t.Errorf("update input should not have priority when flag omitted: %#v", capturedInput)
	}
}

// TestLinearUpdateIssuePriorityAndEstimate verifies that --priority 0 is sent
// (0 = "no priority" is a valid Linear value and must not be silently dropped).
func TestLinearUpdateIssuePriorityZero(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "No Priority Issue", "Backlog", "team-1", "ENG", "Engineering")

	var capturedInput map[string]any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "UpdateIssue"):
			capturedInput, _ = req.Variables["input"].(map[string]any)
			writeLinearGQLData(w, fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	// --priority 0 explicitly: must be forwarded (not treated as "not set").
	out, err := runLinearCmd(t, "", "update-issue", "ENG-1", "--priority", "0")
	if err != nil {
		t.Fatalf("update-issue --priority=0 failed: %v\nout: %s", err, out)
	}
	p, has := capturedInput["priority"]
	if !has {
		t.Fatalf("update input missing priority when --priority=0 was passed: %#v", capturedInput)
	}
	if pf, ok := p.(float64); !ok || int(pf) != 0 {
		t.Errorf("update input priority = %v; want 0", p)
	}
}

// ─── update-issue --status (contract alias for --state) ──────────────────────

// TestLinearUpdateIssueStatus verifies that --status (the contract-canonical flag)
// resolves the state and forwards its ID exactly like --state does.
func TestLinearUpdateIssueStatus(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantState string
		wantErr   bool
	}{
		{
			name:      "--status Cancelled resolves state",
			args:      []string{"update-issue", "ENG-1", "--status", "Cancelled"},
			wantState: "Cancelled",
		},
		{
			name:      "--status Accepted resolves state",
			args:      []string{"update-issue", "ENG-1", "--status", "Accepted"},
			wantState: "Accepted",
		},
		{
			name:      "--state still works unchanged",
			args:      []string{"update-issue", "ENG-1", "--state", "Finished"},
			wantState: "Finished",
		},
		{
			name:    "--status and --state with different values is an error",
			args:    []string{"update-issue", "ENG-1", "--status", "Cancelled", "--state", "Finished"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			stateName := tc.wantState
			if stateName == "" {
				stateName = "Backlog" // placeholder for wantErr cases that never reach the query
			}
			issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", stateName, "team-1", "ENG", "Engineering")
			handler := &multiHandler{
				responses: map[string]string{
					"GetIssue":           fmt.Sprintf(`{"issue":%s}`, issueJSON),
					"ListWorkflowStates": fmt.Sprintf(`{"workflowStates":{"nodes":[{"id":"state-x","name":%q,"type":"completed"}]}}`, stateName),
					"UpdateIssue":        fmt.Sprintf(`{"issueUpdate":{"success":true,"issue":%s}}`, issueJSON),
				},
			}
			setupLinearTest(t, handler.ServeHTTP)

			out, err := runLinearCmd(t, "", tc.args...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error but got none; output: %s", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("update-issue failed: %v\nout: %s", err, out)
			}
			result := decodeJSON(t, out)
			if result["status"] != tc.wantState {
				t.Errorf("status = %v; want %v", result["status"], tc.wantState)
			}
		})
	}
}

// ─── list-labels (C4) ────────────────────────────────────────────────────────

func TestLinearListLabels(t *testing.T) {
	labelsData := `{"issueLabels":{"nodes":[
		{"id":"label-1","name":"Bug"},
		{"id":"label-2","name":"Feature"},
		{"id":"label-3","name":"Needs Human"}
	],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, labelsData)
	})

	out, err := runLinearCmd(t, "", "list-labels")
	if err != nil {
		t.Fatalf("list-labels failed: %v\nout: %s", err, out)
	}

	arr := decodeJSONArray(t, out)
	if len(arr) != 3 {
		t.Fatalf("got %d labels; want 3", len(arr))
	}
	// Output is sorted by name.
	first := arr[0].(map[string]any)
	if first["name"] != "Bug" {
		t.Errorf("first label name = %v; want Bug", first["name"])
	}
	if first["id"] != "label-1" {
		t.Errorf("first label id = %v; want label-1", first["id"])
	}
}

func TestLinearListLabelsScopesByTeam(t *testing.T) {
	var sawLabelFilter map[string]any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch {
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			sawLabelFilter, _ = req.Variables["filter"].(map[string]any)
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-1","name":"Bug"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "list-labels", "--team", "ENG")
	if err != nil {
		t.Fatalf("list-labels --team failed: %v\nout: %s", err, out)
	}
	if sawLabelFilter == nil {
		t.Fatal("list-labels --team sent no label filter")
	}
	arr := decodeJSONArray(t, out)
	if len(arr) != 1 || arr[0].(map[string]any)["id"] != "label-1" {
		t.Fatalf("output = %#v", arr)
	}
}

func TestLinearListLabelsHelpDescribesTeamFilter(t *testing.T) {
	out, err := runLinearCmd(t, "", "list-labels", "--help")
	if err != nil {
		t.Fatalf("list-labels --help: %v", err)
	}
	if !strings.Contains(out, "Canonical team key or UUID to filter labels") {
		t.Fatalf("help = %q, want team filter description", out)
	}
	if strings.Contains(out, "currently unused") || strings.Contains(out, "labels are org-wide") {
		t.Fatalf("help retains obsolete team-scope claim: %q", out)
	}
}

func TestLinearListTeams(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.Query, "ListTeams") {
			t.Fatalf("query = %q, want ListTeams", req.Query)
		}
		writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-b","key":"B","name":"Beta"},{"id":"team-a","key":"A","name":"Alpha"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})

	out, err := runLinearCmd(t, "", "list-teams")
	if err != nil {
		t.Fatalf("list-teams failed: %v\nout: %s", err, out)
	}
	arr := decodeJSONArray(t, out)
	if len(arr) != 2 {
		t.Fatalf("teams = %v, want 2 entries", arr)
	}
	first := arr[0].(map[string]any)
	if first["id"] != "team-a" || first["key"] != "A" || first["name"] != "Alpha" {
		t.Fatalf("first team = %#v", first)
	}
}

func TestLinearListProjectsFlattensMultiTeamMembership(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.Query, "ListProjects") {
			t.Fatalf("query = %q, want ListProjects", req.Query)
		}
		writeLinearGQLData(w, `{"projects":{"nodes":[
			{"id":"project-b","name":"Beta","state":"planned","teams":{"nodes":[{"id":"team-2","key":"OPS","name":"Operations"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}},
			{"id":"project-a","name":"Alpha","state":"started","teams":{"nodes":[{"id":"team-2","key":"OPS","name":"Operations"},{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})

	out, err := runLinearCmd(t, "", "list-projects")
	if err != nil {
		t.Fatalf("list-projects failed: %v\nout: %s", err, out)
	}
	arr := decodeJSONArray(t, out)
	if len(arr) != 3 {
		t.Fatalf("projects = %v, want one row per project-team membership", arr)
	}
	want := []struct{ id, name, teamKey, state string }{
		{"project-a", "Alpha", "ENG", "started"},
		{"project-a", "Alpha", "OPS", "started"},
		{"project-b", "Beta", "OPS", "planned"},
	}
	for i, expected := range want {
		got := arr[i].(map[string]any)
		if got["id"] != expected.id || got["name"] != expected.name || got["teamKey"] != expected.teamKey || got["state"] != expected.state {
			t.Fatalf("project row %d = %#v, want %#v", i, got, expected)
		}
	}
}

func TestLinearListProjectsTeamFilter(t *testing.T) {
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListProjects"):
			filter, _ := req.Variables["filter"].(map[string]any)
			accessible, _ := filter["accessibleTeams"].(map[string]any)
			some, _ := accessible["some"].(map[string]any)
			id, _ := some["id"].(map[string]any)
			if id["eq"] != "team-1" {
				t.Fatalf("project filter = %#v, want team-1", filter)
			}
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"project-a","name":"Alpha","state":"started","teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"},{"id":"team-2","key":"OPS","name":"Operations"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "list-projects", "--team", "ENG")
	if err != nil {
		t.Fatalf("list-projects --team failed: %v\nout: %s", err, out)
	}
	arr := decodeJSONArray(t, out)
	if len(arr) != 1 {
		t.Fatalf("projects = %v, want one team-filtered membership", arr)
	}
	if got := arr[0].(map[string]any); got["teamKey"] != "ENG" || got["state"] != "started" {
		t.Fatalf("project = %#v", got)
	}
}

func TestLinearListProjectsWritesNoPartialOutputOnFailure(t *testing.T) {
	var calls int
	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"project-a","name":"Alpha","state":"started","teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`)
			return
		}
		writeLinearGQLError(w, "upstream unavailable")
	})

	out, err := runLinearCmd(t, "", "list-projects")
	if err == nil || !strings.Contains(err.Error(), "list projects") {
		t.Fatalf("list-projects error = %v, want contextual failure", err)
	}
	if out != "" {
		t.Fatalf("list-projects wrote partial output: %q", out)
	}
}

// ─── apply-label (C4) ────────────────────────────────────────────────────────

func TestLinearApplyLabel(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	// Issue already has label "Feature" from issueNodeJSON fixture.
	// We apply "Bug" which doesn't yet exist on the issue.
	labelsData := `{"issueLabels":{"nodes":[{"id":"label-bug","name":"Bug"},{"id":"label-1","name":"Feature"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`
	updatedJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")

	var capturedIssueID, capturedLabelID string
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabels"):
			writeLinearGQLData(w, labelsData)
		case strings.Contains(req.Query, "issueAddLabel"):
			capturedIssueID, _ = req.Variables["id"].(string)
			capturedLabelID, _ = req.Variables["labelId"].(string)
			writeLinearGQLData(w, fmt.Sprintf(`{"issueAddLabel":{"success":true,"issue":%s}}`, updatedJSON))
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Bug")
	if err != nil {
		t.Fatalf("apply-label failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["appliedLabel"] != "Bug" {
		t.Errorf("appliedLabel = %v; want Bug", result["appliedLabel"])
	}
	if result["alreadyApplied"] != false {
		t.Errorf("alreadyApplied = %v; want false", result["alreadyApplied"])
	}
	if result["createdLabel"] != false {
		t.Errorf("createdLabel = %v; want false", result["createdLabel"])
	}
	if capturedIssueID != "issue-1" || capturedLabelID != "label-bug" {
		t.Errorf("issueAddLabel variables = (%q, %q)", capturedIssueID, capturedLabelID)
	}
}

func TestLinearApplyLabelMissingLabel(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "apply-label", "ENG-1")
	if err == nil {
		t.Fatal("expected error when --label flag is missing")
	}
}

func TestLinearApplyLabelNotFound(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	labelsData := `{"issueLabels":{"nodes":[{"id":"label-1","name":"Feature"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`

	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabels"):
			writeLinearGQLData(w, labelsData)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	_, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "NonExistentLabel")
	if err == nil {
		t.Fatal("expected error when label does not exist in workspace")
	}
	if !strings.Contains(err.Error(), "NonExistentLabel") {
		t.Errorf("error should mention the label name; got: %v", err)
	}
}

func TestLinearApplyLabelCreateUsesIssueTeam(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	var createInput map[string]any
	createCalls := 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			createCalls++
			createInput, _ = req.Variables["input"].(map[string]any)
			writeLinearGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"label-security","name":"Security"}}}`)
		case strings.Contains(req.Query, "issueAddLabel"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issueAddLabel":{"success":true,"issue":%s}}`, issueJSON))
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Security", "--create")
	if err != nil {
		t.Fatalf("apply-label --create failed: %v\nout: %s", err, out)
	}
	if createCalls != 1 || createInput["name"] != "Security" || createInput["teamId"] != "team-1" {
		t.Fatalf("create calls/input = %d/%#v", createCalls, createInput)
	}
	if got := decodeJSON(t, out)["createdLabel"]; got != true {
		t.Fatalf("createdLabel = %v, want true", got)
	}
}

func TestLinearApplyLabelCreateDoesNotDuplicateApplicableLabel(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	createCalls, addCalls := 0, 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-1","name":"Feature"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			createCalls++
		case strings.Contains(req.Query, "issueAddLabel"):
			addCalls++
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "feature", "--create")
	if err != nil {
		t.Fatalf("apply-label existing label: %v", err)
	}
	result := decodeJSON(t, out)
	if createCalls != 0 || addCalls != 0 {
		t.Fatalf("create/add calls = %d/%d, want 0/0", createCalls, addCalls)
	}
	if result["alreadyApplied"] != true || result["createdLabel"] != false {
		t.Fatalf("result = %#v", result)
	}
}

func TestLinearApplyLabelCreateFailureDoesNotMutateIssue(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	labelReads, issueMutations := 0, 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			labelReads++
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			writeLinearGQLError(w, "label creation rejected")
		case strings.Contains(req.Query, "issueAddLabel"), strings.Contains(req.Query, "issueUpdate"):
			issueMutations++
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	_, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Security", "--create")
	if err == nil || !strings.Contains(err.Error(), "create label") {
		t.Fatalf("error = %v, want contextual creation failure", err)
	}
	if labelReads != 2 || issueMutations != 0 {
		t.Fatalf("label reads/issue mutations = %d/%d, want 2/0", labelReads, issueMutations)
	}
}

func TestLinearApplyLabelCreateReportsAuthorizationFailure(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	labelReads, issueMutations := 0, 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			labelReads++
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			w.WriteHeader(http.StatusForbidden)
		case strings.Contains(req.Query, "issueAddLabel"), strings.Contains(req.Query, "issueUpdate"):
			issueMutations++
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	_, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Security", "--create")
	if err == nil || !strings.Contains(err.Error(), "not authorized") || !strings.Contains(err.Error(), "workspace admin") {
		t.Fatalf("error = %v, want actionable authorization failure", err)
	}
	if labelReads != 2 || issueMutations != 0 {
		t.Fatalf("label reads/issue mutations = %d/%d, want 2/0", labelReads, issueMutations)
	}
}

func TestLinearApplyLabelCreateRecoversDuplicateRace(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	labelReads, addCalls := 0, 0
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			labelReads++
			if labelReads == 1 {
				writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
				return
			}
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[{"id":"label-winner","name":"Security"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			writeLinearGQLError(w, "Label name must be unique")
		case strings.Contains(req.Query, "issueAddLabel"):
			addCalls++
			writeLinearGQLData(w, fmt.Sprintf(`{"issueAddLabel":{"success":true,"issue":%s}}`, issueJSON))
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	out, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Security", "--create")
	if err != nil {
		t.Fatalf("apply-label race recovery: %v", err)
	}
	if labelReads != 2 || addCalls != 1 {
		t.Fatalf("label reads/add calls = %d/%d, want 2/1", labelReads, addCalls)
	}
	if got := decodeJSON(t, out)["createdLabel"]; got != false {
		t.Fatalf("createdLabel = %v, want false for race winner", got)
	}
}

func TestLinearApplyLabelReportsCreateThenApplyPartialFailure(t *testing.T) {
	issueJSON := issueNodeJSON("issue-1", "ENG-1", "Issue", "Backlog", "team-1", "ENG", "Engineering")
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "GetIssue"):
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, issueJSON))
		case strings.Contains(req.Query, "ListTeams"):
			writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "ListLabels"):
			writeLinearGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
		case strings.Contains(req.Query, "issueLabelCreate"):
			writeLinearGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"label-security","name":"Security"}}}`)
		case strings.Contains(req.Query, "issueAddLabel"):
			writeLinearGQLError(w, "issue update rejected")
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	})

	_, err := runLinearCmd(t, "", "apply-label", "ENG-1", "--label", "Security", "--create")
	if err == nil || !strings.Contains(err.Error(), "was created") || !strings.Contains(err.Error(), "could not be applied") {
		t.Fatalf("error = %v, want explicit partial-state failure", err)
	}
}

// ─── comment (grooming contract verb) ────────────────────────────────────────

func TestLinearComment(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	commentData := fmt.Sprintf(`{"commentCreate":{"success":true,"comment":{"id":"c-run","body":"Grooming run summary","createdAt":%q,"user":null}}}`, now)

	setupLinearTest(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLinearGQLData(w, commentData)
	})

	out, err := runLinearCmd(t, "", "comment", "ENG-1", "--body", "Grooming run summary")
	if err != nil {
		t.Fatalf("comment failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	if result["id"] != "c-run" {
		t.Errorf("id = %v; want c-run", result["id"])
	}
	if result["body"] != "Grooming run summary" {
		t.Errorf("body = %v; want Grooming run summary", result["body"])
	}
}

func TestLinearCommentMissingBody(t *testing.T) {
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "comment", "ENG-1")
	if err == nil {
		t.Fatal("expected error when --body is missing from comment command")
	}
}

// ─── list-sub-issues parentId per sub-issue ──────────────────────────────────

func TestLinearListSubIssuesParentIDPerSubIssue(t *testing.T) {
	parentJSON := issueNodeJSON("parent-1", "ENG-1", "Parent", "Backlog", "team-1", "ENG", "Engineering")
	childJSON := issueNodeJSON("child-1", "ENG-2", "Child", "Backlog", "team-1", "ENG", "Engineering")

	handler := &multiHandler{
		responses: map[string]string{
			"GetIssue":      fmt.Sprintf(`{"issue":%s}`, parentJSON),
			"ListSubIssues": fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, childJSON),
		},
	}
	setupLinearTest(t, handler.ServeHTTP)

	out, err := runLinearCmd(t, "", "list-sub-issues", "ENG-1")
	if err != nil {
		t.Fatalf("list-sub-issues failed: %v\nout: %s", err, out)
	}

	result := decodeJSON(t, out)
	subIssues, ok := result["subIssues"].([]any)
	if !ok || len(subIssues) != 1 {
		t.Fatalf("subIssues = %v; want 1 entry", result["subIssues"])
	}
	sub := subIssues[0].(map[string]any)
	// Each sub-issue must carry parentId per the grooming CLI contract.
	if _, has := sub["parentId"]; !has {
		t.Errorf("sub-issue missing parentId field: %#v", sub)
	}
	if sub["parentId"] != "parent-1" {
		t.Errorf("sub-issue parentId = %v; want parent-1", sub["parentId"])
	}
	// relations field must also be present (empty slice).
	if _, has := sub["relations"]; !has {
		t.Errorf("sub-issue missing relations field: %#v", sub)
	}
}

// ─── env defaults for grooming verbs (A2 donmai half) ────────────────────────

// TestLinearListBacklogIssuesEnvDefaults verifies that DONMAI_LINEAR_PROJECT
// and DONMAI_LINEAR_TEAM are used when --project/--team flags are omitted.
// NOTE: must NOT call t.Parallel() — uses t.Setenv.
func TestLinearListBacklogIssuesEnvDefaults(t *testing.T) {
	t.Setenv("DONMAI_LINEAR_PROJECT", "EnvProject")
	t.Setenv("DONMAI_LINEAR_TEAM", "")

	var sawProjectFilter string
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			// Capture the name filter to confirm env var was used.
			sawProjectFilter = projectFilterEqIgnoreCase(req.Variables, "name")
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-env","name":"EnvProject"}]}}`)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, `{"issues":{"nodes":[]}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	// No --project flag: must fall back to DONMAI_LINEAR_PROJECT.
	out, err := runLinearCmd(t, "", "list-backlog-issues")
	if err != nil {
		t.Fatalf("list-backlog-issues from DONMAI_LINEAR_PROJECT failed: %v\nout: %s", err, out)
	}
	if sawProjectFilter != "EnvProject" {
		t.Errorf("project filter = %q; want EnvProject (from DONMAI_LINEAR_PROJECT)", sawProjectFilter)
	}
}

// TestLinearListBacklogIssuesMissingProjectWithoutEnv verifies the fail-loud
// error when neither --project nor DONMAI_LINEAR_PROJECT is set.
func TestLinearListBacklogIssuesMissingProjectWithoutEnv(t *testing.T) {
	t.Setenv("DONMAI_LINEAR_PROJECT", "")
	setupLinearTest(t, func(_ http.ResponseWriter, _ *http.Request) {})
	_, err := runLinearCmd(t, "", "list-backlog-issues")
	if err == nil {
		t.Fatal("expected error when --project and DONMAI_LINEAR_PROJECT are both absent")
	}
}

// TestLinearListUnblockedBacklogEnvDefaults mirrors the test above for list-unblocked-backlog.
func TestLinearListUnblockedBacklogEnvDefaults(t *testing.T) {
	t.Setenv("DONMAI_LINEAR_PROJECT", "EnvProject2")
	t.Setenv("DONMAI_LINEAR_TEAM", "")

	var sawProjectFilter string
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case strings.Contains(req.Query, "ListProjects"):
			sawProjectFilter = projectFilterEqIgnoreCase(req.Variables, "name")
			writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-env2","name":"EnvProject2"}]}}`)
		case strings.Contains(req.Query, "ListBacklogIssues"):
			writeLinearGQLData(w, `{"issues":{"nodes":[]}}`)
		case strings.Contains(req.Query, "ListRelations"):
			writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		default:
			writeLinearGQLData(w, `{}`)
		}
	})

	out, err := runLinearCmd(t, "", "list-unblocked-backlog")
	if err != nil {
		t.Fatalf("list-unblocked-backlog from DONMAI_LINEAR_PROJECT failed: %v\nout: %s", err, out)
	}
	if sawProjectFilter != "EnvProject2" {
		t.Errorf("project filter = %q; want EnvProject2 (from DONMAI_LINEAR_PROJECT)", sawProjectFilter)
	}
}

// ─── linear.Linear interface compliance ──────────────────────────────────────

// TestLinearClientImplementsInterface verifies *linear.Client satisfies the full interface.
func TestLinearClientImplementsInterface(_ *testing.T) {
	var _ linear.Linear = (*linear.Client)(nil)
}
