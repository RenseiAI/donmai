//go:build darwin

package workarea

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileIdentityRecordsBirthOnDarwin pins the darwin birth recording on
// which the renumber tolerance depends: fileIdentity of a fresh directory
// must carry HasBirth with a non-zero birth time, and NewAcquisitionStore
// must persist that birth time into both store.json and the parent anchor.
// If darwin ever stops recording birth, this test goes RED instead of
// silently falling back to inode-only matching.
func TestFileIdentityRecordsBirthOnDarwin(t *testing.T) {
	dir := t.TempDir()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := fileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	if !ident.HasBirth {
		t.Fatal("fileIdentity on darwin must set HasBirth")
	}
	if ident.BirthSec == 0 {
		t.Fatal("fileIdentity on darwin must record a non-zero birth time")
	}

	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	storeRecord := readStoreIdentityForTest(t, filepath.Join(parent, acquisitionStoreDirName, acquisitionStoreIdentityName))
	anchorRecord := readStoreIdentityForTest(t, filepath.Join(parent, acquisitionStoreAnchorName))
	if !storeRecord.Identity.HasBirth {
		t.Fatal("store.json must carry birth time on darwin")
	}
	if !anchorRecord.Identity.HasBirth {
		t.Fatal("parent anchor must carry birth time on darwin")
	}
	if storeRecord.Identity.BirthSec == 0 || anchorRecord.Identity.BirthSec == 0 {
		t.Fatal("stored birth time on darwin must be non-zero")
	}
}
