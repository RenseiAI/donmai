package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestReuseCacheTree_CopiesExistingHostEntriesAtomically pins the seed
// path's basic shape — an allowlisted file the host cache already has is
// reproduced at the destination with the same content and no leftover temp
// artifact — see copyFileAtomic's doc comment for why every copy goes
// through a same-directory temp file + rename rather than a direct write or
// a hard link: a hard link would share ONE inode across every
// concurrently-live session, silently propagating any in-place rewrite
// Codex might ever do, and nothing in this package has verified Codex does
// not do one.
func TestReuseCacheTree_CopiesExistingHostEntriesAtomically(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), `{"plugins":[]}`)

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
	entries, err := os.ReadDir(filepath.Join(dst, "remote_plugin_catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the reused entry with no leftover temp file, got %v", entries)
	}
}

// TestReuseCacheTree_NeverLeavesATruncatedFileAtTheCanonicalName pins the
// atomicity of a copy: a copy that cannot complete (here, a destination
// directory with no write permission — standing in for any interrupted-
// write cause, crash included, since harvestPluginCache runs inside
// remove() on exactly that exit path) must never leave anything at the
// file's own canonical name. reuseCacheTree's never-overwrite rule (see its
// doc comment) would otherwise make a truncated file there permanent for
// every future session on the host.
//
// RED proof: in copyReaderAtomic, replace the temp-file-then-rename sequence
// with a direct `os.OpenFile(target, os.O_WRONLY|os.O_CREATE, mode.Perm())`
// write and this test fails — a failed/interrupted copy leaves a partial
// file sitting at the canonical name.
func TestReuseCacheTree_NeverLeavesATruncatedFileAtTheCanonicalName(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), strings.Repeat("x", 1024))
	targetDir := filepath.Join(dst, "remote_plugin_catalog")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(targetDir, 0o500); err != nil { //nolint:gosec // G302: the test deliberately removes write permission to force a failed copy.
		t.Fatal(err)
	}
	//nolint:gosec // G302: restoring 0700 so t.TempDir()'s own cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(targetDir, 0o700) })

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(targetDir, "abc123.json")); !os.IsNotExist(err) {
		t.Fatalf("canonical target exists despite a failed copy: err=%v", err)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an orphaned temp file was left behind: %v", entries)
	}
}

// failingReaderAfter returns some real bytes and then a fixed error — used
// to force an interrupted-mid-copy failure deterministically, something a
// real *os.File source cannot reproduce on demand in a test.
type failingReaderAfter struct {
	data []byte
	err  error
}

func (f *failingReaderAfter) Read(p []byte) (int, error) {
	if len(f.data) > 0 {
		n := copy(p, f.data)
		f.data = f.data[n:]
		return n, nil
	}
	return 0, f.err
}

// TestCopyReaderAtomic_InterruptedReadNeverLeavesFileAtCanonicalName is the
// same property in its most direct form: a read that fails partway
// through — exactly what a process dying mid-copy looks like from
// io.Copy's point of view — must never leave anything at the file's own
// canonical name, and must not leave an orphaned temp file behind either.
//
// RED proof: in copyFileAtomic/copyReaderAtomic, replace the temp-file-then-
// rename sequence with a direct `os.OpenFile(target, os.O_WRONLY|os.O_CREATE, mode.Perm())`
// write straight to target and this test fails — the canonical name ends up
// holding the partial bytes the reader delivered before failing.
func TestCopyReaderAtomic_InterruptedReadNeverLeavesFileAtCanonicalName(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "abc123.json")
	r := &failingReaderAfter{data: []byte("partial content before the read fails"), err: errors.New("simulated interrupted read")}

	if err := copyReaderAtomic(r, target, 0o600); err == nil {
		t.Fatal("expected an error from the failing reader")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("canonical target exists despite an interrupted copy: err=%v", err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an orphaned temp file was left behind: %v", entries)
	}
}

// TestReuseCacheTree_SharesOnlyTheCatalogNotPerAppCaches pins the closed
// allowlist (codexPluginCacheAllowedTopLevel), using the two shapes that
// actually matter.
//
// The per-app names here are real entries a live codex-cli home carries, not
// invented ones: they describe ONE app — its server info, its tool schemas —
// including a private or self-hosted app. This cache is shared host-wide
// across every session on the box, so reproducing them would persist one
// session's private app surface where unrelated sessions are seeded from it.
// A vendor cache this package has never heard of is inert for the same
// reason: sharing it would be a guess about a Codex version nobody has
// looked at.
//
// RED proof: in reuseCacheTree, delete the
// `if !codexPluginCacheAllowedTopLevel[topLevel] { ... }` branch (walking
// every top-level entry unconditionally) and this test fails — the per-app
// caches are reproduced at the destination.
func TestReuseCacheTree_SharesOnlyTheCatalogNotPerAppCaches(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), "catalog-wide, host-invariant")
	perApp := []string{"codex_app_directory", "codex_apps_server_info", "codex_apps_tools"}
	for _, name := range perApp {
		writeCacheFixture(t, src, filepath.Join(name, "private-app.json"), "one app's own surface")
	}
	writeCacheFixture(t, src, filepath.Join("some_future_vendor_cache", "new.json"), "not yet allowlisted")

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "remote_plugin_catalog", "abc123.json")); err != nil {
		t.Fatalf("the catalog-wide entry was not reused: %v", err)
	}
	for _, name := range append(perApp, "some_future_vendor_cache") {
		if _, err := os.Lstat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was reproduced into a host-wide cache shared across unrelated sessions: err=%v", name, err)
		}
	}
}

// TestCopyReaderAtomic_IsNotBlockedByACrashLeftoverTempFile pins the
// crash-recovery property of the temp naming. A pid-derived temp name is not
// unique over time: the crash this mechanism exists to survive leaves one
// behind, and the next process to draw that pid then collides with it on
// every attempt — permanently, because the never-overwrite rule means
// nothing ever cleans the canonical name up either. The fixture plants
// exactly the name a pid-derived scheme would choose for this process.
func TestCopyReaderAtomic_IsNotBlockedByACrashLeftoverTempFile(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "abc123.json")
	leftover := target + codexPluginCacheTempMarker + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(leftover, []byte("truncated bytes from a crashed copy"), 0o600); err != nil {
		t.Fatal(err)
	}

	const body = `{"schema_version":1,"plugins":[]}`
	if err := copyReaderAtomic(strings.NewReader(body), target, 0o600); err != nil {
		t.Fatalf("a crash leftover made this cache entry permanently uncopyable: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("copied body = %q, want %q", got, body)
	}
}

// TestReuseCacheTree_SkipsLeftoverTempFiles pins the other half: a temp file
// a crashed copy left inside an ALLOWLISTED directory is litter, not a cache
// entry. Reproducing it would carry it from one session's home into the
// host-level cache and from there into every future session, where the
// never-overwrite rule would keep it forever.
func TestReuseCacheTree_SkipsLeftoverTempFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", "abc123.json"), "a real entry")
	litter := "abc123.json" + codexPluginCacheTempMarker + "1234567"
	writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", litter), "truncated bytes")

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "remote_plugin_catalog", "abc123.json")); err != nil {
		t.Fatalf("the real entry was not reused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "remote_plugin_catalog", litter)); !os.IsNotExist(err) {
		t.Fatalf("a crashed copy's leftover temp file was reproduced at the destination: err=%v", err)
	}
}

// TestReuseCacheTree_BoundsTotalBytesPerCall pins the aggregate cap: a
// per-file cap alone does not bound a directory holding many files, which
// matters specifically because os.TempDir() is commonly tmpfs (RAM-backed)
// on a typical Linux daemon host. Four files sit right at the per-file cap;
// the fourth pushes cumulative bytes past codexPluginCacheMaxTotalBytes and
// must be skipped.
func TestReuseCacheTree_BoundsTotalBytesPerCall(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	body := strings.Repeat("x", codexPluginCacheMaxFileBytes)
	for _, name := range []string{"a.json", "b.json", "c.json", "d.json"} {
		writeCacheFixture(t, src, filepath.Join("remote_plugin_catalog", name), body)
	}

	if err := reuseCacheTree(src, dst); err != nil {
		t.Fatalf("reuseCacheTree: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dst, "remote_plugin_catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("copied %d files, want exactly 3 before the total-bytes cap stopped the walk (of 4, %d bytes each)", len(entries), codexPluginCacheMaxFileBytes)
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
// nothing ever seeded it.
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
