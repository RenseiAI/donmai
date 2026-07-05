package afcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

// newCodeCmd constructs the `donmai code` command tree.
//
// Architecture: all six subcommands are implemented natively in Go
// (afclient/codeintel/native.go) — no external binary is required. Setting
// DONMAI_CODE_BIN (or legacy AGENTFACTORY_CODE_BIN) routes all subcommands
// through the deprecated TypeScript exec-shim instead; see
// afclient/codeintel/runner.go for the resolution order and rationale.
func newCodeCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	var repoPath string
	cmd := &cobra.Command{
		Use:   "code",
		Short: "Code intelligence: repo maps, symbol search, BM25, dedup, type usages, cross-dep validation",
		Long: `Code intelligence commands for navigating and searching code.

All six subcommands (get-repo-map, search-symbols, search-code, check-duplicate,
find-type-usages, validate-cross-deps) use the native Go implementation by
default; no external binary is required. The first invocation builds the index
(~5-10s for a large repo); subsequent calls reuse the persisted index from
.donmai/code-index/.

By default the index root is the enclosing git repository root (discovered by
walking up from the current directory for a ` + "`.git`" + ` entry — directory or file,
so worktree checkouts resolve correctly), not just the invocation cwd. Use
--repo-path to scope to a subtree within that root (e.g. a single package in a
monorepo). If no git root is found, the current directory is used as a
fallback and a note is printed to stderr.

Override: set DONMAI_CODE_BIN to force the deprecated TypeScript exec-shim
path for ALL subcommands (useful for testing against the legacy reference
implementation; prints a one-time deprecation notice to stderr).

Optional env vars for enhanced search-code results:
  VOYAGE_AI_API_KEY   Enables semantic vector embeddings (hybrid BM25+vector mode)
  COHERE_API_KEY      Enables cross-encoder reranking for more precise result ordering`,
		SilenceUsage: true,
	}

	cmd.PersistentFlags().StringVar(&repoPath, "repo-path", "",
		"Relative path under the git root to scope indexing to (e.g. a monorepo package); must stay inside the root")

	cmd.AddCommand(newCodeGetRepoMapCmd(bin, &repoPath))
	cmd.AddCommand(newCodeSearchSymbolsCmd(bin, &repoPath))
	cmd.AddCommand(newCodeSearchCodeCmd(bin, &repoPath))
	cmd.AddCommand(newCodeCheckDuplicateCmd(bin, &repoPath))
	cmd.AddCommand(newCodeFindTypeUsagesCmd(bin, &repoPath))
	cmd.AddCommand(newCodeValidateCrossDepsCmd(bin, &repoPath))

	return cmd
}

// printJSON marshals v as indented JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// newCodeGetRepoMapCmd constructs `donmai code get-repo-map`.
func newCodeGetRepoMapCmd(bin string, repoPath *string) *cobra.Command {
	var (
		maxFiles     int
		filePatterns string
	)

	cmd := &cobra.Command{
		Use:   "get-repo-map",
		Short: "Get a PageRank-ranked repository map showing the most important files",
		Long: `Generates a PageRank-ranked map of repository files and their key symbols.

Files are ranked by PageRank over the intra-repo import/dependency graph
(resolved for TypeScript/JS relative imports and Go/Python/Rust package
imports). The output JSON has the shape {entries, rootHash, files}, where each
entry carries a filePath, its PageRank rank, and its extracted symbols.

The index is built once and persisted under .donmai/code-index/; later calls
incrementally update only files that changed rather than re-extracting the
whole repo.

Examples:
  ` + bin + ` code get-repo-map
  ` + bin + ` code get-repo-map --max-files 20
  ` + bin + ` code get-repo-map --file-patterns "*.go,src/**"`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			opts := codeintel.GetRepoMapOptions{MaxFiles: maxFiles}
			if filePatterns != "" {
				for _, p := range strings.Split(filePatterns, ",") {
					if s := strings.TrimSpace(p); s != "" {
						opts.FilePatterns = append(opts.FilePatterns, s)
					}
				}
			}
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("get-repo-map: %w", err)
			}
			out, err := codeintel.New(root).GetRepoMap(opts)
			if err != nil {
				return fmt.Errorf("get-repo-map: %w", err)
			}
			return printJSON(out)
		},
	}

	cmd.Flags().IntVar(&maxFiles, "max-files", 0, "Maximum files to include (0 = use default)")
	cmd.Flags().StringVar(&filePatterns, "file-patterns", "", "Comma-separated file pattern filters (e.g. \"*.go,src/**\")")

	return cmd
}

// newCodeSearchSymbolsCmd constructs `donmai code search-symbols <query>`.
func newCodeSearchSymbolsCmd(bin string, repoPath *string) *cobra.Command {
	var (
		maxResults  int
		kinds       string
		filePattern string
	)

	cmd := &cobra.Command{
		Use:   "search-symbols <query>",
		Short: "Search for code symbols (functions, classes, types) by name or query",
		Long: `BM25 search over the symbol index (function, class, interface, type, etc.).

Examples:
  ` + bin + ` code search-symbols "SearchEngine"
  ` + bin + ` code search-symbols "handleRequest" --kinds "function,method" --file-pattern "*.go"
  ` + bin + ` code search-symbols "Agent" --max-results 5`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			opts := codeintel.SearchSymbolsOptions{
				Query:       args[0],
				MaxResults:  maxResults,
				FilePattern: filePattern,
			}
			if kinds != "" {
				for _, k := range strings.Split(kinds, ",") {
					if s := strings.TrimSpace(k); s != "" {
						opts.Kinds = append(opts.Kinds, s)
					}
				}
			}
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("search-symbols: %w", err)
			}
			out, err := codeintel.New(root).SearchSymbols(opts)
			if err != nil {
				return fmt.Errorf("search-symbols: %w", err)
			}
			return printJSON(out)
		},
	}

	cmd.Flags().IntVar(&maxResults, "max-results", 0, "Maximum results (0 = use default of 20)")
	cmd.Flags().StringVar(&kinds, "kinds", "", "Comma-separated symbol kinds: function,class,interface,type,etc.")
	cmd.Flags().StringVar(&filePattern, "file-pattern", "", "Filter by file pattern (e.g. \"*.go\")")

	return cmd
}

// newCodeSearchCodeCmd constructs `donmai code search-code <query>`.
func newCodeSearchCodeCmd(bin string, repoPath *string) *cobra.Command {
	var (
		maxResults int
		language   string
	)

	cmd := &cobra.Command{
		Use:   "search-code <query>",
		Short: "BM25 keyword search with code-aware tokenization",
		Long: `Hybrid BM25 + optional semantic search over code content, natively in Go.

Okapi BM25 keyword search runs by default with no external services. When
VOYAGE_AI_API_KEY is set, the search upgrades to hybrid BM25+vector mode
(falling back to BM25-only if the embedding call fails). When COHERE_API_KEY
is additionally set, results are reranked with a cross-encoder for improved
precision.

Examples:
  ` + bin + ` code search-code "incremental indexer"
  ` + bin + ` code search-code "pagerank algorithm" --language typescript
  ` + bin + ` code search-code "error handling" --max-results 5`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("search-code: %w", err)
			}
			r := codeintel.New(root)
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

			out, err := r.SearchCode(codeintel.SearchCodeOptions{
				Query:      args[0],
				MaxResults: maxResults,
				Language:   language,
			})
			if err != nil {
				return fmt.Errorf("search-code: %w", err)
			}
			return printJSON(out)
		},
	}

	cmd.Flags().IntVar(&maxResults, "max-results", 0, "Maximum results (0 = use default of 20)")
	cmd.Flags().StringVar(&language, "language", "", "Filter by language (e.g. typescript, go, python)")

	return cmd
}

// newCodeCheckDuplicateCmd constructs `donmai code check-duplicate`.
func newCodeCheckDuplicateCmd(bin string, repoPath *string) *cobra.Command {
	var (
		content     string
		contentFile string
	)

	cmd := &cobra.Command{
		Use:   "check-duplicate",
		Short: "Check if content is a duplicate using xxHash64 and SimHash",
		Long: `Checks content for exact duplicates (xxHash64) and near-duplicates (SimHash).

Exactly one of --content or --content-file must be provided.

Examples:
  ` + bin + ` code check-duplicate --content "function hello() { return 'world' }"
  ` + bin + ` code check-duplicate --content-file /tmp/snippet.go`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("check-duplicate: %w", err)
			}
			r := codeintel.New(root)
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

			out, err := r.CheckDuplicate(codeintel.CheckDuplicateOptions{
				Content:     content,
				ContentFile: contentFile,
			})
			if err != nil {
				return fmt.Errorf("check-duplicate: %w", err)
			}
			return printJSON(out)
		},
	}

	cmd.Flags().StringVar(&content, "content", "", "Content to check for duplicates (inline)")
	cmd.Flags().StringVar(&contentFile, "content-file", "", "Path to file containing content to check")
	cmd.MarkFlagsOneRequired("content", "content-file")
	cmd.MarkFlagsMutuallyExclusive("content", "content-file")

	return cmd
}

// newCodeFindTypeUsagesCmd constructs `donmai code find-type-usages <TypeName>`.
func newCodeFindTypeUsagesCmd(bin string, repoPath *string) *cobra.Command {
	var maxResults int

	cmd := &cobra.Command{
		Use:   "find-type-usages <TypeName>",
		Short: "Find all switch/case, mapping objects, and usage sites for a union type or enum",
		Long: `Scans the codebase for all places where a union type or enum is used:
  - switch/case statements discriminating over the type
  - Record<TypeName, ...> and mapping objects
  - Exhaustive check patterns (assertNever, etc.)
  - Import sites and type references

Use this before adding new members to a union type to identify all files
that need to be updated.

Examples:
  ` + bin + ` code find-type-usages "AgentWorkType"
  ` + bin + ` code find-type-usages "SandboxProvider" --max-results 100`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("find-type-usages: %w", err)
			}
			r := codeintel.New(root)
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

			out, err := r.FindTypeUsages(codeintel.FindTypeUsagesOptions{
				TypeName:   args[0],
				MaxResults: maxResults,
			})
			if err != nil {
				return fmt.Errorf("find-type-usages: %w", err)
			}
			return printJSON(out)
		},
	}

	cmd.Flags().IntVar(&maxResults, "max-results", 0, "Maximum results (0 = use default of 50)")

	return cmd
}

// newCodeValidateCrossDepsCmd constructs `donmai code validate-cross-deps [path]`.
func newCodeValidateCrossDepsCmd(bin string, repoPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate-cross-deps [path]",
		Short: "Check that cross-package imports have package.json dependency declarations",
		Long: `Validates that all cross-package imports in the monorepo have corresponding
entries in the importing package's package.json (dependencies, devDependencies,
or peerDependencies). Missing entries would cause CI typecheck failures.

An optional path argument scopes the check to a specific package or file.

Examples:
  ` + bin + ` code validate-cross-deps
  ` + bin + ` code validate-cross-deps packages/linear`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := indexRoot(*repoPath)
			if err != nil {
				return fmt.Errorf("validate-cross-deps: %w", err)
			}
			r := codeintel.New(root)
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

			opts := codeintel.ValidateCrossDepsOptions{}
			if len(args) == 1 {
				opts.Path = args[0]
			}

			out, err := r.ValidateCrossDeps(opts)
			if err != nil {
				return fmt.Errorf("validate-cross-deps: %w", err)
			}
			return printJSON(out)
		},
	}

	return cmd
}

// cwd returns the current working directory, falling back to "." on error.
func cwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// gitRootWarnOnce guards the one-line stderr note emitted (at most once per
// process) when no enclosing git repository is found and cwd() is used as
// the index root fallback instead.
var gitRootWarnOnce sync.Once

// indexRoot resolves the effective root directory that code-intel commands
// should index: the enclosing git repository root, discovered by walking up
// from the current directory for a `.git` entry (directory or file form —
// worktree checkouts use a `.git` file), optionally narrowed to a subtree via
// repoPath (the --repo-path flag).
//
// When no git root is found, the current directory is used as a fallback and
// a one-line note is printed to stderr (once per process) so operators
// understand indexing may be narrower than the whole repository.
//
// repoPath, when non-empty, must be a path RELATIVE to the discovered root
// (absolute paths are rejected) and must resolve, after filepath.Clean, to a
// location that stays inside that root (rejecting ../ escapes). The resolved
// path must exist and be a directory.
func indexRoot(repoPath string) (string, error) {
	dir := cwd()
	root := dir
	if gitRoot, ok := codeintel.FindGitRoot(dir); ok {
		root = gitRoot
	} else {
		gitRootWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr,
				"note: no enclosing git repository found from %s; indexing is scoped to the current directory only\n", dir)
		})
	}

	if repoPath == "" {
		return root, nil
	}

	if filepath.IsAbs(repoPath) {
		return "", fmt.Errorf("--repo-path must be a relative path under the git root, got absolute path %q", repoPath)
	}

	cleaned := filepath.Clean(filepath.Join(root, repoPath))
	rel, err := filepath.Rel(root, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--repo-path %q escapes the git root %q", repoPath, root)
	}

	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("--repo-path %q: %w", repoPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--repo-path %q is not a directory", repoPath)
	}

	return cleaned, nil
}
