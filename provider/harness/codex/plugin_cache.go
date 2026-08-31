package codex

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

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

const (
	// codexPluginCacheMaxFileBytes bounds a single reused cache entry so a
	// corrupted or unexpectedly huge host cache cannot make every fresh
	// session pay to reproduce an unbounded amount of data.
	codexPluginCacheMaxFileBytes = 256 << 20 // 256 MiB
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
// boundary manages stay exactly as isolated as before. cache/ is safe to
// share because it holds nothing session- or credential-specific — it is
// Codex's own network-fetched, content/request-hash-keyed vendor discovery
// data (a remote plugin/apps catalog and its per-app siblings), identical
// regardless of which session fetched it.
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

// reuseCacheTree reproduces every regular file under src into the
// corresponding relative path under dst.
//
//   - A destination entry that already exists at the same relative path is
//     left untouched, never overwritten: cache entries are named by
//     content/request hash, so the same relative path can only ever mean the
//     same content — re-copying it would be wasted work, never a staleness
//     fix.
//   - A symlink anywhere under src is skipped, not followed or reproduced —
//     a legitimate vendor cache never contains one, so this is pure defense
//     against a tampered cache directory smuggling a path escape into either
//     direction (host→session seed or session→host harvest).
//   - A hard link is tried first: src and dst always sit under the same
//     host-owned state-directory tree in production, so this is normally
//     free (no data copied) and keeps the multi-hundred-megabyte catalog
//     file from being duplicated once per session. A cross-device or
//     otherwise link-incapable filesystem (test temp dirs on separate
//     mounts, a `configTempDir` override) falls back to a plain copy.
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
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // best-effort: skip an unreadable entry, keep walking.
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
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return nil //nolint:nilerr // unreachable in practice (path is always under src); never fatal to the walk.
		}
		reuseCacheEntry(path, filepath.Join(dst, rel), d)
		return nil
	})
}

// reuseCacheEntry reproduces one cache file at target, skipping anything
// already there. Errors are for the caller's optional debug log only —
// reuseCacheTree never propagates them.
func reuseCacheEntry(source, target string, d fs.DirEntry) {
	if _, err := os.Lstat(target); err == nil {
		return // already present; cache entries are immutable by name.
	}
	fi, err := d.Info()
	if err != nil || fi.Size() > codexPluginCacheMaxFileBytes {
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), codexHomeMode); err != nil {
		return
	}
	if err := os.Link(source, target); err == nil {
		return
	}
	_ = copyFileBestEffort(source, target, fi.Mode())
}

// copyFileBestEffort is reuseCacheEntry's fallback when a hard link is not
// possible (typically a cross-device destination). O_EXCL makes a
// concurrent duplicate attempt (two sessions racing to seed or harvest the
// same new hash) fail harmlessly on whichever loses, rather than corrupt the
// file either side reads.
func copyFileBestEffort(source, target string, mode os.FileMode) error {
	in, err := os.Open(source) //nolint:gosec // G304: source is a path this package's own WalkDir just enumerated under a codex-owned cache directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()) //nolint:gosec // G304: target is a path this package's own WalkDir just built under a codex-owned cache directory.
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}
