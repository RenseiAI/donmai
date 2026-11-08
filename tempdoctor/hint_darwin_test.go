//go:build darwin

package tempdoctor

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDirEntryCountIsExactPastSixteenBits pins why the darwin hint reads
// getattrlist rather than stat: stat's link count is 16 bits wide and
// saturates at 65535, so a flat cache of millions of entries would be
// reported as 65k.
func TestDirEntryCountIsExactPastSixteenBits(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "flat")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Dangling symlinks: one syscall per directory entry, no file opened.
	const want = 65536 + 464
	for i := range want {
		if err := os.Symlink("x", filepath.Join(dir, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := childCountHint(dir)(dir, info)
	if !ok || got != want {
		t.Fatalf("entry count = %d (ok=%v), want %d", got, ok, want)
	}
}
