package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGetIssueEstimate(t *testing.T) {
	t.Parallel()
	for _, estimate := range []string{"3", "0", "2.5", "null"} {
		t.Run(estimate, func(t *testing.T) {
			t.Parallel()
			client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query string
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				node := `{"id":"issue-1","identifier":"ENG-1"}`
				if strings.Contains(req.Query, "estimate") {
					node = fmt.Sprintf(`{"id":"issue-1","identifier":"ENG-1","estimate":%s}`, estimate)
				}
				writeGQLData(w, `{"issue":`+node+`}`)
			})
			issue, err := client.GetIssue(context.Background(), "ENG-1")
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
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
}
