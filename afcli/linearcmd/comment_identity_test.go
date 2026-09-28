package linearcmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestLinearListCommentsIssueScopedIdentityAndRequestedID(t *testing.T) {
	var mu sync.Mutex
	var cursors []any
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if r.Method != http.MethodPost || !strings.Contains(req.Query, "issue(id: $issueId)") ||
			req.Variables["issueId"] != "ENG-1" {
			t.Errorf("comment request escaped issue scope: %+v", req)
		}
		mu.Lock()
		cursors = append(cursors, req.Variables["after"])
		mu.Unlock()
		if req.Variables["after"] == nil {
			writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[
				{"id":"human","body":"Human text","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":{"id":"u-1","name":"Alice"}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`)
			return
		}
		writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[
			{"id":"edited","body":"Revised text","createdAt":"2026-09-27T10:01:00Z","updatedAt":"2026-09-28T11:00:00Z","user":{"id":"bot-1","name":"Automation"}},
			{"id":"unavailable","body":"Retained text","createdAt":"2026-09-27T10:02:00Z","updatedAt":"2026-09-27T10:02:00Z","user":null}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})

	out, err := runLinearCmd(t, "", "list-comments", "ENG-1")
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeJSONArray(t, out)
	if len(rows) != 3 {
		t.Fatalf("complete issue history has %d comments, want 3", len(rows))
	}
	first := rows[0].(map[string]any)
	firstUser, firstHasUser := first["user"].(map[string]any)
	if first["id"] != "human" || first["body"] != "Human text" || first["createdAt"] == nil ||
		first["updatedAt"] == nil || first["authorStatus"] != "available" ||
		!firstHasUser || firstUser["id"] != "u-1" {
		t.Fatalf("human identity/legacy fields missing: %v", first)
	}
	bot := rows[1].(map[string]any)
	botUser, botHasUser := bot["user"].(map[string]any)
	if !botHasUser || botUser["id"] != "bot-1" || bot["updatedAt"] != "2026-09-28T11:00:00Z" ||
		bot["createdAt"] == bot["updatedAt"] {
		t.Fatalf("edited bot identity/revision missing: %v", bot)
	}
	unavailable := rows[2].(map[string]any)
	if unavailable["user"] != nil || unavailable["authorStatus"] != "unavailable" {
		t.Fatalf("deleted/unavailable author was inferred: %v", unavailable)
	}

	out, err = runLinearCmd(t, "", "list-comments", "ENG-1", "--comment-id", "edited")
	if err != nil {
		t.Fatal(err)
	}
	selected := decodeJSONArray(t, out)
	if len(selected) != 1 || selected[0].(map[string]any)["id"] != "edited" {
		t.Fatalf("requested comment was not uniquely selected: %s", out)
	}
	if _, err := runLinearCmd(t, "", "list-comments", "ENG-1", "--comment-id", "other-issue-comment"); err == nil || !strings.Contains(err.Error(), "not present on issue") {
		t.Fatalf("foreign comment ID accepted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cursors) != 6 {
		t.Fatalf("listing or requested lookup skipped a page: cursors=%v", cursors)
	}
	for i := 0; i < len(cursors); i += 2 {
		if cursors[i] != nil || cursors[i+1] != "next" {
			t.Fatalf("listing %d cursors=%v", i/2, cursors[i:i+2])
		}
	}
}

func TestLinearListCommentsDirectAuthRefusalDoesNotTryAlternateIdentity(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				if r.Header.Get("Authorization") != "test-fixture-key-not-a-secret" {
					t.Errorf("direct Linear identity changed on refusal")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"errors":[{"message":"fixture refused"}]}`))
			})
			t.Setenv("WORKER_AUTH_TOKEN", "other-fixture-token")
			t.Setenv("DONMAI_API_URL", "http://127.0.0.1:1")
			_, err := runLinearCmd(t, "", "list-comments", "ENG-1")
			if err == nil {
				t.Fatal("direct API refusal was hidden")
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Fatalf("direct API refusal retried with another identity: requests=%d", requests)
			}
		})
	}
}
