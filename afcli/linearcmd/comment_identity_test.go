package linearcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

const (
	commentLegacyProxyPath = "/api/cli/linear/graphql"
	commentStrictProxyPath = "/api/cli/linear/graphql/no-fallback-v1"
)

func runProxiedListComments(t *testing.T, origin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("LINEAR_ACCESS_TOKEN", "")
	t.Setenv("WORKER_AUTH_TOKEN", "")
	t.Setenv("DONMAI_API_URL", "")
	root := New(func() afclient.DataSource { return afclient.NewAuthenticatedClient(origin, "rsk_comment_fixture") }, "donmai")
	root.SilenceErrors = true
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"list-comments"}, args...))
	err := root.Execute()
	return output.String(), err
}

func TestLinearListCommentsStrictProxyPreflightAndEveryPage(t *testing.T) {
	var mu sync.Mutex
	var methods, paths []string
	var afters []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != commentStrictProxyPath || r.Header.Get("Authorization") != "Bearer rsk_comment_fixture" ||
			r.Header.Get("X-Donmai-Linear-Auth-Policy") != "no-fallback-v1" {
			t.Errorf("comment request escaped negotiated strict identity: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("X-Donmai-Linear-Auth-Policy", "no-fallback-v1")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode strict comment request: %v", err)
			return
		}
		if request.Variables["issueId"] != "ENG-1" {
			t.Errorf("comment request lost issue scope: %v", request.Variables)
		}
		mu.Lock()
		afters = append(afters, request.Variables["after"])
		mu.Unlock()
		if request.Variables["after"] == nil {
			writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[{"id":"first","body":"one","createdAt":"2026-09-27T10:00:00Z","updatedAt":"2026-09-27T10:00:00Z","user":{"id":"user-1","name":"Alice"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`)
			return
		}
		writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[{"id":"second","body":"two","createdAt":"2026-09-27T10:01:00Z","updatedAt":"2026-09-27T10:01:00Z","user":null}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	}))
	t.Cleanup(server.Close)
	out, err := runProxiedListComments(t, server.URL, "ENG-1", "--comment-id", "second")
	if err != nil {
		t.Fatalf("strict comment read failed: %v out=%q", err, out)
	}
	rows := decodeJSONArray(t, out)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "second" {
		t.Fatalf("requested comment not selected from complete strict listing: %s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 3 || methods[0] != http.MethodOptions || methods[1] != http.MethodPost || methods[2] != http.MethodPost ||
		paths[0] != commentStrictProxyPath || paths[1] != commentStrictProxyPath || paths[2] != commentStrictProxyPath ||
		len(afters) != 2 || afters[0] != nil || afters[1] != "next" {
		t.Fatalf("strict request sequence methods=%v paths=%v afters=%v", methods, paths, afters)
	}
}

func TestLinearListCommentsStrictProxyRefusalsNeverUseLegacy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		capabilityStatus  int
		acknowledge       bool
		firstPageStatus   int
		secondPageStatus  int
		wantProviderPosts int
	}{
		{"unacknowledged", http.StatusNoContent, false, 0, 0, 0},
		{"capability_forbidden", http.StatusForbidden, false, 0, 0, 0},
		{"first_page_unauthorized", http.StatusNoContent, true, http.StatusUnauthorized, 0, 1},
		{"first_page_redirect", http.StatusNoContent, true, http.StatusFound, 0, 1},
		{"second_page_forbidden", http.StatusNoContent, true, 0, http.StatusForbidden, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			providerPosts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				if r.URL.Path != commentStrictProxyPath ||
					r.Header.Get("Authorization") != "Bearer rsk_comment_fixture" ||
					r.Header.Get("X-Donmai-Linear-Auth-Policy") != "no-fallback-v1" {
					t.Errorf("comment request changed route or identity: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.Method == http.MethodOptions {
					if tc.acknowledge {
						w.Header().Set("X-Donmai-Linear-Auth-Policy", "no-fallback-v1")
						w.Header().Set("Cache-Control", "no-store")
					}
					w.WriteHeader(tc.capabilityStatus)
					return
				}
				mu.Lock()
				providerPosts++
				page := providerPosts
				mu.Unlock()
				if page == 1 && tc.firstPageStatus != 0 {
					if tc.firstPageStatus == http.StatusFound {
						w.Header().Set("Location", commentLegacyProxyPath)
					}
					w.WriteHeader(tc.firstPageStatus)
					return
				}
				if page == 2 && tc.secondPageStatus != 0 {
					w.WriteHeader(tc.secondPageStatus)
					return
				}
				writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`)
			}))
			t.Cleanup(server.Close)
			out, err := runProxiedListComments(t, server.URL, "ENG-1")
			if err == nil {
				t.Fatalf("strict proxy refusal accepted: out=%q", out)
			}
			mu.Lock()
			defer mu.Unlock()
			if providerPosts != tc.wantProviderPosts {
				t.Fatalf("unexpected provider requests after refusal: got=%d want=%d paths=%v", providerPosts, tc.wantProviderPosts, paths)
			}
			for _, path := range paths {
				if path != commentStrictProxyPath {
					t.Fatalf("comment read hopped to legacy route: %v", paths)
				}
			}
		})
	}
}

func TestLinearListCommentsDirectModeDoesNotNegotiateProxy(t *testing.T) {
	var mu sync.Mutex
	methods := make([]string, 0, 1)
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "test-fixture-key-not-a-secret" ||
			r.Header.Get("X-Donmai-Linear-Auth-Policy") != "" {
			t.Errorf("direct comment read changed authentication or negotiated proxy: method=%s", r.Method)
		}
		writeLinearGQLData(w, `{"issue":{"id":"issue-uuid","comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	})
	out, err := runLinearCmd(t, "", "list-comments", "ENG-1")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || strings.TrimSpace(out) != "[]" || len(methods) != 1 || methods[0] != http.MethodPost {
		t.Fatalf("direct comment read changed: out=%q err=%v methods=%v", out, err, methods)
	}
}

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
