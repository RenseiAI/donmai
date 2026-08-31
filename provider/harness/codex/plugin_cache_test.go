package codex

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCacheFixture writes body at dir/rel, creating parent directories.
func writeCacheFixture(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestReuseCacheTree_HardLinksExistingHostEntries pins the cheap-path case:
// a file the host cache already has is reproduced at the destination as a
// hard link (same inode) — never a copy — so a multi-hundred-megabyte
// catalog is never duplicated per session.
func TestReuseCacheTree_HardLinksExistingHostEntries(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	srcFile := writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), `{"plugins":[]}`)

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	dstFile := filepath.Join(dst, "remote_plugin_catalog", "abc123.json")
	body, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("read reused entry: %v", err)
	}
	if string(body) != `{"plugins":[]}` {
		t.Fatalf("reused entry body = %q", body)
	}
	srcInfo, err := os.Stat(srcFile)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Fatal("reused entry is a copy, not a hard link — every session would duplicate the catalog on disk")
	}
}

// TestReuseCacheTree_MissingSourceIsANoOp pins the first-ever-session-on-
// this-host case: nothing has warmed the host cache yet, so seeding must be
// a harmless no-op rather than an error that could ever be mistaken for
// something worth failing a spawn over.
func TestReuseCacheTree_MissingSourceIsANoOp(t *testing.T) {
	dst := t.TempDir()
	missing := filepath.Join(t.TempDir(), "never-created")
	if err := reuseCacheTree(missing, dst); err != nil {
		t.Fatalf("reuseCacheTree with missing source: %v", err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dst gained entries from a nonexistent source: %v", entries)
	}
}

// TestReuseCacheTree_SkipsSymlinks pins the tamper-defense: a symlink placed
// inside a cache tree (by either side) must never be followed or reproduced
// on the other side — a legitimate vendor cache never contains one.
func TestReuseCacheTree_SkipsSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	outside := writeCacheFixture(t, t.TempDir(), "secret.json", "must not escape")
	if err := os.MkdirAll(filepath.Join(src, "remote_plugin_catalog"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(src, "remote_plugin_catalog", "escape.json")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Fatal(err)
	}

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "remote_plugin_catalog", "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink was reproduced at destination: err=%v", err)
	}
}

// TestReuseCacheTree_NeverOverwritesAnExistingDestinationEntry pins the
// immutable-by-name contract: cache entries are named by content/request
// hash, so an existing destination file at the same relative path is
// already the right content. Overwriting it would be pure risk (a
// concurrent writer, a partially-written file) for zero benefit.
func TestReuseCacheTree_NeverOverwritesAnExistingDestinationEntry(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), "new content")
	writeCacheFixture(t, dst, filepath.Join("remote_plugin_catalog", "abc123.json"), "existing content — must survive")

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dst, "remote_plugin_catalog", "abc123.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "existing content — must survive" {
		t.Fatalf("existing destination entry was overwritten: %q", body)
	}
}

// TestPluginCacheReuse_SecondSessionDoesNotRefetch is the deliverable's
// headline proof: a first session's app-server "fetches" (writes) the
// catalog into ITS isolated home's cache/ subdirectory; on cleanup that
// fetch is harvested into the host-level cache; a second, independent
// session's isolated home is then seeded from the host cache BEFORE its own
// app-server would ever run — proving the second launch has the catalog
// on disk without any network fetch of its own.
//
// RED proof: comment out the `boundary.enablePluginCacheReuse(...)` call in
// codex.go's New (or interactive.go's spawnInteractivePreparedForGOOS) and
// this test fails — session two's home has no cache/ entry at all, because
// nothing ever seeded it. Verified: FAILED ("second session home has no
// seeded cache entry"), then PASSED again after restoring — see the
// completion report for the exact quotes.
func TestPluginCacheReuse_SecondSessionDoesNotRefetch(t *testing.T) {
	hostCache := t.TempDir()
	sessionParent := t.TempDir()

	// Session 1: construct a boundary, opt into cache reuse (host cache is
	// empty — nothing to seed from yet), then simulate the app-server's own
	// cold fetch by writing the catalog directly into the isolated home's
	// cache/ subdirectory, exactly where Codex itself would place it.
	first, err := newCodexConfigBoundary(sessionParent, false)
	if err != nil {
		t.Fatalf("session 1 boundary: %v", err)
	}
	first.enablePluginCacheReuse(hostCache)
	const catalogRelInSession = "cache/remote_plugin_catalog/abc123.json"
	const catalogRelInHostCache = "remote_plugin_catalog/abc123.json"
	const catalogBody = `{"schema_version":1,"plugins":[]}`
	writeCacheFixture(t, first.home, catalogRelInSession, catalogBody)

	if err := first.remove(); err != nil {
		t.Fatalf("session 1 remove (harvest): %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostCache, catalogRelInHostCache)); err != nil {
		t.Fatalf("session 1's fetch was not harvested into the host cache: %v", err)
	}

	// Session 2: a brand-new, independent boundary. Its cache/ subdirectory
	// must already contain the catalog the moment enablePluginCacheReuse
	// returns — before any app-server has run inside it at all.
	second, err := newCodexConfigBoundary(sessionParent, false)
	if err != nil {
		t.Fatalf("session 2 boundary: %v", err)
	}
	t.Cleanup(func() { _ = second.remove() })
	second.enablePluginCacheReuse(hostCache)

	body, err := os.ReadFile(filepath.Join(second.home, catalogRelInSession))
	if err != nil {
		t.Fatalf("second session home has no seeded cache entry: %v", err)
	}
	if string(body) != catalogBody {
		t.Fatalf("seeded cache entry body = %q, want %q", body, catalogBody)
	}
}

// TestEnablePluginCacheReuse_DisabledEnvSkipsSeeding pins the operator
// escape hatch: with DONMAI_CODEX_PLUGIN_CACHE_DISABLED=1, a boundary must
// behave exactly as it did before this feature existed — no seed, and
// remove() does not harvest either (pluginCacheDir stays unset).
func TestEnablePluginCacheReuse_DisabledEnvSkipsSeeding(t *testing.T) {
	t.Setenv(codexPluginCacheDisabledEnv, "1")
	hostCache := t.TempDir()
	writeCacheFixture(t, hostCache, filepath.Join("remote_plugin_catalog", "abc123.json"), "warm")

	b, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatalf("boundary: %v", err)
	}
	t.Cleanup(func() { _ = b.remove() })
	b.enablePluginCacheReuse(hostCache)

	if _, err := os.Stat(filepath.Join(b.home, "cache", "remote_plugin_catalog", "abc123.json")); !os.IsNotExist(err) {
		t.Fatalf("seeding ran despite the disable env var: err=%v", err)
	}
	if b.pluginCacheDir != "" {
		t.Fatalf("pluginCacheDir = %q, want empty when disabled", b.pluginCacheDir)
	}
}

// TestResolveCodexPluginCacheDir_PrefersExplicitThenEnvThenDefault pins the
// override precedence so a test or operator override is never silently
// shadowed by the env var, and the env var is never shadowed by the default.
func TestResolveCodexPluginCacheDir_PrefersExplicitThenEnvThenDefault(t *testing.T) {
	if got, want := resolveCodexPluginCacheDir("/explicit/wins"), "/explicit/wins"; got != want {
		t.Fatalf("resolveCodexPluginCacheDir(explicit) = %q, want %q", got, want)
	}
	t.Setenv(codexPluginCacheDirEnv, "/env/wins")
	if got, want := resolveCodexPluginCacheDir(""), "/env/wins"; got != want {
		t.Fatalf("resolveCodexPluginCacheDir(env) = %q, want %q", got, want)
	}
}
