package tempdoctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/tempown"
)

// ErrToolCachesRefused reports that shared tool caches were requested but not
// removed: no confirmation, a seat running, or no way to tell.
var ErrToolCachesRefused = errors.New("tempdoctor: shared tool caches were not removed")

// FixOptions configures Fix.
type FixOptions struct {
	// Root is the folder to fix. Empty means os.TempDir().
	Root string
	// IncludeToolCaches also removes the shared tool caches (tsx, Node
	// compile cache, Go build dirs, Codex CLI temp files, jiti). It requires
	// SeatsRunning to report zero, before and after Confirm accepts.
	IncludeToolCaches bool
	// Confirm is asked once, with a description of what will be removed,
	// before tool caches are removed. Nil refuses.
	Confirm func(prompt string) (bool, error)
	// SeatsRunning reports how many seats are running on this host. Nil, an
	// error, or a non-zero count refuses tool-cache removal.
	SeatsRunning func(ctx context.Context) (int, error)
	// CodexRoots are the roots swept for never-used codex homes. Nil means
	// providercodex.DefaultCodexHomeRoots().
	CodexRoots []string
	// Grace is how long a dead owner's artefact must sit idle before it is
	// removed; zero means tempown.DefaultGrace (codex homes keep the codex
	// sweep's own MinAge).
	Grace time.Duration
	// Logger receives the sweeps' log lines. Nil discards them.
	Logger *slog.Logger

	// janitor and codexSweep are test seams.
	janitor    func(context.Context, tempown.SweepOptions) tempown.SweepReport
	codexSweep func(context.Context, providercodex.SweepOptions) providercodex.SweepReport
}

// ToolCacheResult reports the tool-cache step of Fix.
type ToolCacheResult struct {
	Requested bool              `json:"requested"`
	Removed   []string          `json:"removed"`
	Failed    map[string]string `json:"failed,omitempty"`
	Refused   string            `json:"refused,omitempty"`
}

// FixResult reports what Fix removed.
type FixResult struct {
	Janitor    tempown.SweepReport       `json:"janitor"`
	Codex      providercodex.SweepReport `json:"codex"`
	ToolCaches ToolCacheResult           `json:"toolCaches"`
}

// Fix removes daemon-owned dead artefacts under opts.Root:
//
//   - artefacts tempown recorded whose owner is gone and that sat idle past
//     the grace period (tempown.Sweep);
//   - codex homes no codex process was ever handed whose owner is confirmed
//     dead (providercodex.SweepOrphans with NeverUsedOnly). Every other codex
//     home stays with the daemon's own sweep, which alone knows which homes
//     live adopted sessions reference.
//
// With IncludeToolCaches it then removes the shared tool caches, only after
// SeatsRunning reports none and Confirm accepts. A refusal returns the result
// together with ErrToolCachesRefused.
func Fix(ctx context.Context, opts FixOptions) (FixResult, error) {
	root := opts.Root
	if root == "" {
		root = os.TempDir()
	}
	root = filepath.Clean(root)
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	janitor := opts.janitor
	if janitor == nil {
		janitor = tempown.Sweep
	}
	codexSweep := opts.codexSweep
	if codexSweep == nil {
		codexSweep = providercodex.SweepOrphans
	}

	var result FixResult
	result.Janitor = janitor(ctx, tempown.SweepOptions{Root: root, Grace: opts.Grace, Logger: logger})

	roots := opts.CodexRoots
	if roots == nil {
		roots = providercodex.DefaultCodexHomeRoots()
	}
	for _, codexRoot := range providercodex.DedupeCodexHomeRoots(roots) {
		r := codexSweep(ctx, providercodex.SweepOptions{Root: codexRoot, NeverUsedOnly: true, Logger: logger})
		result.Codex.Scanned += r.Scanned
		result.Codex.Reclaimed += r.Reclaimed
		result.Codex.NeverUsed += r.NeverUsed
		result.Codex.SkippedOutOfScope += r.SkippedOutOfScope
		result.Codex.SkippedYoung += r.SkippedYoung
		result.Codex.SkippedUnowned += r.SkippedUnowned
		result.Codex.SkippedProtected += r.SkippedProtected
		result.Codex.Errors += r.Errors
	}

	if !opts.IncludeToolCaches {
		return result, nil
	}
	result.ToolCaches = removeToolCaches(ctx, root, opts)
	if result.ToolCaches.Refused != "" {
		return result, fmt.Errorf("%w: %s", ErrToolCachesRefused, result.ToolCaches.Refused)
	}
	return result, nil
}

func removeToolCaches(ctx context.Context, root string, opts FixOptions) ToolCacheResult {
	res := ToolCacheResult{Requested: true, Removed: []string{}}
	if reason := seatsBlock(ctx, opts.SeatsRunning); reason != "" {
		res.Refused = reason
		return res
	}
	caches, err := toolCacheEntries(root)
	if err != nil {
		res.Refused = "could not list " + root + ": " + err.Error()
		return res
	}
	if len(caches) == 0 {
		return res
	}
	if opts.Confirm == nil {
		res.Refused = "removal was not confirmed"
		return res
	}
	names := make([]string, 0, len(caches))
	for _, c := range caches {
		names = append(names, filepath.Base(c))
	}
	prompt := fmt.Sprintf("Remove %d shared tool cache entr%s in %s (%s)? Every tool that uses them starts cold.",
		len(caches), plural(len(caches), "y", "ies"), root, summarizeNames(names))
	ok, err := opts.Confirm(prompt)
	if err != nil {
		res.Refused = "confirmation failed: " + err.Error()
		return res
	}
	if !ok {
		res.Refused = "removal was not confirmed"
		return res
	}
	// A seat may have started while the operator read the prompt.
	if reason := seatsBlock(ctx, opts.SeatsRunning); reason != "" {
		res.Refused = reason
		return res
	}
	for _, path := range caches {
		if ctx.Err() != nil {
			res.Refused = "interrupted: " + ctx.Err().Error()
			return res
		}
		if err := os.RemoveAll(path); err != nil {
			if res.Failed == nil {
				res.Failed = map[string]string{}
			}
			res.Failed[path] = err.Error()
			continue
		}
		res.Removed = append(res.Removed, path)
	}
	return res
}

// seatsBlock returns why tool caches must not be removed now, or "".
func seatsBlock(ctx context.Context, seatsRunning func(context.Context) (int, error)) string {
	if seatsRunning == nil {
		return "cannot confirm that no seat is running"
	}
	n, err := seatsRunning(ctx)
	if err != nil {
		return "cannot confirm that no seat is running: " + err.Error()
	}
	if n > 0 {
		return fmt.Sprintf("%d seat%s running; stop or drain them first", n, plural(n, " is", "s are"))
	}
	return ""
}

// toolCacheEntries lists root's tool-cache entries this user owns: plain
// files or directories, never symlinks.
func toolCacheEntries(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !isToolCache(e.Name()) {
			continue
		}
		path := filepath.Join(root, e.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func summarizeNames(names []string) string {
	const shown = 4
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:shown], ", ") + fmt.Sprintf(", and %d more", len(names)-shown)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
