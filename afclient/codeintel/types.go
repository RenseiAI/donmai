// Package codeintel provides native Go code intelligence (S0+S1).
//
// # Native implementation
//
// get-repo-map and search-symbols use pure-Go regex extractors
// (TypeScriptExtractor, GoExtractor) with a JSON-serialised index persisted to
// .donmai/code-index/index.json.  No external binary is required for these
// subcommands.
//
// # index.json schema (Go-authoritative, v2)
//
// The Go engine now OWNS this schema. Byte-compatibility with the legacy
// TypeScript code-intelligence IncrementalIndexer.save() output has been
// DELIBERATELY DROPPED: donmai-libraries is being deprecated, so the schema is
// free to evolve for the Go engine's needs (real import graph, content-based
// dedup) without a cross-repo sync constraint. The persisted shape is:
//
//	{ "version": 2, "files": { "<filePath>": FileIndex }, "rootHash": "<hash>" }
//
// The top-level "version" field is authoritative: on load, a missing or older
// version causes the whole index to be discarded and rebuilt from scratch (see
// IndexSchemaVersion / loadIndex). This is a clean full rebuild, never a
// half-migration. FileIndex.gitHash uses git-blob SHA1
// (sha1("blob <size>\0<content>")) as the change-detection / Merkle key;
// contentHash + simHash are content-identity fields for real dedup;
// imports/exports feed the PageRank import graph.
//
// # Exec-shim fallback
//
// For subcommands not yet natively ported (search-code, check-duplicate,
// find-type-usages, validate-cross-deps) the Runner falls back to the exec-shim
// path when DONMAI_CODE_BIN is set:
//
//  1. DONMAI_CODE_BIN env var (legacy: AGENTFACTORY_CODE_BIN) - explicit override
//  2. `donmai-code` on PATH
//  3. `pnpm donmai-code` (monorepo dev)
//
// If none resolve, the command returns ErrNotAvailable.
package codeintel

// SymbolKind enumerates the kinds of code symbols the extractors recognise.
// The Go engine is authoritative for these string values; they are serialised
// into index.json verbatim. (They historically matched the TS SymbolKindSchema,
// but with donmai-libraries deprecating there is no longer a TS side to keep in
// lockstep — change them freely as the Go engine requires.)
type SymbolKind string

// Symbol kind constants — Go-authoritative string values serialised into
// index.json. No cross-repo sync constraint (donmai-libraries is deprecating).
const (
	KindFunction  SymbolKind = "function"
	KindClass     SymbolKind = "class"
	KindInterface SymbolKind = "interface"
	KindType      SymbolKind = "type"
	KindVariable  SymbolKind = "variable"
	KindMethod    SymbolKind = "method"
	KindProperty  SymbolKind = "property"
	KindImport    SymbolKind = "import"
	KindExport    SymbolKind = "export"
	KindEnum      SymbolKind = "enum"
	KindStruct    SymbolKind = "struct"
	KindTrait     SymbolKind = "trait"
	KindImpl      SymbolKind = "impl"
	KindMacro     SymbolKind = "macro"
	KindDecorator SymbolKind = "decorator"
	KindModule    SymbolKind = "module"
)

// CodeSymbol mirrors the legacy TS CodeSymbol type from types.ts.
// JSON field names must match the TS serialisation exactly for index.json
// round-trip compatibility.
type CodeSymbol struct {
	Name          string     `json:"name"`
	Kind          SymbolKind `json:"kind"`
	FilePath      string     `json:"filePath"`
	Line          int        `json:"line"`
	EndLine       *int       `json:"endLine,omitempty"`
	Signature     string     `json:"signature,omitempty"`
	Documentation string     `json:"documentation,omitempty"`
	Exported      bool       `json:"exported"`
	ParentName    string     `json:"parentName,omitempty"`
	Language      string     `json:"language,omitempty"`
}

// FileAST holds the extraction result for a single file. It is an in-memory
// intermediate and is NOT persisted directly; see FileIndex for what goes to
// disk.
type FileAST struct {
	FilePath string
	Language string
	Symbols  []CodeSymbol
	Imports  []string
	Exports  []string
}

// FileIndex is the per-file node persisted to index.json (schema v2,
// Go-authoritative).
//
//   - GitHash is the git-blob SHA1 of raw file content — the change-detection
//     and Merkle-tree key (cheap to recompute; drives incremental re-extraction).
//   - ContentHash is the xxHash64 of the file's normalised content — the exact
//     content-identity key used by dedup (compared against a query's normalised
//     content hash). Distinct from GitHash: git-compat vs dedup-normalised.
//   - SimHash is the 64-bit Charikar fingerprint of the file's normalised
//     content — the near-duplicate key used by dedup.
//   - Imports/Exports are the module specifiers / exported names extracted from
//     the file; Imports feed the PageRank import graph (import_graph.go).
//
// ContentHash/SimHash/Imports/Exports are computed only when a file is actually
// (re)extracted, so they cost nothing on the incremental hash-match fast path.
type FileIndex struct {
	FilePath    string       `json:"filePath"`
	GitHash     string       `json:"gitHash"`
	ContentHash string       `json:"contentHash,omitempty"`
	SimHash     uint64       `json:"simHash,omitempty"`
	Symbols     []CodeSymbol `json:"symbols"`
	Imports     []string     `json:"imports,omitempty"`
	Exports     []string     `json:"exports,omitempty"`
	LastIndexed int64        `json:"lastIndexed"` // Unix ms
}

// IndexMetadata is the top-level metadata block persisted to index.json.
// Schema matches the TS IndexMetadataSchema in types.ts.
type IndexMetadata struct {
	Version      int      `json:"version"`
	RootHash     string   `json:"rootHash"`
	TotalFiles   int      `json:"totalFiles"`
	TotalSymbols int      `json:"totalSymbols"`
	LastUpdated  int64    `json:"lastUpdated"` // Unix ms
	Languages    []string `json:"languages"`
}

// IndexFile is the top-level structure of .donmai/code-index/index.json
// (schema v2, Go-authoritative):
//
//	{ "version": 2, "files": { "<filePath>": FileIndex, ... }, "rootHash": "<hash>" }
//
// Version is the authoritative schema tag. A persisted index whose Version does
// not equal IndexSchemaVersion is discarded on load and rebuilt from scratch.
type IndexFile struct {
	Version  int                  `json:"version"`
	Files    map[string]FileIndex `json:"files"`
	RootHash string               `json:"rootHash"`
}

// RepoMapSymbol is the trimmed symbol shape inside a RepoMapEntry.
type RepoMapSymbol struct {
	Name string     `json:"name"`
	Kind SymbolKind `json:"kind"`
	Line int        `json:"line"`
}

// RepoMapEntry mirrors the TS RepoMapEntrySchema.
type RepoMapEntry struct {
	FilePath string          `json:"filePath"`
	Rank     float64         `json:"rank"`
	Symbols  []RepoMapSymbol `json:"symbols"`
}
