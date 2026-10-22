package linearcmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLinearIssueEstimateOutputs(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{"get", []string{"get-issue", "ENG-1"}},
		// list-issues defaults to the slim row, so estimate coverage opts
		// into the full column set explicitly.
		{"list", []string{"list-issues", "--full"}},
		{"children", []string{"list-sub-issues", "ENG-1"}},
		{"manual", []string{"list-issues", "--order-by", "manual", "--full"}},
		{"backlog", []string{"list-backlog-issues", "--project", "TestProject"}},
		{"unblocked", []string{"list-unblocked-backlog", "--project", "TestProject"}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			for _, estimate := range []string{"3", "0", "2.5", "null"} {
				t.Run(estimate, func(t *testing.T) {
					setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
						var req struct {
							Query string
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
							return
						}
						switch {
						case strings.Contains(req.Query, "ListProjects"):
							writeLinearGQLData(w, `{"projects":{"nodes":[{"id":"proj-1","name":"TestProject"}]}}`)
						case strings.Contains(req.Query, "ListRelations"):
							writeLinearGQLData(w, `{"issue":{"relations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
						default:
							// Model GraphQL selection: an unrequested scalar is absent.
							node := issueNodeJSON("issue-1", "ENG-1", "Estimated issue", "Icebox", "team-1", "ENG", "Engineering")
							if strings.Contains(req.Query, "estimate") {
								node = strings.Replace(node, `"priority":2,`, `"priority":2,"estimate":`+estimate+",", 1)
							}
							if strings.Contains(req.Query, "GetIssue") {
								writeLinearGQLData(w, fmt.Sprintf(`{"issue":%s}`, node))
							} else {
								writeLinearGQLData(w, fmt.Sprintf(`{"issues":{"nodes":[%s],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`, node))
							}
						}
					})
					out, err := runLinearCmd(t, "", command.args...)
					if err != nil {
						t.Fatalf("command: %v", err)
					}
					// Default plain stdout is indented JSON; assert both the visible
					// field and its machine-readable numeric/null value.
					if !strings.Contains(out, `"estimate": `+estimate) {
						t.Errorf("stdout missing estimate %s", estimate)
					}
					var result map[string]any
					switch command.name {
					case "get":
						result = decodeJSON(t, out)
					case "list", "manual", "children":
						key := "issues"
						if command.name == "children" {
							key = "subIssues"
						}
						rows := decodeJSON(t, out)[key].([]any)
						if len(rows) != 1 {
							t.Fatalf("issue count = %d, want 1", len(rows))
						}
						result = rows[0].(map[string]any)
					default:
						rows := decodeJSONArray(t, out)
						if len(rows) != 1 {
							t.Fatalf("issue count = %d, want 1", len(rows))
						}
						result = rows[0].(map[string]any)
					}
					var want any
					if err := json.Unmarshal([]byte(estimate), &want); err != nil {
						t.Fatal(err)
					}
					if got, exists := result["estimate"]; !exists || got != want {
						t.Errorf("estimate = %v (present %t), want %v", got, exists, want)
					}
				})
			}
		})
	}
}
