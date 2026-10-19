package linearcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/linear"
)

type listFilterRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func captureListFilterRequest(t *testing.T, r *http.Request) listFilterRequest {
	t.Helper()
	var req listFilterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return req
}

func assertFilterJSON(t *testing.T, got any, want string) {
	t.Helper()
	var expected any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("filter = %#v, want %s", got, want)
	}
}

func TestStructuredIssueFilters(t *testing.T) {
	for _, filter := range []string{
		`{"createdAt":{"gte":"2026-01-01","lt":"2026-02-01"},"priority":{"gte":1,"lte":2,"neq":0}}`,
		`{"state":{"name":{"in":["Backlog","Started"]}},"labels":{"name":{"in":["Bug","Defect"],"nin":["Duplicate"]}}}`,
		`{"assignee":{"null":true}}`,
		`{"and":[{"or":[{"priority":{"eq":1}},{"dueDate":{"lt":"P2W"}}]},{"or":[{"assignee":{"null":true}},{"labels":{"every":{"name":{"neq":"Duplicate"}}}}]}]}`,
	} {
		t.Run(filter, func(t *testing.T) {
			requests := make(chan listFilterRequest, 1)
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				requests <- captureListFilterRequest(t, r)
				writeLinearGQLData(w, `{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
			})
			out, err := runLinearCmd(t, "", "list-issues", "--filter", filter)
			if err != nil {
				t.Fatalf("list issues: %v; %s", err, out)
			}
			req := <-requests
			assertFilterJSON(t, req.Variables["filter"], filter)
			if !strings.Contains(req.Query, "issues(filter: $filter,") {
				t.Errorf("filter not applied by production query: %s", req.Query)
			}
			if strings.Contains(req.Query, "includeArchived") || req.Variables["includeArchived"] != nil {
				t.Errorf("archives included by default: %#v", req)
			}
		})
	}
}

func TestStructuredIssuesComposeEveryNamedFlag(t *testing.T) {
	for _, assignee := range []string{"me", "Pat"} {
		t.Run(assignee, func(t *testing.T) {
			requests := make(chan listFilterRequest, 4)
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				req := captureListFilterRequest(t, r)
				requests <- req
				switch {
				case strings.Contains(req.Query, "ListProjects"):
					writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"project-one","name":"Alpha"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
				case strings.Contains(req.Query, "ListTeams"):
					writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-one","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
				case strings.Contains(req.Query, "Viewer"):
					writeLinearGQLData(w, `{"viewer":{"id":"user-one","name":"Pat"}}`)
				case strings.Contains(req.Query, "ListUsers"):
					writeLinearGQLData(w, `{"users":{"nodes":[{"id":"user-one","name":"Pat"}]}}`)
				default:
					writeLinearGQLData(w, `{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
				}
			})
			out, err := runLinearCmd(t, "", "list-issues", "--project", "Alpha", "--team", "Engineering", "--state", "Started", "--label", "Bug", "--priority", "0", "--assignee", assignee, "--query", "needle", "--filter", `{"project":{"id":{"neq":"project-one"}},"team":{"id":{"neq":"team-one"}},"or":[{"priority":{"gte":2}},{"assignee":{"null":true}}]}`)
			if err != nil {
				t.Fatalf("combined list: %v; %s", err, out)
			}
			var listing listFilterRequest
			for range 4 {
				req := <-requests
				if strings.Contains(req.Query, "ListIssues") {
					listing = req
				}
			}
			assertFilterJSON(t, listing.Variables["filter"], `{"and":[{"project":{"id":{"eq":"project-one"}},"team":{"id":{"eq":"team-one"}},"state":{"name":{"eqIgnoreCase":"Started"}},"labels":{"name":{"eqIgnoreCase":"Bug"}},"priority":{"eq":0},"assignee":{"id":{"eq":"user-one"}},"or":[{"title":{"containsIgnoreCase":"needle"}},{"description":{"containsIgnoreCase":"needle"}}]},{"project":{"id":{"neq":"project-one"}},"team":{"id":{"neq":"team-one"}},"or":[{"priority":{"gte":2}},{"assignee":{"null":true}}]}]}`)
		})
	}
}

func TestStructuredIssuePaginationAndOrdering(t *testing.T) {
	for _, order := range []string{"manual", "createdAt", "updatedAt"} {
		t.Run(order, func(t *testing.T) {
			requests := make(chan listFilterRequest, 2)
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				req := captureListFilterRequest(t, r)
				requests <- req
				if req.Variables["after"] == nil {
					nodes := make([]string, 250)
					for i := range nodes {
						nodes[i] = fmt.Sprintf(`{"id":"node-%d","identifier":"item-%d","priority":4,"sortOrder":%d}`, i, i, i)
					}
					writeLinearGQLData(w, `{"issues":{"nodes":[`+strings.Join(nodes, ",")+`],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`)
				} else {
					writeLinearGQLData(w, `{"issues":{"nodes":[{"id":"node-last","identifier":"item-last","priority":1,"sortOrder":250}],"pageInfo":{"hasNextPage":true,"endCursor":"more"}}}`)
				}
			})
			root := New(nil, "donmai")
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"list-issues", "--filter", `{"priority":{"gte":1}}`, "--include-archived", "--order-by", order, "--limit", "251"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			for page := range 2 {
				req := <-requests
				assertFilterJSON(t, req.Variables["filter"], `{"priority":{"gte":1}}`)
				if req.Variables["includeArchived"] != true || !strings.Contains(req.Query, "$includeArchived: Boolean") || !strings.Contains(req.Query, "issues(includeArchived: $includeArchived, filter: $filter,") {
					t.Errorf("archive pagination argument missing: %#v", req)
				}
				wantSize := float64(250)
				if page == 1 {
					wantSize = 1
					if req.Variables["after"] != "next" {
						t.Errorf("cursor = %v", req.Variables["after"])
					}
				}
				if req.Variables["first"] != wantSize {
					t.Errorf("first = %v, want %v", req.Variables["first"], wantSize)
				}
				if order == "manual" {
					if !strings.Contains(req.Query, "sort: $sort") || req.Variables["orderBy"] != nil {
						t.Errorf("manual query = %#v", req)
					}
					assertFilterJSON(t, req.Variables["sort"], `[{"manual":{"order":"Ascending","nulls":"last"}}]`)
				} else if req.Variables["orderBy"] != order || !strings.Contains(req.Query, "orderBy: $orderBy") {
					t.Errorf("timestamp query = %#v", req)
				}
			}
			result := decodeJSON(t, stdout.String())
			issues := result["issues"].([]any)
			if len(issues) != 251 || result["truncated"] != true || result["limit"] != float64(251) || !strings.Contains(stderr.String(), "reached the --limit of 251") {
				t.Fatalf("pagination output = %s; %s", stdout.String(), stderr.String())
			}
			wantFirst := "item-last"
			if order == "manual" {
				wantFirst = "item-0"
			}
			if issues[0].(map[string]any)["id"] != wantFirst {
				t.Errorf("first issue = %v, want %s", issues[0], wantFirst)
			}
		})
	}
}

func TestStructuredProjectFilters(t *testing.T) {
	const filter = `{"lead":{"name":{"startsWith":"Pat"}},"name":{"containsIgnoreCase":"release"},"accessibleTeams":{"some":{"id":{"neq":"team-one"}}}}`
	for _, source := range []string{"inline", "file"} {
		for _, archive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/archive=%t", source, archive), func(t *testing.T) {
				requests := make(chan listFilterRequest, 3)
				setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
					req := captureListFilterRequest(t, r)
					requests <- req
					if strings.Contains(req.Query, "ListTeams") {
						writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-one","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
						return
					}
					if req.Variables["after"] == nil {
						writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"b","name":"Beta","state":"planned","teams":{"nodes":[{"id":"team-one","key":"ENG","name":"Engineering"},{"id":"team-two","key":"OPS","name":"Operations"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}`)
					} else {
						writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"a","name":"Alpha","state":"started","teams":{"nodes":[{"id":"team-one","key":"ENG","name":"Engineering"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
					}
				})
				args := []string{"list-projects", "--team", "ENG"}
				if source == "inline" {
					args = append(args, "--filter", filter)
				} else {
					path := filepath.Join(t.TempDir(), "filter.json")
					if err := os.WriteFile(path, []byte(filter), 0o600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--filter-file", path)
				}
				if archive {
					args = append(args, "--include-archived")
				}
				out, err := runLinearCmd(t, "", args...)
				if err != nil {
					t.Fatalf("projects: %v; %s", err, out)
				}
				<-requests // team resolution
				for page := range 2 {
					req := <-requests
					assertFilterJSON(t, req.Variables["filter"], `{"and":[{"accessibleTeams":{"some":{"id":{"eq":"team-one"}}}},`+filter+`]}`)
					if !strings.Contains(req.Query, "filter: $filter, first: 25, after: $after") {
						t.Errorf("project pagination query = %s", req.Query)
					}
					if archive {
						if req.Variables["includeArchived"] != true || !strings.Contains(req.Query, "$includeArchived: Boolean") || !strings.Contains(req.Query, "projects(includeArchived: $includeArchived,") {
							t.Errorf("archive query = %#v", req)
						}
					} else if req.Variables["includeArchived"] != nil || strings.Contains(req.Query, "includeArchived") {
						t.Errorf("default archive query = %#v", req)
					}
					if page == 1 && req.Variables["after"] != "next" {
						t.Errorf("project cursor = %v", req.Variables["after"])
					}
				}
				rows := decodeJSONArray(t, out)
				if len(rows) != 2 || rows[0].(map[string]any)["name"] != "Alpha" || rows[1].(map[string]any)["teamKey"] != "ENG" {
					t.Errorf("project output order/scope = %s", out)
				}
			})
		}
	}
}

func TestStructuredFiltersRefuseBeforeClientCreation(t *testing.T) {
	for _, command := range []string{"list-issues", "list-projects"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("LINEAR_API_KEY", "")
			t.Setenv("LINEAR_ACCESS_TOKEN", "")
			t.Setenv("WORKER_AUTH_TOKEN", "")
			missing := filepath.Join(t.TempDir(), "missing.json")
			badFile := filepath.Join(t.TempDir(), "bad.json")
			largeFile := filepath.Join(t.TempDir(), "large.json")
			if err := os.WriteFile(badFile, []byte(`[]`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(largeFile, []byte(`{"name":"`+strings.Repeat("x", 65536)+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"--filter", ""},
				{"--filter", "{"},
				{"--filter", "[]"},
				{"--filter", "null"},
				{"--filter", "true"},
				{"--filter", "123"},
				{"--filter", `"text"`},
				{"--filter", "{} {}"},
				{"--filter", "{} garbage"},
				{"--filter", `{"name":"` + strings.Repeat("x", 65536) + `"}`},
				{"--filter-file", missing},
				{"--filter-file", badFile},
				{"--filter-file", largeFile},
				{"--filter", "{}", "--filter-file", missing},
				{"--filter", "", "--filter-file", ""},
			} {
				var created atomic.Int32
				root := New(func() afclient.DataSource { created.Add(1); return afclient.NewMockClient() }, "donmai")
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				root.SilenceErrors = true
				root.SetArgs(append([]string{command}, args...))
				err := root.Execute()
				if err == nil || created.Load() != 0 || strings.Contains(err.Error(), "linear access requires") {
					t.Errorf("invalid input accepted or client created: err=%v, created=%d", err, created.Load())
				}
			}
		})
	}
}

func TestStructuredFiltersPreserveProxyErrorsAndAuthority(t *testing.T) {
	for _, command := range []string{"list-issues", "list-projects"} {
		for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusUnauthorized} {
			t.Run(fmt.Sprintf("%s/status=%d", command, status), func(t *testing.T) {
				t.Setenv("LINEAR_API_KEY", "")
				t.Setenv("LINEAR_ACCESS_TOKEN", "")
				t.Setenv("WORKER_AUTH_TOKEN", "")
				var calls atomic.Int32
				var proxyPath string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != proxyPath || r.Header.Get("Authorization") != "Bearer fixture-session-token" {
						t.Errorf("transport authority changed")
					}
					req := captureListFilterRequest(t, r)
					assertFilterJSON(t, req.Variables["filter"], `{"unknownFilterField":{"eq":"value"}}`)
					if !strings.Contains(req.Query, "query List") {
						t.Errorf("operation changed: %s", req.Query)
					}
					w.WriteHeader(status)
					if status == http.StatusOK {
						writeLinearGQLError(w, "Unknown filter field")
					} else {
						_, _ = w.Write([]byte(`{"error":"operation denied"}`))
					}
				}))
				t.Cleanup(srv.Close)
				proxy, err := linear.NewProxiedClient(srv.URL, "fixture-session-token")
				if err != nil {
					t.Fatal(err)
				}
				proxyPath = strings.TrimPrefix(proxy.BaseURL, srv.URL)
				root := New(func() afclient.DataSource { return afclient.NewAuthenticatedClient(srv.URL, "fixture-session-token") }, "donmai")
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				root.SilenceErrors = true
				root.SetArgs([]string{command, "--filter", `{"unknownFilterField":{"eq":"value"}}`, "--include-archived"})
				err = root.Execute()
				if err == nil || calls.Load() != 1 || out.Len() != 0 {
					t.Fatalf("server denial/error became success: err=%v, calls=%d, out=%s", err, calls.Load(), out.String())
				}
				if status == http.StatusOK && !strings.Contains(err.Error(), "Unknown filter field") {
					t.Errorf("GraphQL error lost: %v", err)
				}
			})
		}
	}
}

func TestStructuredIssueFilterFile(t *testing.T) {
	const filter = `{"and":[{"priority":{"gte":1}},{"createdAt":{"gte":"2026-01-01"}}]}`
	path := filepath.Join(t.TempDir(), "filter.json")
	if err := os.WriteFile(path, []byte(filter), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan listFilterRequest, 1)
	setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- captureListFilterRequest(t, r)
		writeLinearGQLData(w, `{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
	})
	if out, err := runLinearCmd(t, "", "list-issues", "--filter-file", path, "--status", "Backlog", "--include-archived=false"); err != nil {
		t.Fatalf("filter file: %v; %s", err, out)
	}
	req := <-requests
	assertFilterJSON(t, req.Variables["filter"], `{"and":[{"state":{"name":{"eqIgnoreCase":"Backlog"}}},`+filter+`]}`)
	if strings.Contains(req.Query, "includeArchived") || req.Variables["includeArchived"] != nil {
		t.Errorf("false opt-in includes archives: %#v", req)
	}
}
