package afcli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient/codeintel"
)

// newCodeCmd constructs the `af code` command tree.
//
// Architecture: shell-out bridge to `pnpm af-code` (TS implementation).
// See afclient/codeintel/runner.go for the full rationale.
func newCodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "code",
		Short: "Code intelligence — repo maps, symbol search, BM25, dedup, type usages, cross-dep validation",
		Long: `Code intelligence commands powered by @renseiai/agentfactory-code-intelligence.

All commands output JSON to stdout. The first invocation builds the index
(~5-10s); subsequent calls reuse the persisted index from .agentfactory/code-index/.

Optional env vars for enhanced search:
  VOYAGE_AI_API_KEY   Enables semantic vector embeddings (hybrid BM25+vector mode)
  COHERE_API_KEY      Enables cross-encoder reranking for more precise result ordering

Binary resolution (in order):
  1. AGENTFACTORY_CODE_BIN env var (explicit override)
  2. af-code on PATH (npm install -g @renseiai/agentfactory-cli)
  3. pnpm af-code (monorepo dev)`,
		SilenceUsage: true,
	}

	cmd.AddCommand(newCodeGetRepoMapCmd())
	cmd.AddCommand(newCodeSearchSymbolsCmd())
	cmd.AddCommand(newCodeSearchCodeCmd())
	cmd.AddCommand(newCodeCheckDuplicateCmd())
	cmd.AddCommand(newCodeFindTypeUsagesCmd())
	cmd.AddCommand(newCodeValidateCrossDepsCmd())

	return cmd
}

// printJSON marshals v as indented JSON to stdout.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// newCodeGetRepoMapCmd constructs `af code get-repo-map`.
func newCodeGetRepoMapCmd() *cobra.Command {
	var (
		maxFiles     int
		filePatterns string
	)

	cmd := &cobra.Command{
		Use:   "get-repo-map",
		Short: "Get a PageRank-ranked repository map showing the most important files",
		Long: `Generates a PageRank-ranked map of repository files and their key symbols.

Files are ranked by their importance in the dependency graph. The output JSON
contains both structured entries and a formatted string suitable for agent context.

Examples:
  af code get-repo-map
  af code get-repo-map --max-files 20
  af code get-repo-map --file-patterns "*.go,src/**"`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			r := codeintel.New(cwd())
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

			opts := codeintel.GetRepoMapOptions{MaxFiles: maxFiles}
			if filePatterns != "" {
				for _, p := range strings.Split(filePatterns, ",") {
					if s := strings.TrimSpace(p); s != "" {
						opts.FilePatterns = append(opts.FilePatterns, s)
					}
				}
			}

			out, err := r.GetRepoMap(opts)
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

// newCodeSearchSymbolsCmd constructs `af code search-symbols <query>`.
func newCodeSearchSymbolsCmd() *cobra.Command {
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
  af code search-symbols "SearchEngine"
  af code search-symbols "handleRequest" --kinds "function,method" --file-pattern "*.go"
  af code search-symbols "Agent" --max-results 5`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())
			if !r.IsCodeAvailable() {
				return fmt.Errorf("%w", codeintel.ErrNotAvailable)
			}

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

			out, err := r.SearchSymbols(opts)
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

// newCodeSearchCodeCmd constructs `af code search-code <query>`.
func newCodeSearchCodeCmd() *cobra.Command {
	var (
		maxResults int
		language   string
	)

	cmd := &cobra.Command{
		Use:   "search-code <query>",
		Short: "BM25 keyword search with code-aware tokenization",
		Long: `Hybrid BM25 + optional semantic search over code content.

When VOYAGE_AI_API_KEY is set, the search automatically upgrades to hybrid
BM25+vector mode. When COHERE_API_KEY is additionally set, results are
reranked with a cross-encoder for improved precision.

Examples:
  af code search-code "incremental indexer"
  af code search-code "pagerank algorithm" --language typescript
  af code search-code "error handling" --max-results 5`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())
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

// newCodeCheckDuplicateCmd constructs `af code check-duplicate`.
func newCodeCheckDuplicateCmd() *cobra.Command {
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
  af code check-duplicate --content "function hello() { return 'world' }"
  af code check-duplicate --content-file /tmp/snippet.go`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			r := codeintel.New(cwd())
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

// newCodeFindTypeUsagesCmd constructs `af code find-type-usages <TypeName>`.
func newCodeFindTypeUsagesCmd() *cobra.Command {
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
  af code find-type-usages "AgentWorkType"
  af code find-type-usages "SandboxProvider" --max-results 100`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())
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

// newCodeValidateCrossDepsCmd constructs `af code validate-cross-deps [path]`.
func newCodeValidateCrossDepsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate-cross-deps [path]",
		Short: "Check that cross-package imports have package.json dependency declarations",
		Long: `Validates that all cross-package imports in the monorepo have corresponding
entries in the importing package's package.json (dependencies, devDependencies,
or peerDependencies). Missing entries would cause CI typecheck failures.

An optional path argument scopes the check to a specific package or file.

Examples:
  af code validate-cross-deps
  af code validate-cross-deps packages/linear`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())
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
