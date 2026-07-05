package codeintel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindTypeUsages_SkipsSymlinkEscapingRoot proves the type-usage scanner does
// NOT follow a symlink whose target lives OUTSIDE the search root. A malicious /
// untrusted repo could plant an in-root symlink (an indexed extension) pointing
// at an arbitrary host file; following it would read that out-of-root file and
// return its verbatim line content — mislabelled as an in-root path — to the
// agent, exfiltrating host secrets one identifier-query at a time.
//
// RED (before the fix): type_usages.go's WalkDir treated a symlink-to-file as a
// regular file, so os.ReadFile followed the link and the out-of-root line leaked
// into the returned usages.
func TestFindTypeUsages_SkipsSymlinkEscapingRoot(t *testing.T) {
	// An "outside" directory that is NOT under the search root.
	outside := t.TempDir()
	secretLine := "type ExfilTarget struct { Password string }"
	writeFile(t, filepath.Join(outside, "private.go"),
		"package private\n\n"+secretLine+"\n")

	root := t.TempDir()
	// A legitimate in-root file that references a DIFFERENT type.
	writeFile(t, filepath.Join(root, "ok.go"),
		"package ok\n\ntype OKType struct{}\n")
	// The malicious symlink: an indexed extension, target outside the root.
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "pkg", "innocent.go")
	if err := os.Symlink(filepath.Join(outside, "private.go"), link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	result, err := FindTypeUsages(root, "ExfilTarget", 50)
	if err != nil {
		t.Fatalf("FindTypeUsages: %v", err)
	}

	if result.TotalUsages != 0 {
		t.Errorf("out-of-root symlink target must NOT be scanned; got %d usages: %+v",
			result.TotalUsages, result.Usages)
	}
	for _, u := range result.Usages {
		if u.Context == secretLine {
			t.Errorf("out-of-root file content leaked through find-type-usages: %q (path %q)",
				u.Context, u.FilePath)
		}
	}
}

// TestValidateCrossDeps_SkipsSymlinkEscapingRoot proves the cross-deps validator
// likewise does not follow an in-root symlink to an out-of-root source file.
//
// RED (before the fix): crossdeps.go's WalkDir os.ReadFile'd the symlink target,
// counting the out-of-root file toward FilesChecked (and parsing its imports).
func TestValidateCrossDeps_SkipsSymlinkEscapingRoot(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "private.ts"),
		"import { Secret } from '@corp/secrets'\n")

	root := t.TempDir()
	// A package.json so there is at least one workspace package to check against.
	writeFile(t, filepath.Join(root, "package.json"),
		`{"name":"@app/root","dependencies":{}}`)
	writeFile(t, filepath.Join(root, "index.ts"),
		"export const x = 1\n")
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "pkg", "innocent.ts")
	if err := os.Symlink(filepath.Join(outside, "private.ts"), link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	result, err := ValidateCrossDeps(root, "")
	if err != nil {
		t.Fatalf("ValidateCrossDeps: %v", err)
	}

	// The out-of-root symlink target must not be read/parsed. Only index.ts is a
	// real in-root source file, so exactly one file may be checked.
	if result.FilesChecked > 1 {
		t.Errorf("out-of-root symlink target must NOT be checked; FilesChecked=%d (want <=1)",
			result.FilesChecked)
	}
}
