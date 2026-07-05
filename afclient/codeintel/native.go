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

	// extractCount is a test seam: it counts how many times a language
	// extractor was actually invoked on a file (the expensive parse step). The
	// incremental hot path must NOT re-invoke extractors for unchanged files, so
	// tests assert this counter stays flat across repeated builds of an
	// unchanged tree. Atomic for the concurrent warm-path model.
	extractCount atomic.Int64
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
const IndexSchemaVersion = 2

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
	var paths []string
	err := filepath.WalkDir(n.cwd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		name := d.Name()
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
		// Apply optional file-pattern filter (simple glob via filepath.Match).
		if len(filePatterns) > 0 {
			matched := false
			for _, pat := range filePatterns {
				if ok, _ := filepath.Match(pat, rel); ok {
					matched = true
					break
				}
				if ok, _ := filepath.Match(pat, name); ok {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
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
	return idx, nil
}

// newFileIndex assembles a schema-v2 FileIndex for a freshly-extracted file,
// computing the content-identity fields (ContentHash, SimHash) over the file's
// normalised content so dedup can compare against them later.
func (n *NativeRunner) newFileIndex(relPath, gitHash string, raw []byte, ast FileAST) FileIndex {
	normalized := normalizeDupContent(string(raw))
	fi := FileIndex{
		FilePath:    relPath,
		GitHash:     gitHash,
		ContentHash: ContentXXHash64(normalized),
		SimHash:     SimHashCompute(normalized),
		Symbols:     ast.Symbols,
		Imports:     ast.Imports,
		Exports:     ast.Exports,
		LastIndexed: time.Now().UnixMilli(),
	}
	if fi.Symbols == nil {
		fi.Symbols = []CodeSymbol{}
	}
	return fi
}

// ── GetRepoMap ────────────────────────────────────────────────────────────────

// GetRepoMapNative builds (or loads) the code index and returns a
// PageRank-ranked repository map as a slice of RepoMapEntry values serialised
// to JSON-compatible any.
//
// The ranking heuristic assigns each file a score based on the number of
// exported symbols and penalises test files. This intentionally mirrors the
// spirit of the TS implementation (BM25 + PageRank) without the full graph
// computation, which is deferred to S3.
func (n *NativeRunner) GetRepoMapNative(opts GetRepoMapOptions) (any, error) {
	idx, err := n.BuildIndex(opts)
	if err != nil {
		return nil, err
	}

	var entries []RepoMapEntry
	for _, fi := range idx.Files {
		rank := computeFileRank(fi)
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
			Rank:     rank,
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

// computeFileRank assigns a rank score to a file.
func computeFileRank(fi FileIndex) float64 {
	exported := 0
	for _, s := range fi.Symbols {
		if s.Exported {
			exported++
		}
	}
	score := float64(exported)*2 + float64(len(fi.Symbols))
	// Penalise test files.
	if strings.HasSuffix(fi.FilePath, "_test.go") ||
		strings.Contains(fi.FilePath, "_test.") ||
		strings.Contains(fi.FilePath, ".test.") ||
		strings.Contains(fi.FilePath, ".spec.") {
		score *= 0.3
	}
	return score
}

// ── SearchSymbols ─────────────────────────────────────────────────────────────

// SearchSymbolsNative searches the code index for symbols matching the query.
// It uses a simple BM25-inspired scoring: exact-name match > prefix match >
// contains match, filtered optionally by kind and file pattern.
func (n *NativeRunner) SearchSymbolsNative(opts SearchSymbolsOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required")
	}

	idx, err := n.BuildIndex(GetRepoMapOptions{})
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
		// File-pattern filter.
		if opts.FilePattern != "" {
			matched, _ := filepath.Match(opts.FilePattern, fi.FilePath)
			if !matched {
				matched, _ = filepath.Match(opts.FilePattern, filepath.Base(fi.FilePath))
			}
			if !matched {
				continue
			}
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

	// Sort descending by score.
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].symbol.Name < results[j].symbol.Name
	})

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
			"symbol":    r.symbol,
			"score":     r.score,
			"matchType": matchType(r.symbol.Name, strings.ToLower(opts.Query)),
		})
	}
	return out, nil
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

	idx, err := n.BuildIndex(GetRepoMapOptions{})
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
	type result struct {
		symbol    CodeSymbol
		score     float64
		matchType string
	}
	var results []result

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
		results = append(results, result{sym, finalScore, matchType})
	}

	// Re-sort after boosting.
	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

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
			"symbol":    r.symbol,
			"score":     r.score,
			"matchType": r.matchType,
		})
	}
	return out, nil
}

// ── CheckDuplicate (S3) ───────────────────────────────────────────────────────

// CheckDuplicateNative checks content for exact or near-duplicate matches in
// the current index using xxHash64 (Tier 1) and SimHash (Tier 2).
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

	idx, err := n.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		return nil, err
	}

	// Collect FileIndex entries as the corpus.
	corpus := make([]FileIndex, 0, len(idx.Files))
	for _, fi := range idx.Files {
		corpus = append(corpus, fi)
	}

	result := CheckDuplicateContent(content, corpus, SimHashDefaultThreshold)
	return map[string]any{
		"isDuplicate":     result.IsDuplicate,
		"matchType":       result.MatchType,
		"existingId":      result.ExistingID,
		"hammingDistance": result.HammingDistance,
	}, nil
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
