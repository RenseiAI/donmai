package linearcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const readinessTeamID = "11111111-1111-4111-8111-111111111111"

type blockerReadinessFixture struct {
	t              *testing.T
	states         []string
	mu             sync.Mutex
	issueReads     map[string]int
	beforeResponse func(string, map[string]any)
}

func (f *blockerReadinessFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Errorf("decode request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/" || r.Header.Get("Authorization") != "test-fixture-key-not-a-secret" {
		f.t.Errorf("unexpected transport: %s %s", r.Method, r.URL.Path)
	}
	if f.beforeResponse != nil {
		f.beforeResponse(request.Query, request.Variables)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mainIssue := issueNodeJSON("candidate-main", "ENG-1", "Candidate", "Backlog", readinessTeamID, "ENG", "Engineering")
	readyIssue := issueNodeJSON("candidate-ready", "ENG-2", "Independent candidate", "Backlog", readinessTeamID, "ENG", "Engineering")
	switch {
	case strings.Contains(request.Query, "query GetIssue("):
		id, _ := request.Variables["id"].(string)
		f.issueReads[id]++
		if id == "ENG-1" {
			writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, mainIssue))
			return
		}
		for i, state := range f.states {
			if id == fmt.Sprintf("dependency-%d", i) {
				node := issueNodeJSON(id, fmt.Sprintf("CURRENT-%d", i), fmt.Sprintf("Current dependency %d", i), state, readinessTeamID, "ENG", "Engineering")
				writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, node))
				return
			}
		}
		f.t.Errorf("unexpected issue lookup %q (outgoing and nonblocking relations must not be fetched)", id)
		writeLinearGQLError(w, "unexpected issue lookup")
	case strings.Contains(request.Query, "query ListProjects("):
		if projectFilterEqIgnoreCase(request.Variables, "name") != "TestProject" {
			f.t.Errorf("project scope lost: %v", request.Variables)
		}
		filter, _ := request.Variables["filter"].(map[string]any)
		teams, _ := filter["accessibleTeams"].(map[string]any)
		some, _ := teams["some"].(map[string]any)
		id, _ := some["id"].(map[string]any)
		if id["eq"] != readinessTeamID {
			f.t.Errorf("team scope lost: %v", request.Variables)
		}
		writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"project-fixture","name":"TestProject"}]}}`)
	case strings.Contains(request.Query, "query ListBacklogIssues("):
		if request.Variables["projectId"] != "project-fixture" ||
			!reflect.DeepEqual(request.Variables["states"], []any{"Backlog"}) ||
			!strings.Contains(request.Query, "parent: { null: true }") {
			f.t.Errorf("candidate selection changed: variables=%v query=%q", request.Variables, request.Query)
		}
		writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s,%s]}}`, mainIssue, readyIssue))
	case strings.Contains(request.Query, "query ListRelations("):
		id, _ := request.Variables["issueId"].(string)
		if id == "candidate-ready" {
			writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
			return
		}
		if id != "candidate-main" {
			f.t.Errorf("wrong relation target %q", id)
		}
		var inverse []string
		for i := range f.states {
			inverse = append(inverse, fmt.Sprintf(`{"id":"relation-%d","type":"blocks","issue":{"id":"dependency-%d","identifier":"STALE-%d"},"createdAt":"2026-09-30T10:00:00Z"}`, i, i, i))
		}
		inverse = append(inverse, `{"id":"related","type":"related","issue":{"id":"nonblocking-dependency","identifier":"STALE-RELATED"},"createdAt":"2026-09-30T10:00:00Z"}`)
		writeLinearGQLData(w, fmt.Sprintf(`{"issue":{"relations":{"nodes":[{"id":"outgoing","type":"blocks","relatedIssue":{"id":"outgoing-dependency","identifier":"STALE-OUTGOING"},"createdAt":"2026-09-30T10:00:00Z"}],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`, strings.Join(inverse, ",")))
	default:
		f.t.Errorf("unexpected operation: %s", request.Query)
		writeLinearGQLError(w, "unexpected operation")
	}
}

func runBlockerReadinessCommand(ctx context.Context, command string) (string, error) {
	root := New(nil, "donmai")
	root.SilenceErrors = true
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	args := []string{command, "ENG-1"}
	if command == "list-unblocked-backlog" {
		args = []string{command, "--project", "TestProject", "--team", readinessTeamID, "--statuses", "Backlog"}
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func TestLinearBlockerReadinessNamedStates(t *testing.T) {
	for _, command := range []string{"check-blocked", "list-unblocked-backlog"} {
		for _, tc := range []struct {
			name        string
			states      []string
			wantIndexes []int
		}{
			{name: "Done", states: []string{"Done"}},
			{name: "Accepted", states: []string{"Accepted"}},
			{name: "both_ready", states: []string{"Done", "Accepted"}},
			{name: "Started", states: []string{"Started"}, wantIndexes: []int{0}},
			{name: "Backlog", states: []string{"Backlog"}, wantIndexes: []int{0}},
			{name: "Icebox", states: []string{"Icebox"}, wantIndexes: []int{0}},
			{name: "Finished", states: []string{"Finished"}, wantIndexes: []int{0}},
			{name: "Delivered", states: []string{"Delivered"}, wantIndexes: []int{0}},
			{name: "mixed", states: []string{"Done", "Accepted", "Finished", "Delivered", "Started"}, wantIndexes: []int{2, 3, 4}},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				fixture := &blockerReadinessFixture{t: t, states: tc.states, issueReads: map[string]int{}}
				setupLinearTest(t, fixture.serveHTTP)
				out, err := runBlockerReadinessCommand(context.Background(), command)
				if err != nil {
					t.Fatalf("command failed: %v output=%q", err, out)
				}
				if command == "check-blocked" {
					var result struct {
						IssueID   string              `json:"issueId"`
						Blocked   bool                `json:"blocked"`
						BlockedBy []map[string]string `json:"blockedBy"`
					}
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatal(err)
					}
					want := make([]map[string]string, 0, len(tc.wantIndexes))
					for _, i := range tc.wantIndexes {
						want = append(want, map[string]string{"identifier": fmt.Sprintf("CURRENT-%d", i), "title": fmt.Sprintf("Current dependency %d", i), "status": tc.states[i]})
					}
					if result.IssueID != "ENG-1" || result.Blocked != (len(want) > 0) || !reflect.DeepEqual(result.BlockedBy, want) {
						t.Fatalf("active blockers=%s; want current unresolved rows %v", out, want)
					}
				} else {
					rows := decodeJSONArray(t, out)
					wantIDs := []string{"ENG-1", "ENG-2"}
					if len(tc.wantIndexes) > 0 {
						wantIDs = []string{"ENG-2"}
					}
					var gotIDs []string
					for _, row := range rows {
						item := row.(map[string]any)
						gotIDs = append(gotIDs, item["identifier"].(string))
						if item["blocked"] != false || !reflect.DeepEqual(item["blockedBy"], []any{}) || item["status"] != "Backlog" {
							t.Errorf("unblocked candidate shape changed: %v", item)
						}
					}
					if !reflect.DeepEqual(gotIDs, wantIDs) {
						t.Fatalf("unblocked identifiers=%v want=%v output=%s", gotIDs, wantIDs, out)
					}
				}
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				wantReads := map[string]int{}
				if command == "check-blocked" {
					wantReads["ENG-1"] = 1
				}
				for i := range tc.states {
					wantReads[fmt.Sprintf("dependency-%d", i)] = 1
				}
				if !reflect.DeepEqual(fixture.issueReads, wantReads) {
					t.Fatalf("dependency reads=%v want=%v", fixture.issueReads, wantReads)
				}
			})
		}
	}
}

func TestLinearBlockerReadinessCancellation(t *testing.T) {
	for _, command := range []string{"check-blocked", "list-unblocked-backlog"} {
		for _, stage := range []string{"relations", "dependency"} {
			t.Run(command+"/"+stage, func(t *testing.T) {
				started, finish := make(chan struct{}), make(chan struct{})
				fixture := &blockerReadinessFixture{t: t, states: []string{"Done"}, issueReads: map[string]int{}}
				fixture.beforeResponse = func(query string, variables map[string]any) {
					if (stage == "relations" && strings.Contains(query, "query ListRelations(")) ||
						(stage == "dependency" && strings.Contains(query, "query GetIssue(") && variables["id"] == "dependency-0") {
						close(started)
						<-finish
					}
				}
				setupLinearTest(t, fixture.serveHTTP)
				t.Cleanup(func() { close(finish) })
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				type commandResult struct {
					output string
					err    error
				}
				result := make(chan commandResult, 1)
				go func() {
					out, err := runBlockerReadinessCommand(ctx, command)
					result <- commandResult{output: out, err: err}
				}()
				select {
				case <-started:
				case r := <-result:
					t.Fatalf("command returned before selected read: err=%v output=%q", r.err, r.output)
				case <-time.After(3 * time.Second):
					t.Fatal("selected readiness read did not start")
				}
				cancel()
				select {
				case r := <-result:
					if !errors.Is(r.err, context.Canceled) || r.output != "" {
						t.Fatalf("canceled read returned success: err=%v output=%q", r.err, r.output)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("readiness command ignored cancellation")
				}
			})
		}
	}
}

func TestLinearBlockerReadinessHelp(t *testing.T) {
	for _, command := range []string{"check-blocked", "list-unblocked-backlog"} {
		t.Run(command, func(t *testing.T) {
			out, err := runLinearCmd(t, "", command, "--help")
			if err != nil {
				t.Fatal(err)
			}
			for _, phrase := range []string{"active blockers", "Done or Accepted", "Finished and Delivered", "list-relations", "historical relations"} {
				if !strings.Contains(out, phrase) {
					t.Errorf("help missing %q: %s", phrase, out)
				}
			}
		})
	}
}
