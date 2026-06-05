// Package codeintel provides code intelligence for the donmai CLI.
//
// # Native implementation (S0–S3)
//
// All six code-intel subcommands are now implemented natively in Go:
//   - get-repo-map (S0/S1): TypeScript/JS + Go + Python + Rust regex extractors
//   - search-symbols (S1): BM25-style symbol name search
//   - search-code (S2): Okapi BM25 full-text search over symbol corpus
//   - check-duplicate (S3): xxHash64 exact + SimHash near-dup detection
//   - find-type-usages (S3): regex scan for type reference sites
//   - validate-cross-deps (S3): cross-package import validator
//
// The native path is the PRIMARY implementation for all subcommands.
// The exec-shim (DONMAI_CODE_BIN env override) is kept as a FALLBACK for
// operators who need to force the TypeScript implementation.
//
// # Exec-shim fallback
//
// When DONMAI_CODE_BIN is set the shim path is used:
//
//  1. DONMAI_CODE_BIN env var (legacy: AGENTFACTORY_CODE_BIN) — explicit override
//  2. `donmai-code` on PATH (installed via `npm install -g @donmai/cli`)
//  3. `pnpm donmai-code` (monorepo dev)
//
// If none resolve, the command returns ErrNotAvailable with clear installation
// instructions. Operators can set DONMAI_CODE_BIN to any binary/script to
// override the native implementation as well.
//
// # Index format compatibility
//
// The persisted .donmai/code-index/index.json schema is byte-compatible with
// the TypeScript @donmai/code-intelligence IncrementalIndexer.save() output:
//
//	{ "files": { "<filePath>": FileIndex }, "rootHash": "<hash>" }
//
// where FileIndex matches the TS FileIndexSchema (types.ts). The content hash
// (gitHash field) uses git-blob SHA1, and the S3-scope xxHash64 dedup field
// uses github.com/cespare/xxhash/v2 (seed=0) which produces identical output
// to the TS xxhash-wasm h64ToString() call.
package codeintel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RenseiAI/donmai/internal/envcompat"
)

// ErrNotAvailable is returned when the donmai-code binary cannot be found.
// Callers should surface this with instructions rather than treating it as a
// fatal error.
var ErrNotAvailable = errors.New(
	"donmai-code binary not found — install @donmai/cli globally " +
		"(`npm install -g @donmai/cli`) or set DONMAI_CODE_BIN",
)

// ErrArchNotAvailable is returned when the donmai-arch binary cannot be found.
var ErrArchNotAvailable = errors.New(
	"donmai-arch binary not found — install @donmai/cli globally " +
		"(`npm install -g @donmai/cli`) or set DONMAI_ARCH_BIN",
)

// Runner wraps the donmai-code / donmai-arch CLI binaries and exposes each command as
// a typed Go function. All public methods return raw JSON-decoded output as
// map[string]any or []any, matching the TS CLI's JSON-to-stdout contract.
type Runner struct {
	cwd          string
	codeBinCache string // lazily resolved, cached after first successful lookup
	archBinCache string
}

// New creates a Runner that invokes commands relative to cwd.
// cwd should be the repository root (the directory where .donmai/
// resides or will reside).
func New(cwd string) *Runner {
	return &Runner{cwd: cwd}
}

// resolveCodeBin finds the donmai-code binary using the priority chain described
// in the package doc.
func (r *Runner) resolveCodeBin() ([]string, error) {
	if r.codeBinCache != "" {
		return strings.Fields(r.codeBinCache), nil
	}

	// 1. Explicit env override (DONMAI_CODE_BIN; legacy AGENTFACTORY_CODE_BIN via shim).
	if v := envcompat.GetCodeBin(); v != "" {
		r.codeBinCache = v
		return strings.Fields(v), nil
	}

	// 2. donmai-code on PATH.
	if p, err := exec.LookPath("donmai-code"); err == nil {
		r.codeBinCache = p
		return []string{p}, nil
	}

	// 3. pnpm donmai-code (monorepo).
	if p, err := exec.LookPath("pnpm"); err == nil {
		r.codeBinCache = p + " donmai-code"
		return []string{p, "donmai-code"}, nil
	}

	return nil, ErrNotAvailable
}

// resolveArchBin finds the donmai-arch binary.
func (r *Runner) resolveArchBin() ([]string, error) {
	if r.archBinCache != "" {
		return strings.Fields(r.archBinCache), nil
	}

	// 1. Explicit env override (DONMAI_ARCH_BIN; legacy AGENTFACTORY_ARCH_BIN via shim).
	if v := envcompat.GetArchBin(); v != "" {
		r.archBinCache = v
		return strings.Fields(v), nil
	}

	// 2. donmai-arch on PATH.
	if p, err := exec.LookPath("donmai-arch"); err == nil {
		r.archBinCache = p
		return []string{p}, nil
	}

	// 3. pnpm donmai-arch (monorepo).
	if p, err := exec.LookPath("pnpm"); err == nil {
		r.archBinCache = p + " donmai-arch"
		return []string{p, "donmai-arch"}, nil
	}

	return nil, ErrArchNotAvailable
}

// runCode executes donmai-code <args...> in r.cwd and JSON-decodes stdout.
func (r *Runner) runCode(args ...string) (any, error) {
	binArgs, err := r.resolveCodeBin()
	if err != nil {
		return nil, err
	}
	return r.run(binArgs, args)
}

// run builds the full argv, executes the process, and decodes stdout as JSON.
func (r *Runner) run(bin []string, extraArgs []string) (any, error) {
	bin = append(bin, extraArgs...)
	argv := bin
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec
	cmd.Dir = r.cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("donmai-code %s: %w\nstderr: %s", strings.Join(extraArgs, " "), err, stderr.String())
	}

	var result any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("donmai-code %s: invalid JSON output: %w\nstdout: %s", strings.Join(extraArgs, " "), err, stdout.String())
	}
	return result, nil
}

// ── af code commands ──────────────────────────────────────────────────────────

// GetRepoMapOptions holds the flags for get-repo-map.
type GetRepoMapOptions struct {
	MaxFiles     int
	FilePatterns []string
}

// GetRepoMap builds a PageRank-ranked repository map.
//
// When DONMAI_CODE_BIN is set the exec-shim is used (operator override for
// testing or forcing the TypeScript path). Otherwise the native Go
// implementation is used — no external binary required.
func (r *Runner) GetRepoMap(opts GetRepoMapOptions) (any, error) {
	// Exec-shim override: DONMAI_CODE_BIN is explicitly set.
	if r.isExecOverride() {
		args := []string{"get-repo-map"}
		if opts.MaxFiles > 0 {
			args = append(args, "--max-files", fmt.Sprintf("%d", opts.MaxFiles))
		}
		if len(opts.FilePatterns) > 0 {
			args = append(args, "--file-patterns", strings.Join(opts.FilePatterns, ","))
		}
		return r.runCode(args...)
	}
	// Native primary path.
	return NewNativeRunner(r.cwd).GetRepoMapNative(opts)
}

// SearchSymbolsOptions holds the flags for search-symbols.
type SearchSymbolsOptions struct {
	Query       string
	MaxResults  int
	Kinds       []string
	FilePattern string
}

// SearchSymbols searches the code index for symbols matching the query.
//
// When DONMAI_CODE_BIN is set the exec-shim is used. Otherwise the native Go
// implementation is used.
func (r *Runner) SearchSymbols(opts SearchSymbolsOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required for search-symbols")
	}
	// Exec-shim override.
	if r.isExecOverride() {
		args := []string{"search-symbols", opts.Query}
		if opts.MaxResults > 0 {
			args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
		}
		if len(opts.Kinds) > 0 {
			args = append(args, "--kinds", strings.Join(opts.Kinds, ","))
		}
		if opts.FilePattern != "" {
			args = append(args, "--file-pattern", opts.FilePattern)
		}
		return r.runCode(args...)
	}
	// Native primary path.
	return NewNativeRunner(r.cwd).SearchSymbolsNative(opts)
}

// SearchCodeOptions holds the flags for search-code.
type SearchCodeOptions struct {
	Query      string
	MaxResults int
	Language   string
}

// SearchCode performs BM25 full-text code search.
//
// When DONMAI_CODE_BIN is set the exec-shim is used. Otherwise the native Go
// BM25 implementation (S2) is used — no external binary required.
func (r *Runner) SearchCode(opts SearchCodeOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required for search-code")
	}
	// Exec-shim override.
	if r.isExecOverride() {
		args := []string{"search-code", opts.Query}
		if opts.MaxResults > 0 {
			args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
		}
		if opts.Language != "" {
			args = append(args, "--language", opts.Language)
		}
		return r.runCode(args...)
	}
	// Native primary path (S2).
	return NewNativeRunner(r.cwd).SearchCodeNative(opts)
}

// CheckDuplicateOptions holds the flags for check-duplicate.
type CheckDuplicateOptions struct {
	Content     string
	ContentFile string
}

// CheckDuplicate detects exact and near-duplicate content against the index.
//
// When DONMAI_CODE_BIN is set the exec-shim is used. Otherwise the native
// xxHash64 + SimHash implementation (S3) is used.
func (r *Runner) CheckDuplicate(opts CheckDuplicateOptions) (any, error) {
	if opts.Content == "" && opts.ContentFile == "" {
		return nil, fmt.Errorf("either --content or --content-file is required for check-duplicate")
	}
	// Exec-shim override.
	if r.isExecOverride() {
		args := []string{"check-duplicate"}
		if opts.ContentFile != "" {
			args = append(args, "--content-file", opts.ContentFile)
		} else {
			args = append(args, "--content", opts.Content)
		}
		return r.runCode(args...)
	}
	// Native primary path (S3).
	return NewNativeRunner(r.cwd).CheckDuplicateNative(opts)
}

// FindTypeUsagesOptions holds the flags for find-type-usages.
type FindTypeUsagesOptions struct {
	TypeName   string
	MaxResults int
}

// FindTypeUsages finds all switch/case, mapping objects, and usage sites for a type.
//
// When DONMAI_CODE_BIN is set the exec-shim is used. Otherwise the native
// regex-scan implementation (S3) is used.
func (r *Runner) FindTypeUsages(opts FindTypeUsagesOptions) (any, error) {
	if opts.TypeName == "" {
		return nil, fmt.Errorf("type name is required for find-type-usages")
	}
	// Exec-shim override.
	if r.isExecOverride() {
		args := []string{"find-type-usages", opts.TypeName}
		if opts.MaxResults > 0 {
			args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
		}
		return r.runCode(args...)
	}
	// Native primary path (S3).
	return NewNativeRunner(r.cwd).FindTypeUsagesNative(opts)
}

// ValidateCrossDepsOptions holds the flags for validate-cross-deps.
type ValidateCrossDepsOptions struct {
	Path string // Optional scoping path
}

// ValidateCrossDeps checks cross-package imports have package.json declarations.
//
// When DONMAI_CODE_BIN is set the exec-shim is used. Otherwise the native
// implementation (S3) is used.
func (r *Runner) ValidateCrossDeps(opts ValidateCrossDepsOptions) (any, error) {
	// Exec-shim override.
	if r.isExecOverride() {
		args := []string{"validate-cross-deps"}
		if opts.Path != "" {
			args = append(args, opts.Path)
		}
		return r.runCode(args...)
	}
	// Native primary path (S3).
	return NewNativeRunner(r.cwd).ValidateCrossDepsNative(opts)
}

// ── af arch commands ──────────────────────────────────────────────────────────

// ArchAssessOptions holds the flags for donmai-arch assess.
type ArchAssessOptions struct {
	// PrURL is the full GitHub PR URL (e.g. https://github.com/org/repo/pull/123).
	// Takes precedence over Repository+PrNumber when both are provided.
	PrURL string

	// Repository is the repo identifier (e.g. github.com/org/repo).
	Repository string

	// PrNumber is the PR number within the repository.
	PrNumber int

	// GatePolicy overrides DONMAI_DRIFT_GATE: none | no-severity-high | zero-deviations | max:N
	GatePolicy string

	// ScopeLevel is the scope level for the baseline query.
	// Valid values: project | org | tenant | global
	ScopeLevel string

	// ProjectID is the project ID for scope.
	ProjectID string

	// DB is the SQLite DB path (overrides DONMAI_ARCH_DB).
	DB string

	// Summary outputs human-readable text instead of JSON.
	Summary bool
}

// ArchAssess runs `donmai-arch assess`.
// Exit code 1 from the subprocess means the gate was triggered — this is
// mapped to an ErrGateTriggered sentinel rather than a generic error so callers
// can handle it without parsing stderr.
func (r *Runner) ArchAssess(opts ArchAssessOptions) (any, error) {
	args := []string{"assess"}

	if opts.PrURL != "" {
		args = append(args, opts.PrURL)
	}
	if opts.Repository != "" {
		args = append(args, "--repository", opts.Repository)
	}
	if opts.PrNumber > 0 {
		args = append(args, "--pr", fmt.Sprintf("%d", opts.PrNumber))
	}
	if opts.GatePolicy != "" {
		args = append(args, "--gate-policy", opts.GatePolicy)
	}
	if opts.ScopeLevel != "" {
		args = append(args, "--scope-level", opts.ScopeLevel)
	}
	if opts.ProjectID != "" {
		args = append(args, "--project-id", opts.ProjectID)
	}
	if opts.DB != "" {
		args = append(args, "--db", opts.DB)
	}
	if opts.Summary {
		args = append(args, "--summary")
	}

	binArgs, err := r.resolveArchBin()
	if err != nil {
		return nil, err
	}

	binArgs = append(binArgs, args...)
	cmd := exec.Command(binArgs[0], binArgs[1:]...) //nolint:gosec
	cmd.Dir = r.cwd

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// Exit code 1 → gate triggered; still decode stdout.
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		return nil, fmt.Errorf("donmai-arch assess: %w\nstderr: %s", runErr, stderr.String())
	}

	if opts.Summary {
		// In summary mode, stdout is human-readable text, not JSON.
		return map[string]any{
			"gated":       exitCode == 1,
			"summaryText": stdout.String(),
		}, nil
	}

	var result any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("donmai-arch assess: invalid JSON output: %w\nstdout: %s", err, stdout.String())
	}

	// Inject gated flag if exit code indicates it.
	if exitCode == 1 {
		if m, ok := result.(map[string]any); ok {
			m["gated"] = true
		}
	}

	return result, nil
}

// IsCodeAvailable returns true. The native implementation for get-repo-map and
// search-symbols is always available (no external binary required). For the
// remaining exec-shim commands (search-code, check-duplicate, find-type-usages,
// validate-cross-deps) callers should handle ErrNotAvailable gracefully.
func (r *Runner) IsCodeAvailable() bool {
	return true
}

// isExecOverride returns true when DONMAI_CODE_BIN (or legacy
// AGENTFACTORY_CODE_BIN) is explicitly set, signalling that the operator
// wants the exec-shim path rather than the native implementation.
func (r *Runner) isExecOverride() bool {
	return envcompat.GetCodeBin() != ""
}

// IsArchAvailable returns true if the donmai-arch binary can be found.
func (r *Runner) IsArchAvailable() bool {
	_, err := r.resolveArchBin()
	return err == nil
}
