package codex

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/internal/statepath"
)

// codexPluginCacheSubdir is the ONE CODEX_HOME subdirectory this file ever
// reads from or writes to. Every host-invariant, network-fetched vendor
// cache Codex maintains — a remote plugin/apps catalog and its per-app
// server-info/tool-schema siblings, observed under a real codex-cli home —
// lives under "cache/", filed by content/request hash, never by session
// identity. config.toml, auth.json, history, and session/rollout state all
// live OUTSIDE this subdirectory and this file never looks at them.
const codexPluginCacheSubdir = "cache"

// codexPluginCacheAllowedTopLevel is the exact, closed set of cache/
// top-level entries this package will ever seed or harvest — observed
// directly against a real codex-cli home: a remote plugin/apps catalog and
// three per-app siblings (directory listing, per-app server info, per-app
// tool schemas). A new vendor cache entry Codex introduces later is inert
// by default — never shared between sessions — until this list is
// deliberately extended, rather than shared automatically the moment it
// appears. Sharing everything under cache/ unconditionally would be an
// observation about one Codex version, not a guarantee about any other.
var codexPluginCacheAllowedTopLevel = map[string]bool{
	"remote_plugin_catalog":  true,
	"codex_app_directory":    true,
	"codex_apps_server_info": true,
	"codex_apps_tools":       true,
}

const (
	// codexPluginCacheMaxFileBytes bounds a single reused cache entry. The
	// largest observed real catalog file is ~16.5 MiB; this leaves headroom
	// for growth without inviting a corrupted or adversarial host cache to
	// make every fresh session reproduce an unbounded amount of data.
	codexPluginCacheMaxFileBytes = 32 << 20 // 32 MiB
	// codexPluginCacheMaxTotalBytes bounds the TOTAL bytes copied across one
	// seed or harvest call — the per-file cap alone does not bound a
	// directory with many files, and os.TempDir() is commonly tmpfs
	// (RAM-backed) on a typical Linux daemon host, where "bounded per file"
	// and "bounded in aggregate" are very different promises.
	codexPluginCacheMaxTotalBytes = 96 << 20 // 96 MiB
	// codexPluginCacheMaxEntries bounds total files walked per seed/harvest
	// call. The real catalog is a handful of files; this ceiling only exists
	// so a pathologically large cache directory cannot turn every session
	// spawn (or every cleanup) into unbounded filesystem work.
	codexPluginCacheMaxEntries = 4096
)

// codexPluginCacheDirEnv is a test/operator override for the host-level warm
// cache location; production leaves it unset and gets the real per-host
// state-dir path.
const codexPluginCacheDirEnv = "DONMAI_CODEX_PLUGIN_CACHE_DIR"

// codexPluginCacheDisabledEnv is the operator escape hatch: set to "1" to
// fall back to the pre-existing behavior (every session pays its own cold
// fetch) without a code change, if host cache reuse is ever suspected of
// causing a problem in the field.
const codexPluginCacheDisabledEnv = "DONMAI_CODEX_PLUGIN_CACHE_DISABLED"

func codexPluginCacheDisabled() bool {
	return os.Getenv(codexPluginCacheDisabledEnv) == "1"
}

// resolveCodexPluginCacheDir returns the host-level directory donmai uses to
// persist Codex's own warm, host-invariant network caches ACROSS the
// otherwise-fresh-per-session CODEX_HOME boundary (see enablePluginCacheReuse).
// explicit is Options.pluginCacheDir, a test seam; production leaves it empty
// and this resolves to ~/.donmai/codex/plugin-cache (or the env override,
// for an operator who wants it elsewhere without a code change).
func resolveCodexPluginCacheDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv(codexPluginCacheDirEnv); env != "" {
		return env
	}
	return statepath.Resolve(
		filepath.Join("codex", "plugin-cache"),
		filepath.Join(os.TempDir(), "donmai-codex-plugin-cache"),
	)
}

// enablePluginCacheReuse points this boundary at hostCacheDir — the
// host-level warm cache of Codex's own cache/ subtree — and immediately
// seeds this session's isolated home from whatever is already warm there.
//
// This is the ONLY thing this boundary ever shares across sessions: the
// per-session config.toml, the linked auth.json, and every other file this
// boundary manages stay exactly as isolated as before. Only the four
// allowlisted cache/ entries (codexPluginCacheAllowedTopLevel) are ever
// shared — they hold nothing session- or credential-specific: Codex's own
// network-fetched, content/request-hash-keyed vendor discovery data,
// identical regardless of which session fetched it.
//
// Called at most once, right after successful construction, by a boundary's
// owner (New's headless boundary, newInteractiveCodexConfigBoundary's
// interactive one) — never by existing boundary tests that want the
// pre-existing, cache-reuse-free behavior: the zero value of
// b.pluginCacheDir keeps remove() a pure removal with no harvest step,
// exactly what every test that never calls this method already assumes.
//
// Best-effort throughout: a seed failure just means this session pays the
// cold-fetch cost every session paid before this existed. It must never
// fail session construction.
func (b *codexConfigBoundary) enablePluginCacheReuse(hostCacheDir string) {
	if b == nil || hostCacheDir == "" || b.home == "" || codexPluginCacheDisabled() {
		return
	}
	b.pluginCacheDir = hostCacheDir
	dst := filepath.Join(b.home, codexPluginCacheSubdir)
	if err := reuseCacheTree(hostCacheDir, dst); err != nil {
		slog.Debug("codex: plugin cache seed skipped", "hostCacheDir", hostCacheDir, "err", err)
	}
}

// harvestPluginCache copies back whatever this session's own cache/
// subdirectory holds that the host-level cache did not already have — the
// new catalog fetch, request-hash-named, that THIS session paid for because
// the host cache did not have it yet — so the NEXT session on this host
// gets to skip that fetch too. Called by remove(), before the home directory
// (cache/ included) is removed. A no-op when enablePluginCacheReuse was
// never called (b.pluginCacheDir == "", the default for every boundary that
// doesn't opt in).
func (b *codexConfigBoundary) harvestPluginCache() {
	if b == nil || b.pluginCacheDir == "" || b.home == "" {
		return
	}
	src := filepath.Join(b.home, codexPluginCacheSubdir)
	if err := reuseCacheTree(src, b.pluginCacheDir); err != nil {
		slog.Debug("codex: plugin cache harvest skipped", "hostCacheDir", b.pluginCacheDir, "err", err)
	}
}

// reuseCacheTree reproduces every allowlisted, regular file under src into
// the corresponding relative path under dst.
//
//   - Only the four allowlisted top-level entries (codexPluginCacheAllowedTopLevel)
//     are ever descended into; anything else directly under src — a future
//     vendor cache this package has not been taught about, or anything
//     unexpected — is skipped entirely, never read or reproduced. See that
//     var's doc comment.
//   - A destination entry that already exists at the same relative path is
//     left untouched, never overwritten: cache entries are named by
//     content/request hash, so the same relative path can only ever mean the
//     same content — re-copying it would be wasted work, never a staleness
//     fix.
//   - A symlink anywhere under src is skipped, not followed or reproduced —
//     a legitimate vendor cache never contains one, so this is pure defense
//     against a tampered cache directory smuggling a path escape into either
//     direction (host→session seed or session→host harvest).
//   - Every copy is atomic (see copyFileAtomic): a process that dies
//     mid-copy — the exact crash class this whole mechanism exists to
//     survive, and harvest runs inside remove(), on the same exit path —
//     never leaves a truncated file at the file's own canonical name, which
//     the never-overwrite rule above would otherwise make a permanently
//     poisoned entry for every future session on the host.
//
// Every per-file error is swallowed and that one file is skipped — the walk
// always completes, and one unreadable or unwritable entry never blocks
// every other entry from being reused. The only errors this function returns
// are ones that mean the walk never meaningfully started (src is not a
// directory, or src does not exist yet — the ordinary case for a brand-new
// host with nothing warmed up).
func reuseCacheTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect cache source %q: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("cache source %q is not a plain directory", src)
	}
	entries := 0
	var totalBytes int64
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // best-effort: skip an unreadable entry, keep walking.
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil //nolint:nilerr // unreachable in practice (path is always under src); never fatal to the walk.
		}
		topLevel := rel
		if idx := strings.IndexRune(rel, filepath.Separator); idx >= 0 {
			topLevel = rel[:idx]
		}
		if !codexPluginCacheAllowedTopLevel[topLevel] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		entries++
		if entries > codexPluginCacheMaxEntries {
			return fs.SkipAll
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.Size() > codexPluginCacheMaxFileBytes {
			return nil
		}
		if totalBytes+fi.Size() > codexPluginCacheMaxTotalBytes {
			return fs.SkipAll
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Lstat(target); err == nil {
			return nil // already present; cache entries are immutable by name.
		}
		if err := os.MkdirAll(filepath.Dir(target), codexHomeMode); err != nil {
			return nil
		}
		if err := copyFileAtomic(path, target, fi.Mode()); err == nil {
			totalBytes += fi.Size()
		}
		return nil
	})
}

// copyFileAtomic copies source to target by writing into a sibling temp file
// in target's own directory (guaranteeing the same filesystem, so the final
// rename is atomic) and renaming it into place only once the copy fully
// succeeds. target's canonical name never exists until that rename completes
// — a crash, kill, or write error at any point before then leaves at most an
// orphaned, uniquely-named temp file, never a truncated file at the name
// reuseCacheTree's never-overwrite rule would otherwise treat as valid
// content forever.
func copyFileAtomic(source, target string, mode os.FileMode) error {
	in, err := os.Open(source) //nolint:gosec // G304: source is a path this package's own WalkDir just enumerated under a codex-owned, allowlisted cache directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return copyReaderAtomic(in, target, mode)
}

// copyReaderAtomic is copyFileAtomic's core, taking an io.Reader directly so
// an interrupted-read failure partway through a copy — the exact "process
// dies mid-copy" class F4 exists to close, and the one case a real *os.File
// source cannot deterministically reproduce in a test — is exercisable
// without a real filesystem-level fault.
func copyReaderAtomic(r io.Reader, target string, mode os.FileMode) error {
	tmp := target + ".donmai-tmp-" + strconv.Itoa(os.Getpid())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()) //nolint:gosec // G304: tmp is derived from target, a path this package's own WalkDir just built.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
