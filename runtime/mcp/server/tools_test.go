package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToolDescriptions_LanguageNeutral pins the WS4 contract: tool
// descriptions must not read as single-language idiom. The pilot showed the
// find_type_usages description ("union type or enum … Record<> …") telling a
// Go agent the tool was inapplicable, which drove refactor adoption to ~0%.
func TestToolDescriptions_LanguageNeutral(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	tsIdioms := []string{"Record<", "union type", "exhaustive check"}
	for _, td := range s.buildTools() {
		for _, idiom := range tsIdioms {
			if strings.Contains(td.description, idiom) {
				t.Errorf("tool %s description contains TS-only idiom %q: %q", td.name, idiom, td.description)
			}
		}
	}
}

// TestFindTypeUsagesDescription_TaskOriented pins the task framing: the xref
// tool must advertise the cross-file rename/refactor job (enumerate every
// affected site BEFORE editing), not a TS union-membership niche.
func TestFindTypeUsagesDescription_TaskOriented(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepo(t))
	var desc string
	for _, td := range s.buildTools() {
		if td.name == ToolFindTypeUsages {
			desc = td.description
		}
	}
	if desc == "" {
		t.Fatalf("tool %s not found in buildTools", ToolFindTypeUsages)
	}
	low := strings.ToLower(desc)
	for _, want := range []string{"usage", "rename"} {
		if !strings.Contains(low, want) {
			t.Errorf("find_type_usages description should mention %q; got %q", want, desc)
		}
	}
}

// TestToolSurfaceWeight_Budget pins the WS11 slimming: the six schemas +
// descriptions inject into the model context on every tool load (and into the
// ToolSearch payload when the client defers MCP tools), so they are a fixed
// per-session token tax. Pre-WS11 the combined weight was 3483 chars
// (descriptions 1032 + compacted schemas 2451); the budget below locks in the
// ~40% reduction. Measured on json.Compact output because MCP clients
// re-serialize the schema — source indentation never reaches the model.
func TestToolSurfaceWeight_Budget(t *testing.T) {
	t.Parallel()
	const (
		maxDescriptionChars = 120  // one-line descriptions only
		maxCombinedChars    = 2100 // ~40% below the pre-WS11 3483
	)
	s := newTestServer(t, fixtureRepo(t))
	combined := 0
	for _, td := range s.buildTools() {
		if n := len(td.description); n > maxDescriptionChars {
			t.Errorf("tool %s description is %d chars (max %d): %q", td.name, n, maxDescriptionChars, td.description)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, td.inputSchema); err != nil {
			t.Fatalf("tool %s schema is not valid JSON: %v", td.name, err)
		}
		combined += len(td.description) + buf.Len()
	}
	if combined > maxCombinedChars {
		t.Errorf("combined tool surface weight = %d chars, budget %d", combined, maxCombinedChars)
	}
}

// TestCheckDuplicate_MaxResultsForwarded proves the maxResults argument is
// plumbed from the MCP tool call through to the dedup engine: against a
// fixture where the same function exists in two files, maxResults=2 must
// return BOTH duplicate sites in a ranked matches array (the default is the
// single top match with no matches array at all).
func TestCheckDuplicate_MaxResultsForwarded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := "func addNums(a int, b int) int {\n\tsum := a + b\n\treturn sum\n}\n"
	for _, f := range []struct{ name, pkg string }{{"a.go", "a"}, {"b.go", "b"}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte("package "+f.pkg+"\n\n"+fn), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", f.name, err)
		}
	}
	s := newTestServer(t, dir)

	res, rerr := s.callTool(context.Background(), ToolCheckDuplicate,
		json.RawMessage(`{"content":"func addNums(a int, b int) int {\n\tsum := a + b\n\treturn sum\n}","maxResults":2}`))
	if rerr != nil {
		t.Fatalf("check-duplicate protocol error: %v", rerr)
	}
	if res.IsError {
		t.Fatalf("check-duplicate returned isError: %+v", res.Content)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("want a single text content item, got %+v", res.Content)
	}
	var out struct {
		MatchType string `json:"matchType"`
		Matches   []struct {
			FilePath   string `json:"filePath"`
			SymbolName string `json:"symbolName"`
			MatchType  string `json:"matchType"`
		} `json:"matches"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode result JSON: %v\n%s", err, res.Content[0].Text)
	}
	if out.MatchType != "exact" {
		t.Fatalf("matchType = %q, want exact", out.MatchType)
	}
	if len(out.Matches) != 2 {
		t.Fatalf("len(matches) = %d, want 2 (maxResults=2 must reach the engine)\n%s", len(out.Matches), res.Content[0].Text)
	}
	if out.Matches[0].FilePath != "a.go" || out.Matches[1].FilePath != "b.go" {
		t.Errorf("match files = %s, %s; want a.go, b.go (deterministic order)", out.Matches[0].FilePath, out.Matches[1].FilePath)
	}
	for i, m := range out.Matches {
		if m.SymbolName != "addNums" {
			t.Errorf("matches[%d].symbolName = %q, want addNums", i, m.SymbolName)
		}
	}
}
