package codeintel

import (
	"crypto/sha1" //nolint:gosec // sha1 is used only for git-compatible blob hashing, not security
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	xxhash "github.com/cespare/xxhash/v2"
)

// NativeRunner provides the S0–S3 code intelligence suite using pure Go
// implementations (no external binary required). It covers:
//
//   - GetRepoMap      — builds / loads index.json, produces a ranked file map
//   - SearchSymbols   — BM25-style symbol search over the native index
//   - SearchCode      — BM25 full-text search over the symbol corpus (S2)
//   - CheckDuplicate  — xxHash64 exact + SimHash near-dup detection (S3)
//   - FindTypeUsages  — regex scan for type reference sites (S3)
//   - ValidateCrossDeps — cross-package import validator (S3)
//
// The native path is the PRIMARY implementation. The exec-shim is ONLY
// consulted when DONMAI_CODE_BIN is set, which operators can use for testing
// or to force the TypeScript path.
type NativeRunner struct {
	cwd      string
	indexDir string

	tsExtractor     *TypeScriptExtractor
	goExtractor     *GoExtractor
	pythonExtractor *PythonExtractor
	rustExtractor   *RustExtractor

	// mu guards the persisted-index disk I/O (loadIndex/saveIndex) and the
	// in-process warm cache below. A long-lived process (the Wave-2 MCP server)
	// shares one NativeRunner across concurrent tool calls; the RWMutex makes
	// those calls safe. The single-shot CLI creates a fresh runner per process,
	// so the lock is uncontended there and behaviour is unchanged.
	mu sync.RWMutex

	// cached is the in-process warm index. Once a build populates it, index-
	// consuming queries reuse it WITHOUT re-walking the tree or re-hashing files
	// (an explicit staleness contract — see Refresh/Invalidate). nil == cold.
	cached *IndexFile

	// extractCount is a test seam: it counts how many times a language
	// extractor was actually invoked on a file (the expensive parse step). The
	// incremental hot path must NOT re-invoke extractors for unchanged files, so
	// tests assert this counter stays flat across repeated builds of an
	// unchanged tree. Atomic for the concurrent warm-path model.
	extractCount atomic.Int64

	// walkCount is a test seam: it counts full filepath.WalkDir passes. The warm
	// path must serve queries without re-walking, so tests assert this stays flat
	// across repeated queries on one runner.
	walkCount atomic.Int64
}

// NewNativeRunner creates a NativeRunner that operates relative to cwd.
func NewNativeRunner(cwd string) *NativeRunner {
	return &NativeRunner{
		cwd:             cwd,
		indexDir:        ".donmai/code-index",
		tsExtractor:     &TypeScriptExtractor{},
		goExtractor:     &GoExtractor{},
		pythonExtractor: &PythonExtractor{},
		rustExtractor:   &RustExtractor{},
	}
}

// ── index.json persistence ────────────────────────────────────────────────────

// IndexSchemaVersion is the authoritative on-disk schema version for
// .donmai/code-index/index.json. A persisted index whose top-level "version"
// does not equal this value is discarded on load and rebuilt from scratch (a
// clean full rebuild, never a half-migration). Bump this whenever the persisted
// FileIndex/IndexFile shape changes in a way that makes stale data unusable.
//
//	v1: { files, rootHash }                          (legacy, TS-compatible)
//	v2: { version, files{…,contentHash,simHash,imports,exports}, rootHash }
//	v3: symbol line/endLine are 1-based declaration-keyword lines (v2 stored
//	    0-based indexes, reporting every definition one line early)
//	v4: per-file symbolHashes (symbol-granular dedup fingerprints; a v3 index
//	    lacks them, so file-buried duplicates would silently miss)
//	v5: body extents come from the string/comment-aware block scanner — v4
//	    extents could be truncated by a '}' inside a string literal or
//	    comment (wrong symbol fingerprints) and TS functions/methods carried
//	    no extents at all (missing fingerprints), so stale v4 hashes must be
//	    rebuilt
//	v6: dedup fingerprints (file + symbol ContentHash/SimHash) are computed
//	    over comment-STRIPPED normalized content (normalizeDupContent) — v5
//	    hashes embed comment tokens, so a query normalized under v6 rules
//	    could never match them (silent dedup false negatives; the
//	    codeintel-dedup-donmai-001 renamed-function regression)
const IndexSchemaVersion = 6

// loadIndex attempts to read the persisted index.json. Returns an empty
// IndexFile if the file does not exist, cannot be decoded, or carries a schema
// version other than IndexSchemaVersion (which forces a clean full rebuild).
func (n *NativeRunner) loadIndex() IndexFile {
	empty := IndexFile{Version: IndexSchemaVersion, Files: map[string]FileIndex{}}
	path := filepath.Join(n.cwd, n.indexDir, "index.json")
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from cwd
	if err != nil {
		return empty
	}
	var idx IndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		return empty
	}
	// Version gate: a missing (0) or older/newer version means the persisted
	// data does not match the current schema — discard it and rebuild clean.
	if idx.Version != IndexSchemaVersion {
		return empty
	}
	if idx.Files == nil {
		idx.Files = map[string]FileIndex{}
	}
	return idx
}

// saveIndex persists index.json under .donmai/code-index/.
func (n *NativeRunner) saveIndex(idx IndexFile) error {
	dir := filepath.Join(n.cwd, n.indexDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, data, 0o640); err != nil { //nolint:gosec // G306 intentional — 640 is appropriate for index files
		return fmt.Errorf("write index: %w", err)
	}
	return nil
}

// ── file discovery ────────────────────────────────────────────────────────────

// indexableExtensions lists the file extensions whose language is supported by
// the native extractors (S2 scope: TypeScript/JS + Go + Python + Rust).
var indexableExtensions = map[string]bool{
	".ts":  true,
	".tsx": true,
	".js":  true,
	".jsx": true,
	".mjs": true,
	".cjs": true,
	".go":  true,
	".py":  true,
	".rs":  true,
}

// skipDirs lists directory names that should never be indexed.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".donmai":      true,
	"dist":         true,
	"build":        true,
	".next":        true,
}

// discoverFiles walks the cwd and returns relative paths of indexable files.
func (n *NativeRunner) discoverFiles(filePatterns []string) ([]string, error) {
	n.walkCount.Add(1) // test seam: one full-tree walk
	var paths []string
	err := filepath.WalkDir(n.cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		name := d.Name()
		// Never follow symlinks. WalkDir already declines to DESCEND into a
		// symlinked directory, but a symlinked *file* dirent has IsDir()==false
		// and would otherwise pass the extension filter and be os.ReadFile'd,
		// transparently following the link — a repo could plant leak.go ->
		// /etc/... or ../outside-secret/x.go and exfiltrate arbitrary host files
		// into the index (and, with hybrid search, off the box). A symlink dirent
		// reports IsDir()==false, so skipping it here excludes both symlinked
		// files and symlinked directories (WalkDir already never descends into a
		// symlinked directory).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !indexableExtensions[ext] {
			return nil
		}
		rel, err := filepath.Rel(n.cwd, path)
		if err != nil {
			return nil
		}
		// Apply optional file-pattern filter (TS-faithful glob semantics, so a
		// trailing "/**" matches the whole subtree). Shared with the get-repo-map
		// output filter via matchesAnyPattern.
		if len(filePatterns) > 0 && !matchesAnyPattern(rel, filePatterns) {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	return paths, err
}

// ── hashing ───────────────────────────────────────────────────────────────────

// gitBlobHash computes a git-compatible SHA1 blob hash for content.
// This matches the TS GitHashProvider.hashContent() method exactly.
func gitBlobHash(content []byte) string {
	header := fmt.Sprintf("blob %d\x00", len(content))
	h := sha1.New() //nolint:gosec // sha1 is required for git compatibility
	_, _ = h.Write([]byte(header))
	_, _ = h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ContentXXHash64 computes the xxHash64 (seed=0) of content as a hex string.
// This matches the TS xxhash64() function which calls h64ToString(content)
// from the xxhash-wasm package.
//
// The xxhash-wasm h64ToString function uses seed=0 (default) and returns the
// hash as a lowercase hex string. github.com/cespare/xxhash/v2 uses seed=0
// by default and produces identical output.
func ContentXXHash64(content string) string {
	return fmt.Sprintf("%016x", xxhash.Sum64String(content))
}

// ── extraction ────────────────────────────────────────────────────────────────

// extractAST dispatches to the appropriate language extractor.
func (n *NativeRunner) extractAST(source, filePath string) FileAST {
	n.extractCount.Add(1) // test seam: one language-extractor invocation
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return n.tsExtractor.Extract(source, filePath)
	case ".go":
		return n.goExtractor.Extract(source, filePath)
	case ".py":
		return n.pythonExtractor.Extract(source, filePath)
	case ".rs":
		return n.rustExtractor.Extract(source, filePath)
	default:
		return FileAST{FilePath: filePath, Language: "unknown"}
	}
}

// ── BuildIndex builds / incrementally updates the index ──────────────────────

// BuildIndex updates the persisted index incrementally and returns it.
//
// It performs a cheap pass over the repository — read + git-blob-hash every
// indexable file — then diffs the resulting Merkle tree against the persisted
// index (MerkleTreeFromIndex / MerkleDiff / MerkleIdentical). The expensive
// language extractor is invoked ONLY on added or modified files; unchanged
// files keep their existing FileIndex verbatim and deleted files are dropped.
// This is the fix for the long-standing bug where the git-blob hash was compared
// only AFTER extraction had already run, so the hash check skipped the map write
// but not the parse — making "incremental" indexing re-parse the whole tree on
// every call.
//
// opts.FilePatterns is NOT applied here: it is an OUTPUT filter (see
// GetRepoMapNative), never an index-scope filter. The persisted index always
// covers the entire indexable tree so the import graph (PageRank) and the
// in-process warm cache stay coherent regardless of which query invoked it.
func (n *NativeRunner) BuildIndex(_ GetRepoMapOptions) (IndexFile, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buildIndexLocked()
}

// buildIndexLocked is the disk-path incremental builder. Callers must hold
// n.mu for writing. It also refreshes the in-process warm cache so subsequent
// queries can reuse it without re-walking.
func (n *NativeRunner) buildIndexLocked() (IndexFile, error) {
	files, err := n.discoverFiles(nil)
	if err != nil {
		return IndexFile{}, fmt.Errorf("discover files: %w", err)
	}

	existing := n.loadIndex()

	// Cheap pass: read + git-blob-hash every discovered file. Retain the raw
	// bytes so a changed file can be extracted without a second read. Binary /
	// unreadable files are skipped (never indexed), matching prior behaviour.
	newHashes := make(map[string]string, len(files))
	rawByPath := make(map[string][]byte, len(files))
	for _, relPath := range files {
		raw, rerr := os.ReadFile(filepath.Join(n.cwd, relPath)) //nolint:gosec // path from cwd
		if rerr != nil || !utf8.Valid(raw) {
			continue
		}
		newHashes[relPath] = gitBlobHash(raw)
		rawByPath[relPath] = raw
	}

	oldTree := MerkleTreeFromIndex(existing)
	newTree := MerkleTreeFromHashes(newHashes)

	// Fast path: the tree is byte-identical to what's persisted — nothing
	// changed. Reuse the existing index wholesale (no extraction, no re-save).
	if MerkleIdentical(oldTree, newTree) && len(existing.Files) == len(newHashes) {
		existing.Version = IndexSchemaVersion
		n.cached = &existing
		return existing, nil
	}

	// Otherwise compute the precise added/modified/deleted change set and
	// re-extract only what actually changed.
	changes := MerkleDiff(oldTree, newTree)
	changed := make(map[string]bool, len(changes.Added)+len(changes.Modified))
	for _, p := range changes.Added {
		changed[p] = true
	}
	for _, p := range changes.Modified {
		changed[p] = true
	}

	newFiles := make(map[string]FileIndex, len(newHashes))
	for relPath, hash := range newHashes {
		if !changed[relPath] {
			if prev, ok := existing.Files[relPath]; ok {
				newFiles[relPath] = prev // unchanged: reuse verbatim, no extraction
				continue
			}
		}
		ast := n.extractAST(string(rawByPath[relPath]), relPath) // counted extraction
		newFiles[relPath] = n.newFileIndex(relPath, hash, rawByPath[relPath], ast)
	}
	// changes.Deleted paths are simply absent from newFiles.

	idx := IndexFile{
		Version:  IndexSchemaVersion,
		Files:    newFiles,
		RootHash: newTree.RootHash(),
	}
	if err := n.saveIndex(idx); err != nil {
		return idx, fmt.Errorf("save index: %w", err)
	}
	n.cached = &idx
	return idx, nil
}

// ── in-process warm path (Wave-2 MCP server) ─────────────────────────────────
//
// A long-lived MCP-server process constructs ONE NativeRunner and calls its
// query methods repeatedly. indexForQuery serves those calls from an in-memory
// cache, skipping the disk load AND the full-tree walk+rehash entirely once the
// index is warm. Staleness is an EXPLICIT contract, not a per-call re-check
// (re-hashing every file each call is O(files), which defeats the point): the
// server invalidates or refreshes the cache when it knows the working tree
// changed — e.g. after an agent tool writes files, or on a filesystem-watch
// debounce.
//
//	Refresh()    — eagerly rebuild from disk now (re-walk + selective
//	               re-extraction of changed files) and re-warm the cache.
//	Invalidate() — drop the cache; the next query rebuilds lazily.
//
// The single-shot CLI never calls these: it builds once and exits, so its
// behaviour is byte-for-byte identical to before the cache existed.

// indexForQuery returns the index for a read-only query, using the warm cache
// when available and falling back to a disk build (which re-warms it) when cold.
func (n *NativeRunner) indexForQuery() (IndexFile, error) {
	n.mu.RLock()
	if n.cached != nil {
		idx := *n.cached
		n.mu.RUnlock()
		return idx, nil
	}
	n.mu.RUnlock()
	// Cold: build from disk (takes the write lock internally). A concurrent
	// builder may win the race; that is fine — the build is idempotent.
	return n.BuildIndex(GetRepoMapOptions{})
}

// Refresh eagerly rebuilds the index from disk and re-warms the in-process
// cache. The MCP server calls this when it knows the working tree may have
// changed. Cheap when nothing changed (Merkle short-circuit), proportional to
// the change set otherwise.
func (n *NativeRunner) Refresh() error {
	_, err := n.BuildIndex(GetRepoMapOptions{})
	return err
}

// Invalidate drops the in-process cache so the next index-consuming call
// rebuilds from disk. Cheaper than Refresh when an immediate rebuild is not
// required (lazy re-warm on the next tool call).
func (n *NativeRunner) Invalidate() {
	n.mu.Lock()
	n.cached = nil
	n.mu.Unlock()
}

// newFileIndex assembles a schema-v4 FileIndex for a freshly-extracted file,
// computing the content-identity fields (ContentHash, SimHash) over the file's
// normalised content, plus the symbol-granular dedup fingerprints
// (SymbolHashes), so dedup can compare against them later. This only runs on
// actual (re)extraction, so per-symbol hashing costs nothing on the
// incremental hash-match fast path.
func (n *NativeRunner) newFileIndex(relPath, gitHash string, raw []byte, ast FileAST) FileIndex {
	content := string(raw)
	normalized := normalizeDupContent(content)
	fi := FileIndex{
		FilePath:     relPath,
		GitHash:      gitHash,
		ContentHash:  ContentXXHash64(normalized),
		SimHash:      SimHashCompute(normalized),
		Symbols:      ast.Symbols,
		Imports:      ast.Imports,
		Exports:      ast.Exports,
		SymbolHashes: ComputeSymbolHashes(content, ast.Symbols),
		LastIndexed:  time.Now().UnixMilli(),
	}
	if fi.Symbols == nil {
		fi.Symbols = []CodeSymbol{}
	}
	return fi
}

// ── GetRepoMap ────────────────────────────────────────────────────────────────

// GetRepoMapNative builds (or loads) the code index and returns a PageRank-ranked
// repository map as a slice of RepoMapEntry values serialised to JSON-compatible
// any.
//
// Ranking is real PageRank (import_graph.go + pagerank.go, ported from the TS
// RepoMapGenerator): an import dependency graph is built over the whole index,
// then PageRank (damping 0.85, tolerance 1e-6) scores each file by structural
// importance. rank IS the PageRank score — the TS reference blends no other
// signal, so neither do we (the old exported*2+symbolCount heuristic and the
// test-file penalty are gone). opts.FilePatterns is an OUTPUT filter over the
// ranked entries (matching RepoMapGenerator); the graph and ranking always span
// the whole repository so a filtered-out hub still contributes its edges.
func (n *NativeRunner) GetRepoMapNative(opts GetRepoMapOptions) (any, error) {
	idx, err := n.indexForQuery()
	if err != nil {
		return nil, err
	}

	graph := NewImportGraph()
	graph.GoModulePrefix = n.goModulePrefix()
	graph.BuildFromIndex(idx.Files)
	ranks := NewPageRank().Compute(graph.Adjacency())

	// Non-nil so an empty/filtered-out result serialises as JSON [] (an array),
	// never null — array-typed consumers must not break on a zero match.
	entries := make([]RepoMapEntry, 0, len(idx.Files))
	for _, fi := range idx.Files {
		// Output filter: limit which files appear, not which are ranked/graphed.
		if len(opts.FilePatterns) > 0 && !matchesAnyPattern(fi.FilePath, opts.FilePatterns) {
			continue
		}
		syms := make([]RepoMapSymbol, 0, len(fi.Symbols))
		for _, s := range fi.Symbols {
			syms = append(syms, RepoMapSymbol{
				Name: s.Name,
				Kind: s.Kind,
				Line: s.Line,
			})
		}
		entries = append(entries, RepoMapEntry{
			FilePath: fi.FilePath,
			Rank:     ranks[normalizeSlash(fi.FilePath)],
			Symbols:  syms,
		})
	}

	// Sort descending by rank, then ascending by path for determinism.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Rank != entries[j].Rank {
			return entries[i].Rank > entries[j].Rank
		}
		return entries[i].FilePath < entries[j].FilePath
	})

	// Apply max-files cap.
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 50
	}
	if maxFiles > len(entries) {
		maxFiles = len(entries)
	}
	entries = entries[:maxFiles]

	return map[string]any{
		"entries":  entries,
		"rootHash": idx.RootHash,
		"files":    len(idx.Files),
	}, nil
}

// goModulePrefix reads the `module` line from go.mod at the index root, if one
// exists, so Go package imports under that prefix resolve exactly to their
// intra-repo directory in the import graph. Returns "" when there is no go.mod
// (or it has no module line), in which case the graph falls back to suffix
// matching. Best-effort: any read/parse failure yields "".
func (n *NativeRunner) goModulePrefix() string {
	data, err := os.ReadFile(filepath.Join(n.cwd, "go.mod")) //nolint:gosec // path from cwd
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			// require a space/tab after the keyword, then the module path
			if trimmed := strings.TrimSpace(rest); trimmed != "" && (rest[0] == ' ' || rest[0] == '\t') {
				// strip an optional trailing comment
				if i := strings.Index(trimmed, "//"); i >= 0 {
					trimmed = strings.TrimSpace(trimmed[:i])
				}
				return trimmed
			}
		}
	}
	return ""
}

// matchGlob reports whether a single file pattern matches a slash-relative path.
//
// It mirrors the TS RepoMapGenerator.matchAnyPattern semantics so the documented
// examples work as written — crucially, a trailing "/**" matches the whole
// subtree (filepath.Match's "**" is a single-level "*" that does NOT cross "/",
// which silently dropped every nested file under a documented "src/**" pattern):
//
//   - "*ext"   → suffix match (path ends with ext), e.g. "*.go" matches
//     "afclient/native.go" as well as "main.go".
//   - "dir/**" → subtree prefix match (path starts with "dir/" or equals "dir").
//   - otherwise → filepath.Match against the full path, then the basename, so
//     single-segment globs like "native.go" still match by basename.
func matchGlob(pattern, path string) bool {
	path = normalizeSlash(path)
	switch {
	case strings.HasPrefix(pattern, "*") && !strings.ContainsAny(pattern[1:], "*?["):
		return strings.HasSuffix(path, pattern[1:])
	case strings.HasSuffix(pattern, "/**"):
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	default:
		if ok, _ := filepath.Match(pattern, path); ok {
			return true
		}
		ok, _ := filepath.Match(pattern, filepath.Base(path))
		return ok
	}
}

// matchesAnyPattern reports whether path matches any of the glob patterns,
// using matchGlob's TS-faithful semantics (trailing "/**" = subtree match).
func matchesAnyPattern(path string, patterns []string) bool {
	for _, pat := range patterns {
		if matchGlob(pat, path) {
			return true
		}
	}
	return false
}

// ── SearchSymbols ─────────────────────────────────────────────────────────────

// SearchSymbolsNative searches the code index for symbols matching the query.
// It uses a simple BM25-inspired scoring: exact-name match > prefix match >
// contains match, filtered optionally by kind and file pattern.
func (n *NativeRunner) SearchSymbolsNative(opts SearchSymbolsOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required")
	}

	idx, err := n.indexForQuery()
	if err != nil {
		return nil, err
	}

	queryLower := strings.ToLower(opts.Query)
	kindsSet := make(map[string]bool, len(opts.Kinds))
	for _, k := range opts.Kinds {
		kindsSet[strings.TrimSpace(k)] = true
	}

	type scored struct {
		symbol CodeSymbol
		score  float64
	}
	var results []scored

	for _, fi := range idx.Files {
		// File-pattern filter (same TS-faithful glob semantics as get-repo-map,
		// so "svc/**" reaches nested files).
		if opts.FilePattern != "" && !matchGlob(opts.FilePattern, fi.FilePath) {
			continue
		}
		for _, sym := range fi.Symbols {
			// Kind filter.
			if len(kindsSet) > 0 && !kindsSet[string(sym.Kind)] {
				continue
			}
			score := scoreSymbol(sym.Name, queryLower)
			if score <= 0 {
				continue
			}
			// Boost exported symbols.
			if sym.Exported {
				score *= 1.5
			}
			results = append(results, scored{sym, score})
		}
	}

	// Sort descending by score with a FULLY-ORDERING tie-break chain
	// (name, filePath, line): candidates come from map iteration, so any tie
	// left to the incoming order makes the result — and any truncation cut —
	// nondeterministic run to run. Exact matches all share the same score AND
	// name, which is why the chain must reach filePath/line.
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.symbol.Name != b.symbol.Name {
			return a.symbol.Name < b.symbol.Name
		}
		if a.symbol.FilePath != b.symbol.FilePath {
			return a.symbol.FilePath < b.symbol.FilePath
		}
		return a.symbol.Line < b.symbol.Line
	})

	// Exact-match short-circuit: when the query names a symbol exactly, the
	// sibling prefix/fuzzy hits are noise — return the exact matches, ALL of
	// them up to symbolExactMaxResults (or an explicit MaxResults). "Where is
	// X defined" with several same-name definitions must surface every
	// definition, never a silent subset; if a cap still truncates, a trailing
	// sentinel reports the omitted count.
	var exacts []scored
	for _, r := range results {
		if strings.ToLower(r.symbol.Name) == queryLower {
			exacts = append(exacts, r)
		}
	}
	truncatedExacts := 0
	if len(exacts) > 0 {
		limit := symbolExactMaxResults
		if opts.MaxResults > 0 {
			limit = opts.MaxResults
		}
		if limit > len(exacts) {
			limit = len(exacts)
		}
		truncatedExacts = len(exacts) - limit
		results = exacts[:limit]
	} else {
		maxResults := opts.MaxResults
		if maxResults <= 0 {
			maxResults = symbolDefaultMaxResults
		}
		if maxResults > len(results) {
			maxResults = len(results)
		}
		results = results[:maxResults]
	}

	out := make([]map[string]any, 0, len(results)+1)
	for _, r := range results {
		out = append(out, map[string]any{
			"symbol":    projectSymbol(r.symbol, opts.IncludeDoc),
			"score":     r.score,
			"matchType": matchType(r.symbol.Name, queryLower),
		})
	}
	if truncatedExacts > 0 {
		// Sentinel final element: no "symbol"/"filePath" keys, so parsers that
		// walk hits (or collect filePath values) skip it safely, while an agent
		// reading the JSON sees the truncation instead of a silent subset.
		out = append(out, map[string]any{
			"truncatedExactMatches": truncatedExacts,
			"hint":                  "raise maxResults to see all exact definitions",
		})
	}
	return out, nil
}

// symbolDefaultMaxResults is the default result cap for symbol lookups when no
// exact match exists. Deliberately small: search-symbols answers "where is X
// defined" and each extra hit is pure token cost for the calling agent.
// Callers can always raise MaxResults explicitly.
const symbolDefaultMaxResults = 5

// symbolExactMaxResults bounds the exact-match short-circuit: when the query
// names a symbol exactly, ALL exact hits are returned up to this hard cap —
// the pre-WS1 effective ceiling, so the short-circuit is never lossier than
// the old default path. An explicit MaxResults overrides the cap in either
// direction. When the cap still truncates, a trailing sentinel element
// {"truncatedExactMatches": n, "hint": …} reports the omitted count so the
// caller is never silently shown a subset of same-name definitions.
const symbolExactMaxResults = 20

// compactDocMaxLen is the rune cap for the truncated one-line documentation in
// the compact (default) search result projection.
const compactDocMaxLen = 160

// firstDocLine reduces a (possibly multi-line) documentation block to its
// trimmed first line, capped at compactDocMaxLen runes with a trailing
// ellipsis when cut.
func firstDocLine(doc string) string {
	if doc == "" {
		return ""
	}
	line := doc
	if idx := strings.IndexByte(doc, '\n'); idx >= 0 {
		line = doc[:idx]
	}
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) > compactDocMaxLen {
		return string(runes[:compactDocMaxLen]) + "…"
	}
	return line
}

// projectSymbol returns the result-payload representation of a symbol.
//
// Default (includeDoc=false) is the compact projection — {name, kind,
// filePath, line, signature, documentation(first line)} — which is what an
// agent needs to jump to the definition; the full multi-line documentation
// block was the dominant token cost per hit and bought no task success.
// includeDoc=true restores the full CodeSymbol (same JSON field names, so the
// compact shape is a strict subset).
func projectSymbol(sym CodeSymbol, includeDoc bool) any {
	if includeDoc {
		return sym
	}
	m := map[string]any{
		"name":     sym.Name,
		"kind":     sym.Kind,
		"filePath": sym.FilePath,
		"line":     sym.Line,
	}
	if sym.Signature != "" {
		m["signature"] = sym.Signature
	}
	if doc := firstDocLine(sym.Documentation); doc != "" {
		m["documentation"] = doc
	}
	if sym.ParentName != "" {
		m["parentName"] = sym.ParentName
	}
	return m
}

// scoreSymbol computes a BM25-inspired relevance score for a symbol name
// against a lower-cased query.
func scoreSymbol(name, queryLower string) float64 {
	nameLower := strings.ToLower(name)
	switch {
	case nameLower == queryLower:
		return 10.0
	case strings.HasPrefix(nameLower, queryLower):
		return 5.0
	case strings.Contains(nameLower, queryLower):
		return 2.0
	default:
		// fuzzy: all query chars appear in order in the name
		if fuzzyMatch(nameLower, queryLower) {
			return 0.5
		}
		return 0
	}
}

// matchType categorises the match quality as exact/fuzzy/bm25.
func matchType(name, queryLower string) string {
	nameLower := strings.ToLower(name)
	if nameLower == queryLower {
		return "exact"
	}
	if strings.HasPrefix(nameLower, queryLower) || strings.Contains(nameLower, queryLower) {
		return "bm25"
	}
	return "fuzzy"
}

// fuzzyMatch returns true if all runes of query appear in order in target.
func fuzzyMatch(target, query string) bool {
	tIdx := 0
	trunes := []rune(target)
	for _, qr := range query {
		found := false
		for ; tIdx < len(trunes); tIdx++ {
			if trunes[tIdx] == qr {
				tIdx++
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ── SearchCode (S2) ───────────────────────────────────────────────────────────

// SearchCodeNative performs full-text BM25 search over the code symbol corpus.
//
// The implementation builds (or loads) the code index, collects all symbols
// into a BM25 inverted index, scores them against the query, and applies an
// optional language filter and max-results cap.
//
// This matches the TS SearchEngine.search + BM25 pipeline:
//   - Tokenizer: code-aware camelCase / snake_case expansion (bm25.go:tokenize)
//   - Scoring: Okapi BM25 (k1=1.5, b=0.75)
//   - Post-processing: exact-name match × 3.0, partial-name match × 1.5
//   - Returns: []map[string]any with "symbol", "score", "matchType" keys
//
// Intentional deviation from TS: the TS SearchEngine holds an in-process
// symbol set that is rebuilt on indexer flush; the Go implementation rebuilds
// on every call from the persisted index.json. This is acceptable because the
// index is persisted to disk and re-read only on content change (incremental).
func (n *NativeRunner) SearchCodeNative(opts SearchCodeOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required for search-code")
	}

	idx, err := n.indexForQuery()
	if err != nil {
		return nil, err
	}

	// Collect all symbols from the index.
	var allSymbols []CodeSymbol
	for _, fi := range idx.Files {
		for _, sym := range fi.Symbols {
			// Language filter (optional).
			if opts.Language != "" && sym.Language != opts.Language {
				continue
			}
			allSymbols = append(allSymbols, sym)
		}
	}

	if len(allSymbols) == 0 {
		return []map[string]any{}, nil
	}

	// Build inverted index + BM25 score.
	invertedIdx := buildInvertedIndex(allSymbols)
	scored := bm25Score(opts.Query, invertedIdx)

	queryLower := strings.ToLower(opts.Query)
	var results []searchCandidate

	for _, sd := range scored {
		sym := allSymbols[sd.docID]
		finalScore := sd.score
		matchType := "bm25"
		nameLower := strings.ToLower(sym.Name)
		if nameLower == queryLower {
			matchType = "exact"
			finalScore *= 3.0
		} else if strings.Contains(nameLower, queryLower) {
			matchType = "fuzzy"
			finalScore *= 1.5
		}
		results = append(results, searchCandidate{sym, finalScore, matchType})
	}

	// Re-sort after boosting.
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	// Optional Voyage+Cohere hybrid rescoring; no-op (zero network) unless
	// VOYAGE_AI_API_KEY is set. See afclient/codeintel/hybrid.go.
	results = applyHybridSearch(opts.Query, results)

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 20
	}
	if maxResults > len(results) {
		maxResults = len(results)
	}

	out := make([]map[string]any, 0, maxResults)
	for _, r := range results[:maxResults] {
		out = append(out, map[string]any{
			"symbol":    projectSymbol(r.symbol, opts.IncludeDoc),
			"score":     r.score,
			"matchType": r.matchType,
		})
	}
	return out, nil
}

// ── CheckDuplicate (S3) ───────────────────────────────────────────────────────

// CheckDuplicateNative checks content for exact or near-duplicate matches in
// the current index using xxHash64 (Tier 1) and SimHash (Tier 2), at both
// file and symbol granularity (schema v4). A symbol-level match carries
// filePath/symbolName/line so the caller can point at the exact duplicate
// site inside a larger file with no grep follow-up.
//
// The result is bounded to the single top match by default — the agent needs
// ONE authoritative answer. opts.MaxResults > 1 opts into a ranked "matches"
// list alongside the top-match fields.
//
// The threshold defaults to SimHashDefaultThreshold (3), matching the TS
// DedupPipeline default.
func (n *NativeRunner) CheckDuplicateNative(opts CheckDuplicateOptions) (any, error) {
	content := opts.Content
	if opts.ContentFile != "" {
		data, err := os.ReadFile(opts.ContentFile) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("read content file: %w", err)
		}
		content = string(data)
	}
	if content == "" {
		return nil, fmt.Errorf("content is required for check-duplicate")
	}

	idx, err := n.indexForQuery()
	if err != nil {
		return nil, err
	}

	// Collect FileIndex entries as the corpus, sorted for deterministic
	// ranking (idx.Files is a map; tie-breaks must not depend on its order).
	corpus := make([]FileIndex, 0, len(idx.Files))
	for _, fi := range idx.Files {
		corpus = append(corpus, fi)
	}
	sort.Slice(corpus, func(i, j int) bool { return corpus[i].FilePath < corpus[j].FilePath })

	matches := FindDuplicateMatches(content, corpus, SimHashDefaultThreshold, opts.MaxResults)

	// JSON contract: the v2 fields (isDuplicate, matchType, existingId,
	// hammingDistance) keep their names and meaning; v4 ADDS filePath,
	// symbolName, line, matches — never renames.
	out := map[string]any{
		"isDuplicate":     false,
		"matchType":       "none",
		"existingId":      "",
		"hammingDistance": 0,
	}
	if len(matches) > 0 {
		top := matches[0]
		out["isDuplicate"] = true
		out["matchType"] = top.MatchType
		out["existingId"] = top.FilePath
		out["hammingDistance"] = top.HammingDistance
		out["filePath"] = top.FilePath
		if top.SymbolName != "" {
			out["symbolName"] = top.SymbolName
			out["line"] = top.Line
		}
	}
	if opts.MaxResults > 1 && len(matches) > 0 {
		out["matches"] = matches
	}
	return out, nil
}

// ── FindTypeUsages (S3) ───────────────────────────────────────────────────────

// FindTypeUsagesNative finds all usage sites for a named type in the repository.
// Delegates to the FindTypeUsages function (type_usages.go).
func (n *NativeRunner) FindTypeUsagesNative(opts FindTypeUsagesOptions) (any, error) {
	result, err := FindTypeUsages(n.cwd, opts.TypeName, opts.MaxResults)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ── ValidateCrossDeps (S3) ────────────────────────────────────────────────────

// ValidateCrossDepsNative validates cross-package imports in the monorepo.
// Delegates to ValidateCrossDeps (crossdeps.go).
func (n *NativeRunner) ValidateCrossDepsNative(opts ValidateCrossDepsOptions) (any, error) {
	result, err := ValidateCrossDeps(n.cwd, opts.Path)
	if err != nil {
		return nil, err
	}
	return result, nil
}
