package codeintel

import (
	"math"
	"testing"
)

// TestImportGraph_ResolvesRelativeImports checks relative-import resolution with
// extension and /index fallbacks, and that bare package specifiers do not edge.
func TestImportGraph_ResolvesRelativeImports(t *testing.T) {
	files := map[string]FileIndex{
		"src/a.ts":         {FilePath: "src/a.ts", Imports: []string{"./b", "./sub", "react"}},
		"src/b.ts":         {FilePath: "src/b.ts", Imports: []string{"./a"}},
		"src/sub/index.ts": {FilePath: "src/sub/index.ts", Imports: []string{}},
	}
	g := NewImportGraph()
	g.BuildFromIndex(files)

	deps := g.Dependencies("src/a.ts")
	got := map[string]bool{}
	for _, d := range deps {
		got[d] = true
	}
	if !got["src/b.ts"] {
		t.Errorf("a.ts should import b.ts; deps=%v", deps)
	}
	if !got["src/sub/index.ts"] {
		t.Errorf("a.ts should resolve ./sub to src/sub/index.ts; deps=%v", deps)
	}
	if got["react"] {
		t.Error("bare specifier 'react' must not become an edge")
	}
	if len(deps) != 2 {
		t.Errorf("a.ts should have exactly 2 resolved deps; got %v", deps)
	}

	// b.ts imports ../a -> src/a.ts, so a.ts has b.ts as a dependent.
	dependents := g.Dependents("src/a.ts")
	if len(dependents) != 1 || dependents[0] != "src/b.ts" {
		t.Errorf("a.ts dependents = %v; want [src/b.ts]", dependents)
	}
}

// TestPageRank_HubScoresHighest verifies a node imported by many others earns a
// higher PageRank than an unimported leaf.
func TestPageRank_HubScoresHighest(t *testing.T) {
	adj := map[string]map[string]struct{}{
		"hub":  {},
		"leaf": {},
		"i1":   {"hub": {}},
		"i2":   {"hub": {}},
		"i3":   {"hub": {}},
	}
	scores := NewPageRank().Compute(adj)
	if scores["hub"] <= scores["leaf"] {
		t.Errorf("hub (%.5f) should outrank leaf (%.5f)", scores["hub"], scores["leaf"])
	}
	if scores["hub"] <= scores["i1"] {
		t.Errorf("hub (%.5f) should outrank an importer (%.5f)", scores["hub"], scores["i1"])
	}
}

// TestPageRank_Deterministic verifies repeated runs on the same graph agree.
func TestPageRank_Deterministic(t *testing.T) {
	adj := map[string]map[string]struct{}{
		"a": {"b": {}}, "b": {"c": {}}, "c": {"a": {}},
	}
	s1 := NewPageRank().Compute(adj)
	s2 := NewPageRank().Compute(adj)
	for k := range s1 {
		if math.Abs(s1[k]-s2[k]) > 1e-12 {
			t.Errorf("non-deterministic score for %q: %.12f vs %.12f", k, s1[k], s2[k])
		}
	}
}

// TestPageRank_Empty returns an empty map for an empty graph.
func TestPageRank_Empty(t *testing.T) {
	if got := NewPageRank().Compute(map[string]map[string]struct{}{}); len(got) != 0 {
		t.Errorf("empty graph should yield empty scores; got %v", got)
	}
}
