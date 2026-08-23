package workarea

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhysicalUsageUsesAllocatedBlocksAndDeduplicatesHardlinks(t *testing.T) {
	root := RootPath(t.TempDir())
	sparse := filepath.Join(root.String(), "sparse.bin")
	file, err := os.OpenFile(sparse, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(64<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	usageBeforeLink, err := PhysicalUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if usageBeforeLink <= 0 || usageBeforeLink >= 64<<20 {
		t.Fatalf("sparse physical usage = %d, want positive and below logical size", usageBeforeLink)
	}
	info, err := os.Stat(sparse)
	if err != nil {
		t.Fatal(err)
	}
	fileBlocks, err := allocatedFileBytes(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sparse, filepath.Join(root.String(), "sparse-hardlink.bin")); err != nil {
		t.Fatal(err)
	}
	usageAfterLink, err := PhysicalUsage(root)
	if err != nil {
		t.Fatal(err)
	}
	if usageAfterLink-usageBeforeLink >= fileBlocks {
		t.Fatalf("hard link double-charged file blocks: before=%d after=%d file=%d", usageBeforeLink, usageAfterLink, fileBlocks)
	}
}

func TestPhysicalUsageFailsClosedOnUnsafeOrUnreadableRoot(t *testing.T) {
	t.Run("symlink-root", func(t *testing.T) {
		external := t.TempDir()
		link := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(external, link); err != nil {
			t.Fatal(err)
		}
		if _, err := PhysicalUsage(RootPath(link)); err == nil {
			t.Fatal("physical accounting followed a symlink root")
		}
	})

	t.Run("unreadable-child", func(t *testing.T) {
		root := RootPath(t.TempDir())
		blocked := filepath.Join(root.String(), "blocked")
		if err := os.Mkdir(blocked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blocked, "data"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) }) //nolint:gosec // restore owner access for TempDir cleanup
		if _, err := PhysicalUsage(root); err == nil {
			t.Fatal("physical accounting returned a partial total for an unreadable subtree")
		}
	})
}
