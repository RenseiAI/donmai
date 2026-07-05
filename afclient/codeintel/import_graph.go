package codeintel

// import_graph.go — file-level dependency graph built from per-file import
// specifiers. Originally ported from the TS DependencyGraph in
// donmai-libraries/packages/code-intelligence/src/repo-map/dependency-graph.ts,
// then extended so PageRank is meaningful for ALL four supported languages, not
// only JS/TS.
//
// Edges point from an importing file to the imported file(s). Resolution is
// language-aware, keyed off the importing file's extension:
//
//   - JS/TS: RELATIVE specifiers (starting with "." or "/") are resolved against
//     the importing file's directory with extension and /index fallbacks (the
//     original TS-faithful behaviour). Bare/package specifiers do not edge.
//   - Go: package imports are resolved to the intra-repo package directory. When
//     the repo's go.mod module path is known (GoModulePrefix), an import under
//     that prefix maps to the corresponding directory and edges to every .go
//     file in it. Otherwise a longest-suffix match against known package
//     directories is used, so `example.com/mod/pkg/sub` still resolves to a repo
//     dir `pkg/sub`.
//   - Python: dotted module paths (`pkg.mod`) and relative imports (`.mod`,
//     `..pkg.mod`) resolve to `<path>.py` or `<path>/__init__.py`.
//   - Rust: `use` paths (`crate::a::b`, `self::m`, `super::m`) resolve to the
//     corresponding `.rs` file or `mod.rs` via longest-suffix match.
//
// Package/external specifiers that resolve to no indexed file (node_modules, the
// stdlib, third-party crates/modules) produce no edge — only intra-repo
// dependencies contribute to PageRank.

import "strings"

// ImportGraph holds forward and reverse adjacency over indexed files.
type ImportGraph struct {
	adjacency map[string]map[string]struct{} // file -> files it imports
	reverse   map[string]map[string]struct{} // file -> files that import it
	allFiles  map[string]struct{}

	// GoModulePrefix, when set (from the repo's go.mod `module` line), lets Go
	// package imports under that prefix resolve exactly to their intra-repo
	// directory instead of relying on a suffix heuristic. Empty is fine — the
	// suffix fallback still resolves most intra-repo Go imports.
	GoModulePrefix string
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
	// goPkgDirs maps a package directory (slash path) to the .go files in it.
	// rustFiles is the set of slash-normalised .rs paths (extension stripped) for
	// suffix resolution.
	goPkgDirs := make(map[string][]string, len(files))
	for path := range files {
		np := normalizeSlash(path)
		known[np] = struct{}{}
		// A Go `import "pkg"` compiles the package's non-test .go files only;
		// *_test.go files are never pulled in by an importer, so they must not be
		// edge targets (otherwise every test file in the most-imported package
		// would tie with its production files at the top of the ranking).
		if strings.HasSuffix(np, ".go") && !strings.HasSuffix(np, "_test.go") {
			dir := dirOfSlash(np)
			goPkgDirs[dir] = append(goPkgDirs[dir], np)
		}
	}

	for path, fi := range files {
		from := normalizeSlash(path)
		g.allFiles[from] = struct{}{}
		if g.adjacency[from] == nil {
			g.adjacency[from] = map[string]struct{}{}
		}
		lang := fileLang(from)
		for _, imp := range fi.Imports {
			for _, resolved := range g.resolveEdges(lang, imp, from, known, goPkgDirs) {
				if resolved == from {
					continue // never self-edge
				}
				g.adjacency[from][resolved] = struct{}{}
				if g.reverse[resolved] == nil {
					g.reverse[resolved] = map[string]struct{}{}
				}
				g.reverse[resolved][from] = struct{}{}
			}
		}
	}
}

// resolveEdges maps one import specifier to zero or more indexed target files,
// dispatching on the importing file's language.
func (g *ImportGraph) resolveEdges(lang, importPath, fromFile string, known map[string]struct{}, goPkgDirs map[string][]string) []string {
	switch lang {
	case "go":
		return g.resolveGoImport(importPath, goPkgDirs)
	case "python":
		return resolvePythonImport(importPath, fromFile, known)
	case "rust":
		return resolveRustImport(importPath, known)
	default: // js/ts
		if r, ok := resolveRelativeImport(importPath, fromFile, known); ok {
			return []string{r}
		}
		return nil
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

// ── Go resolution ─────────────────────────────────────────────────────────────

// resolveGoImport maps a Go package import path to the .go files of the
// intra-repo package directory it names. When GoModulePrefix is known and the
// import lives under it, the mapping is exact; otherwise a longest-suffix match
// against known package directories is used.
func (g *ImportGraph) resolveGoImport(importPath string, goPkgDirs map[string][]string) []string {
	importPath = strings.Trim(importPath, "/")
	if importPath == "" {
		return nil
	}
	// Exact module-prefix mapping when go.mod is known.
	if g.GoModulePrefix != "" {
		if importPath == g.GoModulePrefix {
			if files, ok := goPkgDirs[""]; ok {
				return files
			}
			return nil
		}
		if rel, ok := strings.CutPrefix(importPath, g.GoModulePrefix+"/"); ok {
			if files, ok := goPkgDirs[rel]; ok {
				return files
			}
			return nil
		}
		// Import is outside this module (stdlib / third-party) — no edge.
		return nil
	}
	// Fallback: longest-suffix match against known package dirs.
	segs := strings.Split(importPath, "/")
	for start := 0; start < len(segs); start++ {
		cand := strings.Join(segs[start:], "/")
		if files, ok := goPkgDirs[cand]; ok {
			return files
		}
	}
	return nil
}

// ── Python resolution ─────────────────────────────────────────────────────────

// resolvePythonImport maps a Python module specifier to a single indexed file.
// Handles absolute dotted paths (`pkg.mod`) and relative imports (`.mod`,
// `..pkg`), resolving to `<path>.py` or `<path>/__init__.py`.
func resolvePythonImport(importPath, fromFile string, known map[string]struct{}) []string {
	importPath = strings.TrimSpace(importPath)
	if importPath == "" {
		return nil
	}
	var base string
	if strings.HasPrefix(importPath, ".") {
		// Relative import: count leading dots. One dot = current package dir.
		dots := 0
		for dots < len(importPath) && importPath[dots] == '.' {
			dots++
		}
		rest := importPath[dots:]
		dir := dirOfSlash(fromFile)
		for i := 1; i < dots; i++ { // each extra dot climbs one parent
			dir = parentDirSlash(dir)
		}
		base = dir
		if rest != "" {
			base = joinSlash(dir, strings.ReplaceAll(rest, ".", "/"))
		}
	} else {
		base = strings.ReplaceAll(importPath, ".", "/")
	}
	if r, ok := lookupPy(base, known); ok {
		return []string{r}
	}
	// Absolute-import fallback for src/-style layouts: drop leading segments.
	if !strings.HasPrefix(importPath, ".") {
		segs := strings.Split(base, "/")
		for start := 1; start < len(segs); start++ {
			if r, ok := lookupPy(strings.Join(segs[start:], "/"), known); ok {
				return []string{r}
			}
		}
	}
	return nil
}

func lookupPy(base string, known map[string]struct{}) (string, bool) {
	for _, cand := range []string{base + ".py", base + "/__init__.py"} {
		if _, ok := known[cand]; ok {
			return cand, true
		}
	}
	return "", false
}

// ── Rust resolution ───────────────────────────────────────────────────────────

// resolveRustImport maps a Rust `use` path to a single indexed .rs file via a
// longest-suffix match. `crate::`, `self::`, `super::` and any trailing brace
// group (`::{a, b}`) are stripped before matching.
func resolveRustImport(importPath string, known map[string]struct{}) []string {
	// Drop a trailing brace group: `a::b::{c, d}` -> `a::b`.
	if i := strings.Index(importPath, "{"); i >= 0 {
		importPath = importPath[:i]
	}
	importPath = strings.TrimSpace(importPath)
	segs := []string{}
	for _, s := range strings.Split(importPath, "::") {
		s = strings.TrimSpace(s)
		switch s {
		case "", "crate", "self", "super":
			continue
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return nil
	}
	// The final segment is usually the imported item, not part of the file path;
	// try matching both with and without it, longest first.
	for drop := 0; drop <= 1 && drop < len(segs); drop++ {
		p := segs[:len(segs)-drop]
		if len(p) == 0 {
			continue
		}
		for start := 0; start < len(p); start++ {
			cand := strings.Join(p[start:], "/")
			if r, ok := lookupRs(cand, known); ok {
				return []string{r}
			}
		}
	}
	return nil
}

func lookupRs(base string, known map[string]struct{}) (string, bool) {
	for _, cand := range []string{base + ".rs", base + "/mod.rs", "src/" + base + ".rs", "src/" + base + "/mod.rs"} {
		if _, ok := known[cand]; ok {
			return cand, true
		}
	}
	return "", false
}

// ── JS/TS relative resolution (unchanged TS-faithful behaviour) ───────────────

// resolveRelativeImport maps a relative JS/TS import specifier to an indexed
// file path. Faithful port of DependencyGraph.resolveImport.
func resolveRelativeImport(importPath, fromFile string, known map[string]struct{}) (string, bool) {
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

// ── path helpers (slash-normalised, platform-independent) ─────────────────────

// fileLang classifies a slash-path by extension into a resolver bucket.
func fileLang(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"):
		return "go"
	case strings.HasSuffix(path, ".py"):
		return "python"
	case strings.HasSuffix(path, ".rs"):
		return "rust"
	default:
		return "js"
	}
}

// dirOfSlash returns the directory portion of a slash path ("" for a root file).
func dirOfSlash(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[:idx]
	}
	return ""
}

// parentDirSlash returns the parent directory of a slash path ("" at the root).
func parentDirSlash(dir string) string {
	if dir == "" {
		return ""
	}
	return dirOfSlash(dir)
}

// joinSlash joins a directory and a relative sub-path with "/".
func joinSlash(dir, sub string) string {
	if dir == "" {
		return sub
	}
	if sub == "" {
		return dir
	}
	return dir + "/" + sub
}

// normalizeSlash converts OS path separators to "/" so import resolution is
// platform-independent.
func normalizeSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
