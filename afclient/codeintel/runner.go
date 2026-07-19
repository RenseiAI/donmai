// Package codeintel provides code intelligence for the donmai CLI.
//
// # Native implementation (S0-S3)
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
//  1. DONMAI_CODE_BIN env var (legacy: AGENTFACTORY_CODE_BIN) - explicit override
//  2. `donmai-code` on PATH (set DONMAI_CODE_BIN to use this shim)
//  3. `pnpm donmai-code` (monorepo dev)
//
// If none resolve, the command returns ErrNotAvailable with clear installation
// instructions. Operators can set DONMAI_CODE_BIN to any binary/script to
// override the native implementation as well.
//
// # Index format compatibility
//
// The persisted .donmai/code-index/index.json schema is byte-compatible with
// the legacy TypeScript code-intelligence IncrementalIndexer.save() output:
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/internal/envcompat"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// ErrNotAvailable is returned when the donmai-code binary cannot be found.
// Callers should surface this with instructions rather than treating it as a
// fatal error.
var ErrNotAvailable = errors.New(
	"donmai-code binary not found. Set DONMAI_CODE_BIN to the binary path, " +
		"or install donmai via `brew install RenseiAI/homebrew-tap/donmai` " +
		"or `go install github.com/RenseiAI/donmai/cmd/donmai@latest`",
)

// ErrArchNotAvailable is returned by resolveArchBin when the DEPRECATED TS arch
// shim is not opted into. Note: ArchAssess does NOT surface this sentinel to
// callers - the native Go Layer-1+2 pipeline is the primary path, so this is the
// expected (non-error) state. IsArchBinAvailable uses it to decide whether to
// route through the legacy shim.
var ErrArchNotAvailable = errors.New(
	"DEPRECATED arch shim not configured - the native Go arch-intel pipeline " +
		"(Layer 1+2) is the primary path; set DONMAI_ARCH_BIN only to force the " +
		"legacy TS arch shim",
)

// codeShimWarnOnce guards the one-time deprecation notice emitted when the
// legacy TS code exec-shim is resolved (so a single process emits it at most
// once).
var codeShimWarnOnce sync.Once

// warnCodeShimDeprecated prints a single deprecation notice to stderr
// explaining that the code exec-shim is a legacy, opt-in fallback now that
// the native Go implementation is the primary path for all six code-intel
// subcommands. Mirrors the arch-shim precedent (warnArchShimDeprecated,
// below) — same one-time-per-process guard, same tone.
func warnCodeShimDeprecated(via string) {
	codeShimWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"warning: using the DEPRECATED TS code exec-shim (%s). The native Go "+
				"code-intel implementation (get-repo-map, search-symbols, search-code, "+
				"check-duplicate, find-type-usages, validate-cross-deps) is the primary "+
				"path and requires no external binary; unset DONMAI_CODE_BIN to use it. "+
				"The shim will be removed when donmai-libraries is archived.\n", via)
	})
}

// archShimWarnOnce guards the one-time deprecation notice emitted when the
// legacy TS arch shim is resolved (so a single process emits it at most once).
var archShimWarnOnce sync.Once

// warnArchShimDeprecated prints a single deprecation notice to stderr explaining
// that the exec-shim is a legacy, opt-in fallback now that the native Go
// Layer-1+2 pipeline is the primary path.
func warnArchShimDeprecated(via string) {
	archShimWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"warning: using the DEPRECATED TS arch shim (%s). The native Go "+
				"arch-intel pipeline (Layer 1+2) is the primary path and requires "+
				"no external binary; unset DONMAI_ARCH_BIN to use it. The shim will "+
				"be removed in a future release.\n", via)
	})
}

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
		warnCodeShimDeprecated("DONMAI_CODE_BIN")
		r.codeBinCache = v
		return strings.Fields(v), nil
	}

	// 2. donmai-code on PATH.
	if p, err := exec.LookPath("donmai-code"); err == nil {
		warnCodeShimDeprecated("donmai-code on PATH")
		r.codeBinCache = p
		return []string{p}, nil
	}

	// 3. pnpm donmai-code (monorepo).
	if p, err := exec.LookPath("pnpm"); err == nil {
		warnCodeShimDeprecated("pnpm donmai-code")
		r.codeBinCache = p + " donmai-code"
		return []string{p, "donmai-code"}, nil
	}

	return nil, ErrNotAvailable
}

// resolveArchBin resolves the DEPRECATED, opt-in TS arch shim.
//
// The native Go arch-intel pipeline (Layer 1+2) is the primary path and needs no
// external binary - this resolver only fires when an operator explicitly opts
// into the legacy TS arch shim. It is NOT a "binary not installed" failure path:
// ErrArchNotAvailable just means the shim was not opted into, and ArchAssess
// then runs the native pipeline.
//
// Resolution order (all paths emit a one-time deprecation notice):
//
//  1. DONMAI_ARCH_BIN (or legacy AGENTFACTORY_ARCH_BIN) - explicit shim override.
//     This is the supported way to force the TS path; it may point at any
//     binary/script (e.g. `af-arch`, `pnpm af-arch`, or a wrapper).
//  2. af-arch on PATH (legacy bin name; earlier versions of the shim).
//
// The previous `pnpm donmai-arch` monorepo probe is dropped: there is no
// `donmai-arch` bin, and monorepo dev should set DONMAI_ARCH_BIN="pnpm af-arch".
func (r *Runner) resolveArchBin() ([]string, error) {
	if r.archBinCache != "" {
		return strings.Fields(r.archBinCache), nil
	}

	// 1. Explicit env override (DONMAI_ARCH_BIN; legacy AGENTFACTORY_ARCH_BIN
	//    emits its own rename warning inside envcompat).
	if v := envcompat.GetArchBin(); v != "" {
		warnArchShimDeprecated("DONMAI_ARCH_BIN")
		r.archBinCache = v
		return strings.Fields(v), nil
	}

	// 2. af-arch on PATH (legacy bin name from the deprecated TS shim).
	if p, err := exec.LookPath("af-arch"); err == nil {
		warnArchShimDeprecated("af-arch on PATH")
		r.archBinCache = p
		return []string{p}, nil
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
	cmd.Env = runtimeenv.FilterRunnerOnly(os.Environ())

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
	// IncludeDoc opts in to the full CodeSymbol per hit, including the complete
	// multi-line documentation block. Default (false) returns the compact
	// projection {name, kind, filePath, line, signature} with documentation
	// truncated to its first line.
	IncludeDoc bool
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
	// IncludeDoc opts in to the full CodeSymbol per hit (see
	// SearchSymbolsOptions.IncludeDoc).
	IncludeDoc bool
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
	// MaxResults bounds the duplicate sites returned. 0 (default) means the
	// single top match only; > 1 adds a ranked "matches" list to the result.
	MaxResults int
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
		if opts.MaxResults > 0 {
			args = append(args, "--max-results", fmt.Sprintf("%d", opts.MaxResults))
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

// ArchAssessOptions holds the flags for arch assess.
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

// ArchAssess assesses a PR/commit for architectural drift.
//
// The PRIMARY path is the native Go arch-intel pipeline (Layer 1+2): it fetches
// the real PR diff, runs the lane-backed assess-against-baseline pipeline
// ("mode":"native") when an arch ModelAdapter is wired and a baseline exists, or
// the pure-regex diff/gate fallback ("mode":"native-diff-only") otherwise. No
// external binary is required.
//
// The DEPRECATED exec-shim is an opt-in legacy fallback for byte-identical TS
// output, taken ONLY when resolveArchBin succeeds:
//  1. DONMAI_ARCH_BIN (or legacy AGENTFACTORY_ARCH_BIN) - explicit shim override.
//  2. af-arch on PATH (legacy bin name from the deprecated TS shim).
//
// Both shim paths emit a one-time deprecation notice on stderr.
//
// Exit code 1 from the shim subprocess means the gate was triggered - this is
// surfaced via a "gated":true flag on the result rather than a generic error so
// callers can handle it without parsing stderr.
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

	binArgs, binErr := r.resolveArchBin()
	if binErr != nil {
		// Shim not opted into (the common case) — run the native Go pipeline.
		return r.archAssessNative(opts)
	}

	binArgs = append(binArgs, args...)
	cmd := exec.Command(binArgs[0], binArgs[1:]...) //nolint:gosec
	cmd.Dir = r.cwd
	cmd.Env = runtimeenv.FilterRunnerOnly(os.Environ())

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
		return nil, fmt.Errorf("arch shim assess: %w\nstderr: %s", runErr, stderr.String())
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
		return nil, fmt.Errorf("arch shim assess: invalid JSON output: %w\nstdout: %s", err, stdout.String())
	}

	// Inject gated flag if exit code indicates it.
	if exitCode == 1 {
		if m, ok := result.(map[string]any); ok {
			m["gated"] = true
		}
	}

	return result, nil
}

// archAssessNative is the PRIMARY path: the native Go arch-intel Layer-1
// diff/gate pipeline, taken whenever the deprecated TS shim is not opted into
// (the common case).
//
// It fetches the REAL PR diff via the GitHub CLI and runs the pure-regex
// diff/gate layer ("mode":"native-diff-only") over the actual changed files +
// patches. A missing/failed `gh` degrades to a metadata-only PrDiff rather than
// erroring, so the gate + JSON shape stay valid.
//
// NOTE: the Layer-2 arch-intelligence pipeline (learned baseline + LLM deviation
// detection against the SQLite observation graph) is intentionally NOT part of
// this OSS path — it is platform-owned per ADR-2026-06-07. The
// Go-native Layer-2 substrate is parked unmerged pending a future OSS-standalone
// ADR.
func (r *Runner) archAssessNative(opts ArchAssessOptions) (any, error) {
	ctx := context.Background()

	repo, prNum := opts.Repository, opts.PrNumber

	// Parse PR URL when provided.
	if opts.PrURL != "" {
		if parsed, num, ok := parsePRURL(opts.PrURL); ok {
			if repo == "" {
				repo = parsed
			}
			if prNum == 0 {
				prNum = num
			}
		}
	}

	scopeLevel := opts.ScopeLevel
	if scopeLevel == "" {
		scopeLevel = "project"
	}

	// Fetch the real PR diff. A missing/failed `gh` is non-fatal: we degrade to
	// a metadata-only PrDiff so the gate + JSON shape stay valid (mirrors the
	// previous stub behaviour, but only on the failure path).
	diff := r.fetchDiffOrMeta(ctx, repo, prNum, opts.PrURL)

	observations := ReadDiffObservations(diff, scopeLevel)
	report := BuildNativeDriftReport(diff, observations, opts.GatePolicy)
	return reportToMap(report, opts.Summary, report.Gated, report.Summary)
}

// fetchDiffOrMeta fetches the real PR diff via the GitHub CLI, degrading to a
// metadata-only PrDiff when `gh` is unavailable or the call fails. The ref
// passed to gh prefers the full URL (gh resolves it directly); otherwise it
// builds an "owner/repo#N" ref from the parsed identifier.
//
// The degrade is loud: a metadata-only PrDiff yields zero diff observations, so
// the WHY (gh missing, fetch error) is written to diffFetchWarnWriter — a
// silent fallback left operators staring at an empty assessment with no
// explanation.
func (r *Runner) fetchDiffOrMeta(ctx context.Context, repo string, prNum int, prURL string) PrDiff {
	ref := prURL
	if ref == "" && repo != "" && prNum > 0 {
		// gh understands "owner/repo#N"; strip the host from "github.com/owner/repo".
		ref = strings.TrimPrefix(repo, "github.com/") + fmt.Sprintf("#%d", prNum)
	}
	if ref != "" {
		d, err := FetchPRDiff(ctx, repo, prNum, ref)
		if err == nil {
			return d
		}
		if errors.Is(err, ErrDiffFetchUnavailable) {
			_, _ = fmt.Fprintf(diffFetchWarnWriter,
				"warning: %v; degrading to metadata-only assessment (no diff observations)\n", err)
		} else {
			_, _ = fmt.Fprintf(diffFetchWarnWriter,
				"warning: PR diff fetch failed for %s: %v; degrading to metadata-only assessment (no diff observations)\n",
				ref, err)
		}
	}
	// Degrade: metadata-only (no Title — avoids spurious decision-signal matches).
	return PrDiff{Repository: repo, PrNumber: prNum}
}

// reportToMap marshals a report struct to map[string]any (the consistent caller
// shape) or, in summary mode, returns the {gated, summaryText} envelope.
func reportToMap(report any, summary, gated bool, summaryText string) (any, error) {
	if summary {
		return map[string]any{
			"gated":       gated,
			"summaryText": summaryText,
		}, nil
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("arch assess native: encode report: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("arch assess native: decode report: %w", err)
	}
	return out, nil
}

// parsePRURL extracts a repository identifier and PR number from a GitHub PR URL
// of the form https://github.com/org/repo/pull/NNN.
func parsePRURL(prURL string) (repo string, prNum int, ok bool) {
	// Minimal parser — no external imports needed.
	// Expected form: https://github.com/<owner>/<repo>/pull/<number>
	const prefix = "https://github.com/"
	if len(prURL) <= len(prefix) {
		return "", 0, false
	}
	rest := prURL[len(prefix):]
	// rest = "owner/repo/pull/NNN"
	parts := strings.Split(rest, "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", 0, false
	}
	var n int
	if _, err := fmt.Sscanf(parts[3], "%d", &n); err != nil {
		return "", 0, false
	}
	return "github.com/" + parts[0] + "/" + parts[1], n, true
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

// IsArchAvailable returns true. The native Go arch-intel pipeline is always
// available (no external binary required). The deprecated TS shim, when opted
// into via DONMAI_ARCH_BIN or af-arch on PATH, runs instead. Callers should not
// gate on this method.
func (r *Runner) IsArchAvailable() bool {
	return true
}

// IsArchBinAvailable returns true only when the DEPRECATED TS arch shim is opted
// into — via DONMAI_ARCH_BIN (or legacy AGENTFACTORY_ARCH_BIN), or af-arch on
// PATH. arch.go uses it to decide whether to wire the native LLM lane (when the
// shim is NOT opted into) vs deferring to the legacy shim.
func (r *Runner) IsArchBinAvailable() bool {
	_, err := r.resolveArchBin()
	return err == nil
}
