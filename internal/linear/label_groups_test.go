package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeLabelCatalogPreservesScopesAndPaginates(t *testing.T) {
	var cursors []any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(request.Query, "ListLabelDetails") {
			t.Fatalf("unexpected query: %s", request.Query)
		}
		cursors = append(cursors, request.Variables["after"])
		if len(cursors) == 1 {
			writeGQLData(w, `{"issueLabels":{"nodes":[
				{"id":"workspace-child","name":"draft","isGroup":false,"groupType":null,"team":null,"parent":{"id":"workspace-group","name":"content"}}
			],"pageInfo":{"hasNextPage":true,"endCursor":"page-two"}}}`)
			return
		}
		writeGQLData(w, `{"issueLabels":{"nodes":[
			{"id":"team-child","name":"draft","isGroup":false,"groupType":null,"team":{"id":"team-1","key":"ENG"},"parent":{"id":"team-group","name":"content"}},
			{"id":"flat-colon","name":"content:review","isGroup":false,"groupType":null,"team":null,"parent":null}
		],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})
	labels, err := client.ListLabelDetails(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 3 || labels[0].ParentID != "workspace-group" || labels[1].TeamID != "team-1" || labels[1].ParentID != "team-group" || labels[2].ParentID != "" {
		t.Fatalf("native label metadata lost: %+v", labels)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "page-two" {
		t.Fatalf("pagination cursors=%v", cursors)
	}
}

func TestApplicableLabelTeamHierarchyFailsClosed(t *testing.T) {
	deep := make([]Team, 7)
	for i := range deep {
		deep[i] = Team{ID: string(rune('a' + i)), Key: string(rune('A' + i)), ParentKnown: true}
		if i+1 < len(deep) {
			deep[i].ParentID = string(rune('a' + i + 1))
		}
	}
	for _, tc := range []struct {
		name  string
		teams []Team
	}{
		{"missing_parent_field", []Team{{ID: "child", Key: "CHILD"}}},
		{"inaccessible_parent", []Team{{ID: "child", Key: "CHILD", ParentID: "private", ParentKnown: true}}},
		{"cycle", []Team{{ID: "child", Key: "CHILD", ParentID: "parent", ParentKnown: true}, {ID: "parent", Key: "PARENT", ParentID: "child", ParentKnown: true}}},
		{"duplicate_id", []Team{{ID: "child", Key: "CHILD", ParentKnown: true}, {ID: "child", Key: "OTHER", ParentKnown: true}}},
		{"over_depth", deep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := "CHILD"
			if tc.name == "over_depth" {
				ref = "A"
			}
			if ids, err := applicableLabelTeamIDs(tc.teams, ref); err == nil || ids != nil {
				t.Fatalf("incomplete/ambiguous hierarchy accepted: ids=%v err=%v", ids, err)
			}
		})
	}
}

func TestListIssueLabelsPaginatesAndFailsClosedOnMissingCursor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		missing    bool
		wantLabels int
	}{
		{"complete", false, 2},
		{"missing_cursor", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(request.Query, "ListIssueLabels") || !strings.Contains(request.Query, "includeArchived: true") {
					t.Fatalf("wrong query: %s", request.Query)
				}
				calls++
				if calls == 1 {
					cursor := `"page-two"`
					if tc.missing {
						cursor = "null"
					}
					writeGQLData(w, `{"issue":{"id":"issue-1","labels":{"nodes":[{"id":"old","name":"Bug","isGroup":false,"parent":{"id":"group-1"}}],"pageInfo":{"hasNextPage":true,"endCursor":`+cursor+`}}}}`)
					return
				}
				if request.Variables["after"] != "page-two" {
					t.Fatalf("second page cursor=%v", request.Variables["after"])
				}
				writeGQLData(w, `{"issue":{"id":"issue-1","labels":{"nodes":[{"id":"other","name":"Urgent","isGroup":false,"parent":null}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
			})
			labels, err := client.ListIssueLabels(context.Background(), "issue-1")
			if tc.missing {
				if err == nil || labels != nil || calls != 1 {
					t.Fatalf("missing cursor yielded partial result: labels=%v err=%v calls=%d", labels, err, calls)
				}
				return
			}
			if err != nil || len(labels) != tc.wantLabels || labels[0].ID != "old" || labels[0].ParentID != "group-1" || labels[1].ID != "other" || labels[1].ParentID != "" || calls != 2 {
				t.Fatalf("complete issue labels=%v err=%v calls=%d", labels, err, calls)
			}
		})
	}
}

func TestStrictLabelProxyPreflightAndHeaderOnEveryOperation(t *testing.T) {
	methods := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != strictLabelProxyPath {
			t.Errorf("strict request reached %q, want %q", r.URL.Path, strictLabelProxyPath)
		}
		if r.Header.Get("Authorization") != "Bearer proxy-fixture-token" ||
			r.Header.Get(labelProxyPolicyHeader) != labelProxyPolicy {
			t.Errorf("strict auth/policy header missing on %s", r.Method)
		}
		if r.Method == http.MethodOptions {
			w.Header().Set(labelProxyPolicyHeader, labelProxyPolicy)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if strings.Contains(request.Query, "ListLabelDetails") {
			writeGQLData(w, `{"issueLabels":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
			return
		}
		writeGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"group-1","name":"Type","isGroup":true,"groupType":"singleSelect","team":null,"parent":null}}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewProxiedClient(server.URL, "proxy-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = server.Client()
	if _, err := client.ListLabelDetails(context.Background()); err == nil || len(methods) != 0 {
		t.Fatalf("provider read ran before strict capability: err=%v methods=%v", err, methods)
	}
	if err := client.EnableNoFallbackLabelProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListLabelDetails(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Type", IsGroup: true, GroupType: "singleSelect"}); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 3 || methods[0] != http.MethodOptions || methods[1] != http.MethodPost || methods[2] != http.MethodPost {
		t.Fatalf("strict operation order=%v", methods)
	}
}

func TestStrictLabelProxyOldOrDeniedCapabilityNeverDispatches(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusForbidden, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != strictLabelProxyPath {
					t.Errorf("probe reached legacy route %q", r.URL.Path)
				}
				if r.Method == http.MethodPost {
					posts++
				}
				if status == http.StatusFound {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(status) // no exact policy acknowledgement
			}))
			t.Cleanup(server.Close)
			client, err := NewProxiedClient(server.URL, "proxy-fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			client.HTTPClient = server.Client()
			if err := client.EnableNoFallbackLabelProxy(context.Background()); err == nil {
				t.Fatal("unacknowledged/denied capability accepted")
			}
			if _, err := client.ListLabelDetails(context.Background()); err == nil || posts != 0 {
				t.Fatalf("provider op after failed preflight: err=%v posts=%d", err, posts)
			}
		})
	}
}

func TestStrictLabelProxyDoesNotFollowMutationRedirect(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set(labelProxyPolicyHeader, labelProxyPolicy)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	client, err := NewProxiedClient(server.URL, "proxy-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = server.Client()
	if err := client.EnableNoFallbackLabelProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Type", IsGroup: true, GroupType: "singleSelect"}); err == nil || redirected != 0 {
		t.Fatalf("strict write followed redirect: err=%v redirected=%d", err, redirected)
	}
}

func TestStrictLabelProxyForbiddenWriteHasNoAlternateRequest(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != strictLabelProxyPath || r.Header.Get(labelProxyPolicyHeader) != labelProxyPolicy {
			t.Errorf("strict write escaped versioned policy path: %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodOptions {
			w.Header().Set(labelProxyPolicyHeader, labelProxyPolicy)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		posts++
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := NewProxiedClient(server.URL, "proxy-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = server.Client()
	if err := client.EnableNoFallbackLabelProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Type", IsGroup: true, GroupType: "singleSelect"})
	if !errors.Is(err, ErrForbidden) || posts != 1 {
		t.Fatalf("strict forbidden write: err=%v posts=%d", err, posts)
	}
}

func TestNativeLabelMutationInputsAndIdentity(t *testing.T) {
	var seen []map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, request.Variables)
		switch {
		case strings.Contains(request.Query, "CreateNativeLabel"):
			input, _ := request.Variables["input"].(map[string]any)
			if input["isGroup"] == true {
				writeGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"group-1","name":"Type","isGroup":true,"groupType":"singleSelect","team":null,"parent":null}}}`)
				return
			}
			writeGQLData(w, `{"issueLabelCreate":{"success":true,"issueLabel":{"id":"child-1","name":"Bug","isGroup":false,"groupType":null,"team":null,"parent":{"id":"group-1","name":"Type"}}}}`)
		case strings.Contains(request.Query, "ReparentLabel"):
			writeGQLData(w, `{"issueLabelUpdate":{"success":true,"issueLabel":{"id":"existing-1","name":"Existing","isGroup":false,"groupType":null,"team":null,"parent":{"id":"group-1","name":"Type"}}}}`)
		default:
			t.Fatalf("unexpected query: %s", request.Query)
		}
	})
	group, err := client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Type", IsGroup: true, GroupType: "singleSelect"})
	if err != nil || group.ID != "group-1" {
		t.Fatalf("create group=%+v err=%v", group, err)
	}
	child, err := client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Bug", ParentID: group.ID})
	if err != nil || child.ParentID != group.ID {
		t.Fatalf("create child=%+v err=%v", child, err)
	}
	moved, err := client.ReparentLabel(context.Background(), "existing-1", group.ID)
	if err != nil || moved.ID != "existing-1" || moved.ParentID != group.ID {
		t.Fatalf("reparent=%+v err=%v", moved, err)
	}
	if len(seen) != 3 {
		t.Fatalf("mutations sent %d requests, want three exact writes", len(seen))
	}
	first := seen[0]["input"].(map[string]any)
	if first["isGroup"] != true || first["groupType"] != "singleSelect" || first["teamId"] != nil || first["parentId"] != nil {
		t.Fatalf("group input=%v", first)
	}
	second := seen[1]["input"].(map[string]any)
	if second["parentId"] != "group-1" || second["isGroup"] != nil {
		t.Fatalf("child input=%v", second)
	}
	third := seen[2]["input"].(map[string]any)
	if seen[2]["id"] != "existing-1" || len(third) != 1 || third["parentId"] != "group-1" {
		t.Fatalf("reparent variables=%v", seen[2])
	}
}

func TestNativeLabelAuthorizationRefusesWithoutRetry(t *testing.T) {
	requests := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := client.CreateNativeLabel(context.Background(), NativeLabelCreateInput{Name: "Type", IsGroup: true, GroupType: "singleSelect"})
	if !errors.Is(err, ErrForbidden) || requests != 1 {
		t.Fatalf("forbidden create: err=%v requests=%d", err, requests)
	}
}
