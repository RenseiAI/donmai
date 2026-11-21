package result_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/state"
)

// TestTurnManifestFindingsReachThePostedPayload drives an agent-written
// turn-result through the runner's parser and the result poster and asserts
// the findings the control plane receives. Findings used to be dropped at
// parse (the manifest type had no field), so a REQUEST_CHANGES review posted
// no blocking finding at all.
func TestTurnManifestFindingsReachThePostedPayload(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 300)
	cases := []struct {
		name     string
		findings string
		want     []map[string]any
	}{
		{
			name: "gate rows round-trip with blocking first and defaults filled",
			findings: `[
				{"id":"B2","blocking":false,"severity":"low","path":"a.go","line":3,"title":"nit","fix":"rename"},
				{"id":"B1","blocking":true,"severity":"blocker","path":"b.go","line":10,"title":"nil deref","fix":"guard it","category":"needs-real-probe"}
			]`,
			want: []map[string]any{
				{"id": "B1", "blocking": true, "severity": "critical", "path": "b.go", "line": float64(10), "title": "nil deref", "fix": "guard it", "category": "needs-real-probe"},
				{"id": "B2", "blocking": false, "severity": "low", "path": "a.go", "line": float64(3), "title": "nit", "fix": "rename", "category": "other"},
			},
		},
		{
			name: "malformed fields drop the field, never the manifest or the finding",
			findings: `[
				{"id":"not-a-row","blocking":true,"line":"7","title":` + strconvQuote(long) + `},
				{"severity":"high"}
			]`,
			want: []map[string]any{
				{"blocking": true, "severity": "high", "title": long[:240], "category": "other"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()
			if err := os.MkdirAll(filepath.Join(worktree, state.AgentDirName), 0o750); err != nil {
				t.Fatal(err)
			}
			manifest := `{"schemaVersion":1,"verdict":"passed","reviewVerdict":"REQUEST_CHANGES","summary":"s","findings":` + tc.findings + `}`
			if err := os.WriteFile(filepath.Join(worktree, state.AgentDirName, runner.ManifestFileName), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			m, err := runner.ParseManifest(worktree)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			ok := func(int) (int, string) { return http.StatusOK, `{"ok":true}` }
			srv, captured := captureServer(t, ok, ok)
			res := goodResult()
			res.Manifest = m
			if err := newPoster(t, srv.URL, 0).Post(context.Background(), "sess-1", res); err != nil {
				t.Fatalf("Post: %v", err)
			}
			var got []map[string]any
			captured.mu.Lock()
			defer captured.mu.Unlock()
			for _, body := range captured.bodies {
				var payload struct {
					Manifest *struct {
						Findings []map[string]any `json:"findings"`
					} `json:"manifest"`
				}
				if json.Unmarshal([]byte(body), &payload) == nil && payload.Manifest != nil {
					got = payload.Manifest.Findings
				}
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("posted findings\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
