package pi

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// This file is the D8-style fixture family for the content-addressed
// materialization cache (cold-start hardening): a cache hit must
// be indistinguishable from a fresh write to every EXISTING correctness
// check (mode, byte content, digest verification), and a cache miss/failure
// must degrade to the pre-cache behavior rather than to a wrong result.

// TestWriteViaCache_MissThenHitProduceIdenticalContent proves the basic
// contract: a cold write (miss) and a warm write (hit, hard-linked from the
// blob the first write populated) land byte-identical, correctly-permissioned
// files at their own destinations — the caller cannot tell which path was
// taken.
func TestWriteViaCache_MissThenHitProduceIdenticalContent(t *testing.T) {
	// Not parallel: DONMAI_PI_EXT_CACHE_DIR is process env.
	t.Setenv(piExtCacheDirEnvVar, t.TempDir())

	body := []byte("export default function activate(pi) { /* cache fixture */ }\n")
	digest := sha256Hex(body)

	dest1 := filepath.Join(t.TempDir(), "session-1", "pack.ts")
	if err := os.MkdirAll(filepath.Dir(dest1), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeViaCache(digest, body, dest1, 0o600); err != nil {
		t.Fatalf("writeViaCache (miss): %v", err)
	}
	assertMode0600(t, dest1)
	got1, err := os.ReadFile(dest1)
	if err != nil || string(got1) != string(body) {
		t.Fatalf("dest1 content = %q, err %v; want %q", got1, err, body)
	}

	dest2 := filepath.Join(t.TempDir(), "session-2", "pack.ts")
	if err := os.MkdirAll(filepath.Dir(dest2), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeViaCache(digest, body, dest2, 0o600); err != nil {
		t.Fatalf("writeViaCache (hit): %v", err)
	}
	assertMode0600(t, dest2)
	got2, err := os.ReadFile(dest2)
	if err != nil || string(got2) != string(body) {
		t.Fatalf("dest2 content = %q, err %v; want %q", got2, err, body)
	}

	// The two destinations must be the SAME inode (proves the second write
	// actually took the hardlink fast path, not a coincidentally-identical
	// fresh write).
	fi1, err1 := os.Stat(dest1)
	fi2, err2 := os.Stat(dest2)
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v / %v", err1, err2)
	}
	if !os.SameFile(fi1, fi2) {
		t.Errorf("dest1 and dest2 are different inodes; want the second write to hardlink from the cache")
	}
}

// TestWriteViaCache_OverwritesExistingDestination proves destPath's existing
// content (a resume re-materializing over the same path) is fully replaced,
// not appended to or left stale on a cache hit.
func TestWriteViaCache_OverwritesExistingDestination(t *testing.T) {
	t.Setenv(piExtCacheDirEnvVar, t.TempDir())
	dest := filepath.Join(t.TempDir(), "pack.ts")
	if err := os.WriteFile(dest, []byte("stale content"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := []byte("fresh content")
	if err := writeViaCache(sha256Hex(body), body, dest, 0o600); err != nil {
		t.Fatalf("writeViaCache: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(body) {
		t.Fatalf("dest content = %q, err %v; want %q (stale content must not survive)", got, err, body)
	}
}

// TestWriteViaCache_DisabledCacheDirFallsBackToPlainWrite proves the cache is
// advisory: pointing DONMAI_PI_EXT_CACHE_DIR at a path that cannot be created
// (a file, not a directory, sitting in its place) never breaks
// materialization — it just never benefits from the cache. Correctness must
// never depend on the cache being available.
func TestWriteViaCache_DisabledCacheDirFallsBackToPlainWrite(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(piExtCacheDirEnvVar, blocked) // MkdirAll(blocked, ...) will fail: blocked is a file

	body := []byte("still writes fine")
	dest := filepath.Join(t.TempDir(), "pack.ts")
	if err := writeViaCache(sha256Hex(body), body, dest, 0o600); err != nil {
		t.Fatalf("writeViaCache with an unusable cache dir: %v, want nil (must degrade, not fail)", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(body) {
		t.Fatalf("dest content = %q, err %v; want %q", got, err, body)
	}
}

// TestWriteViaCache_ConcurrentIdenticalWritesRaceCleanly is the N-instance
// analogue at the unit level: many goroutines materializing the SAME digest
// concurrently (the boundary-extension fan-out shape) must never corrupt the
// cache blob or any destination file, under -race. populate's hardlink+
// atomic-rename design is what this pins.
func TestWriteViaCache_ConcurrentIdenticalWritesRaceCleanly(t *testing.T) {
	t.Setenv(piExtCacheDirEnvVar, t.TempDir())
	body := []byte("concurrent fixture body\n")
	digest := sha256Hex(body)

	const n = 50
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dest := filepath.Join(root, "sess-"+strconv.Itoa(i), "pack.ts")
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				errs[i] = err
				return
			}
			errs[i] = writeViaCache(digest, body, dest, 0o600)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: writeViaCache: %v", i, err)
		}
		got, readErr := os.ReadFile(filepath.Join(root, "sess-"+strconv.Itoa(i), "pack.ts"))
		if readErr != nil || string(got) != string(body) {
			t.Errorf("goroutine %d: content = %q, err %v; want %q", i, got, readErr, body)
		}
	}
}

// TestMaterializeExtension_RepeatedCallsShareTheCacheBlob is the integration
// point: materializeExtension (the boundary extension, called on EVERY
// spawn) goes through writeViaCache with a FIXED digest (extensionSHA()), so
// two sessions in the same process must resolve to the same cache blob.
func TestMaterializeExtension_RepeatedCallsShareTheCacheBlob(t *testing.T) {
	t.Setenv(piExtCacheDirEnvVar, t.TempDir())

	layout1, err := materializeExtension(t.TempDir())
	if err != nil {
		t.Fatalf("materializeExtension (1st): %v", err)
	}
	layout2, err := materializeExtension(t.TempDir())
	if err != nil {
		t.Fatalf("materializeExtension (2nd): %v", err)
	}
	fi1, err1 := os.Stat(layout1.extension)
	fi2, err2 := os.Stat(layout2.extension)
	if err1 != nil || err2 != nil {
		t.Fatalf("stat: %v / %v", err1, err2)
	}
	if !os.SameFile(fi1, fi2) {
		t.Errorf("two sessions' materialized boundary extensions are different inodes; want the 2nd to hardlink from the cache the 1st populated")
	}
	// Both must still independently verify — the whole point of the cache is
	// that it is invisible to this check.
	for _, layout := range []sessionLayout{layout1, layout2} {
		data, err := os.ReadFile(layout.extension)
		if err != nil {
			t.Fatalf("read %s: %v", layout.extension, err)
		}
		if string(data) != string(extensionSource()) {
			t.Errorf("materialized extension at %s differs from embedded source", layout.extension)
		}
	}
}
