package linearcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCommandsPreserveProxyDenials(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		query string
	}{
		{[]string{"list-teams"}, "ListTeams"},
		{[]string{"list-projects", "--team", "Example"}, "ListProjects"},
		{[]string{"create-issue", "--team", "Example", "--title", "fixture"}, "ListTeams"},
		{[]string{"create-issue", "--team", "Example", "--title", "fixture"}, "CreateIssue"},
		{[]string{"create-issue", "--team", "Example", "--project", "Example", "--title", "fixture"}, "ListProjects"},
	} {
		args := tc.args
		t.Run(tc.query+"/"+strings.Join(args, " "), func(t *testing.T) {
			calls := 0
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				if !strings.Contains(request.Query, tc.query) {
					writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-a","key":"Example","name":"Example"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
					return
				}
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","extensions":{"code":"BUDGET_EXHAUSTED"}}]}`)
			})
			output, err := runLinearCmd(t, "", args...)
			if err == nil || !strings.Contains(err.Error(), "budget exhausted") || !strings.Contains(err.Error(), "(code: BUDGET_EXHAUSTED)") || !strings.Contains(err.Error(), "retry after 0s") {
				t.Fatalf("output=%q err=%v", output, err)
			}
			var coded interface{ ExitCode() int }
			if !errors.As(err, &coded) || coded.ExitCode() != 75 || calls != 3 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestCreateIssueRecoversFromProxyDenial(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			setupLinearTest(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				if strings.Contains(request.Query, "ListTeams") {
					writeLinearGQLData(w, `{"teams":{"nodes":[{"id":"team-a","key":"Example","name":"Example"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
					return
				}
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
					return
				}
				writeLinearGQLData(w, `{"issueCreate":{"success":true,"issue":{"id":"fixture","title":"fixture"}}}`)
			})
			output, err := runLinearCmd(t, "", "create-issue", "--team", "00000000-0000-4000-8000-000000000001", "--title", "fixture")
			if err != nil || calls != 2 || !strings.Contains(output, "fixture") {
				t.Fatalf("output=%q err=%v calls=%d", output, err, calls)
			}
		})
	}
}
