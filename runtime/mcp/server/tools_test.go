package server

import (
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
