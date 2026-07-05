package codeintel

// import_graph.go — file-level dependency graph built from per-file import
// specifiers. Ported from the TS DependencyGraph in
// donmai-libraries/packages/code-intelligence/src/repo-map/dependency-graph.ts.
//
// Edges point from an importing file to the imported file. Import resolution is
// faithful to the TS reference: only RELATIVE specifiers (starting with "." or
// "/") are resolved, against the importing file's directory, with the same
// extension and /index fallbacks. Bare/package specifiers — Go/Python/Rust
// package paths, node_modules — do NOT resolve to an intra-repo file and produce
// no edge, exactly as in the reference (whose graph is a JS/TS relative-import
// graph). This is a documented limitation: PageRank is only meaningful for repos
// whose intra-repo dependencies are expressed as relative imports.

import "strings"

// ImportGraph holds forward and reverse adjacency over indexed files.
type ImportGraph struct {
	adjacency map[string]map[string]struct{} // file -> files it imports
	reverse   map[string]map[string]struct{} // file -> files that import it
	allFiles  map[string]struct{}
}

// NewImportGraph returns an empty ImportGraph.
func NewImportGraph() *ImportGraph {
	return &ImportGraph{
		adjacency: map[string]map[string]struct{}{},
		reverse:   map[string]map[string]struct{}{},
		allFiles:  map[string]struct{}{},
	}
}

// BuildFromIndex builds the graph from the persisted per-file import specifiers.
// File keys are slash-normalised so resolution is platform-independent.
func (g *ImportGraph) BuildFromIndex(files map[string]FileIndex) {
	g.adjacency = make(map[string]map[string]struct{}, len(files))
	g.reverse = make(map[string]map[string]struct{}, len(files))
	g.allFiles = make(map[string]struct{}, len(files))

	known := make(map[string]struct{}, len(files))
	for path := range files {
		known[normalizeSlash(path)] = struct{}{}
	}

	for path, fi := range files {
		from := normalizeSlash(path)
		g.allFiles[from] = struct{}{}
		if g.adjacency[from] == nil {
			g.adjacency[from] = map[string]struct{}{}
		}
		for _, imp := range fi.Imports {
			resolved, ok := resolveImport(imp, from, known)
			if !ok {
				continue
			}
			g.adjacency[from][resolved] = struct{}{}
			if g.reverse[resolved] == nil {
				g.reverse[resolved] = map[string]struct{}{}
			}
			g.reverse[resolved][from] = struct{}{}
		}
	}
}

// Adjacency returns a fresh file -> imported-files map for PageRank. Every known
// file appears as a key (possibly with an empty set) so PageRank ranks all nodes.
func (g *ImportGraph) Adjacency() map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(g.allFiles))
	for f := range g.allFiles {
		links := make(map[string]struct{}, len(g.adjacency[f]))
		for to := range g.adjacency[f] {
			links[to] = struct{}{}
		}
		out[f] = links
	}
	return out
}

// Dependencies returns the (slash-normalised) files that path imports.
func (g *ImportGraph) Dependencies(path string) []string {
	return keysOf(g.adjacency[normalizeSlash(path)])
}

// Dependents returns the (slash-normalised) files that import path.
func (g *ImportGraph) Dependents(path string) []string {
	return keysOf(g.reverse[normalizeSlash(path)])
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// resolveImport maps a relative import specifier to an indexed file path.
// Faithful port of DependencyGraph.resolveImport.
func resolveImport(importPath, fromFile string, known map[string]struct{}) (string, bool) {
	// Skip node_modules / external / package imports.
	if !strings.HasPrefix(importPath, ".") && !strings.HasPrefix(importPath, "/") {
		return "", false
	}
	fromDir := ""
	if idx := strings.LastIndex(fromFile, "/"); idx >= 0 {
		fromDir = fromFile[:idx]
	}
	resolved := normalizeImportPath(fromDir + "/" + importPath)
	resolved = stripCodeExt(resolved)

	exts := []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs"}
	for _, ext := range exts {
		if _, ok := known[resolved+ext]; ok {
			return resolved + ext, true
		}
	}
	for _, ext := range exts {
		if _, ok := known[resolved+"/index"+ext]; ok {
			return resolved + "/index" + ext, true
		}
	}
	return "", false
}

// normalizeImportPath collapses "." and ".." segments. Faithful port of
// DependencyGraph.normalizePath.
func normalizeImportPath(path string) string {
	var parts []string
	for _, part := range strings.Split(path, "/") {
		switch {
		case part == "." || part == "":
			continue
		case part == "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
			// A leading ".." with no parent is dropped (matches the TS reference).
		default:
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

// stripCodeExt removes a trailing JS/TS source extension, matching the TS
// regex /\.(js|ts|tsx|jsx|mjs|cjs)$/.
func stripCodeExt(path string) string {
	for _, ext := range []string{".tsx", ".jsx", ".mjs", ".cjs", ".ts", ".js"} {
		if strings.HasSuffix(path, ext) {
			return strings.TrimSuffix(path, ext)
		}
	}
	return path
}

// normalizeSlash converts OS path separators to "/" so import resolution is
// platform-independent.
func normalizeSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
