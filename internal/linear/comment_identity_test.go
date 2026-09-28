package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetIssueCommentsProxyRequiresNegotiatedStrictCapability(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		writeGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewProxiedClient(server.URL, "rsk_comment_fixture")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = server.Client()
	comments, err := client.GetIssueComments(context.Background(), "ENG-1")
	if err == nil || comments != nil || posts != 0 {
		t.Fatalf("proxied comment read bypassed strict capability: comments=%v err=%v posts=%d", comments, err, posts)
	}
}

func TestGetIssueCommentsCompleteIdentityAndRevision(t *testing.T) {
	const issueRef = "ENG-1"
	var cursors []any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !strings.Contains(req.Query, "comments(first: 100, after: $after, includeArchived: true)") ||
			!strings.Contains(req.Query, "updatedAt") || !strings.Contains(req.Query, "user { id name }") ||
			req.Variables["issueId"] != issueRef {
			t.Errorf("comment query omitted scope, pagination, identity, or revision: %+v", req)
		}
		cursors = append(cursors, req.Variables["after"])
		if len(cursors) == 1 {
			writeGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[
				{"id":"human","body":"Human text","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":{"id":"user-1","name":"Alice"}},
				{"id":"bot","body":"Bot text","createdAt":"2026-09-27T10:01:00Z","updatedAt":"2026-09-27T10:01:00Z","user":{"id":"bot-1","name":"Automaton"}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"page-two"}}}}`)
			return
		}
		writeGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[
			{"id":"edited","body":"Revised text","createdAt":"2026-09-27T10:02:00Z","updatedAt":"2026-09-28T11:00:00Z","user":{"id":"user-2","name":"Editor"}},
			{"id":"deleted-author","body":"Retained text","createdAt":"2026-09-27T10:03:00Z","updatedAt":"2026-09-27T10:03:00Z","user":null}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	comments, err := client.GetIssueComments(context.Background(), issueRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 4 || len(cursors) != 2 || cursors[0] != nil || cursors[1] != "page-two" {
		t.Fatalf("incomplete comment history: count=%d cursors=%v", len(comments), cursors)
	}
	if comments[0].ID != "human" || comments[0].Body != "Human text" || comments[0].CreatedAt == nil {
		t.Fatalf("legacy comment fields drifted: %+v", comments[0])
	}
	raw, err := json.Marshal(comments)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ id, authorID, status, updated string }{
		{"human", "user-1", "available", "2026-09-27T10:00:00Z"},
		{"bot", "bot-1", "available", "2026-09-27T10:01:00Z"},
		{"edited", "user-2", "available", "2026-09-28T11:00:00Z"},
		{"deleted-author", "", "unavailable", "2026-09-27T10:03:00Z"},
	} {
		row := rows[i]
		if row["id"] != want.id || row["authorStatus"] != want.status || row["updatedAt"] != want.updated {
			t.Fatalf("comment identity/revision %d: got=%v want=%+v", i, row, want)
		}
		if want.authorID == "" {
			if user, present := row["user"]; !present || user != nil {
				t.Fatalf("unavailable author was not explicit null: %v", row)
			}
		} else if user, ok := row["user"].(map[string]any); !ok || user["id"] != want.authorID {
			t.Fatalf("author identity %d: %v", i, row)
		}
	}
}

func TestGetIssueCommentsRefusesIncompleteOrContradictoryPages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []string
	}{
		{"missing_issue", []string{`{"issue":null}`}},
		{"missing_page_info", []string{`{"issue":{"id":"issue-1","comments":{"nodes":[]}}}`}},
		{"missing_cursor", []string{`{"issue":{"id":"issue-1","comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`}},
		{"missing_revision", []string{`{"issue":{"id":"issue-1","comments":{"nodes":[{"id":"c1","body":"text","createdAt":"2026-09-27T10:00:00Z","user":{"id":"u1","name":"Alice"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`}},
		{"missing_user_id", []string{`{"issue":{"id":"issue-1","comments":{"nodes":[{"id":"c1","body":"text","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":{"name":"Alice"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`}},
		{"issue_changed_between_pages", []string{
			`{"issue":{"id":"issue-1","comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`,
			`{"issue":{"id":"issue-2","comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		}},
		{"duplicate_comment_id", []string{
			`{"issue":{"id":"issue-1","comments":{"nodes":[{"id":"c1","body":"a","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":null}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`,
			`{"issue":{"id":"issue-1","comments":{"nodes":[{"id":"c1","body":"b","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":null}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				page := calls
				calls++
				if page >= len(tc.pages) {
					t.Errorf("unexpected comment page %d", page)
					return
				}
				writeGQLData(w, tc.pages[page])
			})
			comments, err := client.GetIssueComments(context.Background(), "ENG-1")
			if err == nil || comments != nil {
				t.Fatalf("incomplete/contradictory comment listing accepted: comments=%+v err=%v", comments, err)
			}
		})
	}
}
