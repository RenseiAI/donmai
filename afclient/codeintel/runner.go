// Package codeintel provides a shell-out bridge to the TypeScript
// @renseiai/agentfactory-code-intelligence CLI (pnpm af-code).
//
// # Architectural choice: shell-out bridge (Phase D parity)
//
// The tree-sitter Go bindings (go-tree-sitter) were evaluated but rejected for
// this phase because:
//
//  1. CGo + native deps make CI slower and cross-compilation fragile.
//  2. The AC requires byte-identical index format with TS readers — easiest to
//     guarantee when TS owns the indexing entirely.
//  3. Phase D goal is parity, not re-implementation.
//
// This package shells out to `pnpm af-code` (resolving via PATH or
// AGENTFACTORY_CODE_BIN env var) and returns the parsed JSON output.
//
// A future issue (post-Wave 4) can replace the shell-out with native Go
// tree-sitter after parity is verified end-to-end.
//
// # Binary resolution (PATH portability)
//
// The binary is resolved in this order:
//  1. AGENTFACTORY_CODE_BIN env var (explicit override for non-monorepo users)
//  2. `af-code` on PATH (installed via `npm install -g @renseiai/agentfactory-cli`)
//  3. `pnpm af-code` via pnpm run in the current working directory (monorepo dev)
//
// If none of those resolve, every command returns an ErrNotAvailable error with
// clear installation instructions. The caller surfaces this gracefully rather
// than crashing.
package codeintel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrNotAvailable is returned when the af-code binary cannot be found.
// Callers should surface this with instructions rather than treating it as a
// fatal error.
var ErrNotAvailable = errors.New(
	"af-code binary not found — install @renseiai/agentfactory-cli globally " +
		"(`npm install -g @renseiai/agentfactory-cli`) or set AGENTFACTORY_CODE_BIN",
)

// ErrArchNotAvailable is returned when the af-arch binary cannot be found.
var ErrArchNotAvailable = errors.New(
	"af-arch binary not found — install @renseiai/agentfactory-cli globally " +
		"(`npm install -g @renseiai/agentfactory-cli`) or set AGENTFACTORY_ARCH_BIN",
)

// Runner wraps the af-code / af-arch CLI binaries and exposes each command as
// a typed Go function. All public methods return raw JSON-decoded output as
// map[string]any or []any, matching the TS CLI's JSON-to-stdout contract.
type Runner struct {
	cwd          string
	codeBinCache string // lazily resolved, cached after first successful lookup
	archBinCache string
}

// New creates a Runner that invokes commands relative to cwd.
// cwd should be the repository root (the directory where .agentfactory/
// resides or will reside).
func New(cwd string) *Runner {
	return &Runner{cwd: cwd}
}

// resolveCodeBin finds the af-code binary using the priority chain described
// in the package doc.
func (r *Runner) resolveCodeBin() ([]string, error) {
	if r.codeBinCache != "" {
		return strings.Fields(r.codeBinCache), nil
	}

	// 1. Explicit env override.
	if v := os.Getenv("AGENTFACTORY_CODE_BIN"); v != "" {
		r.codeBinCache = v
		return strings.Fields(v), nil
	}

	// 2. af-code on PATH.
	if p, err := exec.LookPath("af-code"); err == nil {
		r.codeBinCache = p
		return []string{p}, nil
	}

	// 3. pnpm af-code (monorepo).
	if p, err := exec.LookPath("pnpm"); err == nil {
		r.codeBinCache = p + " af-code"
		return []string{p, "af-code"}, nil
	}

	return nil, ErrNotAvailable
}

// resolveArchBin finds the af-arch binary.
func (r *Runner) resolveArchBin() ([]string, error) {
	if r.archBinCache != "" {
		return strings.Fields(r.archBinCache), nil
	}

	// 1. Explicit env override.
	if v := os.Getenv("AGENTFACTORY_ARCH_BIN"); v != "" {
		r.archBinCache = v
		return strings.Fields(v), nil
	}

	// 2. af-arch on PATH.
	if p, err := exec.LookPath("af-arch"); err == nil {
		r.archBinCache = p
		return []string{p}, nil
	}

	// 3. pnpm af-arch (monorepo).
	if p, err := exec.LookPath("pnpm"); err == nil {
		r.archBinCache = p + " af-arch"
		return []string{p, "af-arch"}, nil
	}

	return nil, ErrArchNotAvailable
}

// runCode executes af-code <args...> in r.cwd and JSON-decodes stdout.
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
		return nil, fmt.Errorf("af-code %s: %w\nstderr: %s", strings.Join(extraArgs, " "), err, stderr.String())
	}

	var result any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("af-code %s: invalid JSON output: %w\nstdout: %s", strings.Join(extraArgs, " "), err, stdout.String())
	}
	return result, nil
}

// ── af code commands ──────────────────────────────────────────────────────────

// GetRepoMapOptions holds the flags for get-repo-map.
type GetRepoMapOptions struct {
	MaxFiles     int
	FilePatterns []string
}

// GetRepoMap runs `af-code get-repo-map`.
func (r *Runner) GetRepoMap(opts GetRepoMapOptions) (any, error) {
	args := []string{"get-repo-map"}
	if opts.MaxFiles > 0 {
		args = append(args, "--max-files", fmt.Sprintf("%d", opts.MaxFiles))
	}
	if len(opts.FilePatterns) > 0 {
		args = append(args, "--file-patterns", strings.Join(opts.FilePatterns, ","))
	}
	return r.runCode(args...)
}

// SearchSymbolsOptions holds the flags for search-symbols.
type SearchSymbolsOptions struct {
	Query       string
	MaxResults  int
	Kinds       []string
	FilePattern string
}

// SearchSymbols runs `af-code search-symbols <query>`.
func (r *Runner) SearchSymbols(opts SearchSymbolsOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required for search-symbols")
	}
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

// SearchCodeOptions holds the flags for search-code.
type SearchCodeOptions struct {
	Query      string
	MaxResults int
	Language   string
}

// SearchCode runs `af-code search-code <query>`.
func (r *Runner) SearchCode(opts SearchCodeOptions) (any, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("query is required for search-code")
	}
	args := []string{"search-code", opts.Query}
	if opts.MaxResults > 0 {
		args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
	}
	if opts.Language != "" {
		args = append(args, "--language", opts.Language)
	}
	return r.runCode(args...)
}

// CheckDuplicateOptions holds the flags for check-duplicate.
type CheckDuplicateOptions struct {
	Content     string
	ContentFile string
}

// CheckDuplicate runs `af-code check-duplicate`.
func (r *Runner) CheckDuplicate(opts CheckDuplicateOptions) (any, error) {
	if opts.Content == "" && opts.ContentFile == "" {
		return nil, fmt.Errorf("either --content or --content-file is required for check-duplicate")
	}
	args := []string{"check-duplicate"}
	if opts.ContentFile != "" {
		args = append(args, "--content-file", opts.ContentFile)
	} else {
		args = append(args, "--content", opts.Content)
	}
	return r.runCode(args...)
}

// FindTypeUsagesOptions holds the flags for find-type-usages.
type FindTypeUsagesOptions struct {
	TypeName   string
	MaxResults int
}

// FindTypeUsages runs `af-code find-type-usages <TypeName>`.
func (r *Runner) FindTypeUsages(opts FindTypeUsagesOptions) (any, error) {
	if opts.TypeName == "" {
		return nil, fmt.Errorf("type name is required for find-type-usages")
	}
	args := []string{"find-type-usages", opts.TypeName}
	if opts.MaxResults > 0 {
		args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
	}
	return r.runCode(args...)
}

// ValidateCrossDepsOptions holds the flags for validate-cross-deps.
type ValidateCrossDepsOptions struct {
	Path string // Optional scoping path
}

// ValidateCrossDeps runs `af-code validate-cross-deps [path]`.
func (r *Runner) ValidateCrossDeps(opts ValidateCrossDepsOptions) (any, error) {
	args := []string{"validate-cross-deps"}
	if opts.Path != "" {
		args = append(args, opts.Path)
	}
	return r.runCode(args...)
}

// ── af arch commands ──────────────────────────────────────────────────────────

// ArchAssessOptions holds the flags for af-arch assess.
type ArchAssessOptions struct {
	// PrURL is the full GitHub PR URL (e.g. https://github.com/org/repo/pull/123).
	// Takes precedence over Repository+PrNumber when both are provided.
	PrURL string

	// Repository is the repo identifier (e.g. github.com/org/repo).
	Repository string

	// PrNumber is the PR number within the repository.
	PrNumber int

	// GatePolicy overrides RENSEI_DRIFT_GATE: none | no-severity-high | zero-deviations | max:N
	GatePolicy string

	// ScopeLevel is the scope level for the baseline query.
	// Valid values: project | org | tenant | global
	ScopeLevel string

	// ProjectID is the project ID for scope.
	ProjectID string

	// DB is the SQLite DB path (overrides RENSEI_ARCH_DB).
	DB string

	// Summary outputs human-readable text instead of JSON.
	Summary bool
}

// ArchAssess runs `af-arch assess`.
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
		return nil, fmt.Errorf("af-arch assess: %w\nstderr: %s", runErr, stderr.String())
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
		return nil, fmt.Errorf("af-arch assess: invalid JSON output: %w\nstdout: %s", err, stdout.String())
	}

	// Inject gated flag if exit code indicates it.
	if exitCode == 1 {
		if m, ok := result.(map[string]any); ok {
			m["gated"] = true
		}
	}

	return result, nil
}

// IsCodeAvailable returns true if the af-code binary can be found.
func (r *Runner) IsCodeAvailable() bool {
	_, err := r.resolveCodeBin()
	return err == nil
}

// IsArchAvailable returns true if the af-arch binary can be found.
func (r *Runner) IsArchAvailable() bool {
	_, err := r.resolveArchBin()
	return err == nil
}
