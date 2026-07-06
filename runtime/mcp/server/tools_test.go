package server

import (
	"bytes"
	"encoding/json"
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
