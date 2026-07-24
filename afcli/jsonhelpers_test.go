package afcli

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeJSON decodes a JSON string into a map. Shared by the afcli-package
// command tests (e.g. github_test.go). The Linear command tests keep their own
// copy in afcli/linearcmd since test helpers do not cross package boundaries.
func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	// find the first `{` to skip any leading newline
	idx := strings.Index(s, "{")
	if idx < 0 {
		t.Fatalf("no JSON object found in output: %q", s)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s[idx:]), &out); err != nil {
		t.Fatalf("decode JSON: %v\nraw: %s", err, s)
	}
	return out
}

// decodeJSONArray decodes a JSON array string.
func decodeJSONArray(t *testing.T, s string) []any {
	t.Helper()
	idx := strings.Index(s, "[")
	if idx < 0 {
		t.Fatalf("no JSON array found in output: %q", s)
	}
	var out []any
	if err := json.Unmarshal([]byte(s[idx:]), &out); err != nil {
		t.Fatalf("decode JSON array: %v\nraw: %s", err, s)
	}
	return out
}
