//go:build unix

package mcpheaders

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAuthorizationJSONRefusesLatePathSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	original := path + ".original"
	writePrivate(t, path, "launch-token")
	realOpen := openBearerFile
	openBearerFile = func(requested string) (*os.File, os.FileInfo, error) {
		if err := os.Rename(requested, original); err != nil {
			return nil, nil, err
		}
		if err := os.Symlink(original, requested); err != nil {
			return nil, nil, err
		}
		return realOpen(requested)
	}
	t.Cleanup(func() { openBearerFile = realOpen })
	if got, err := ReadAuthorizationJSON(path); err == nil || len(got) != 0 {
		t.Fatalf("late symlink substitution accepted: got=%q err=%v", got, err)
	}
}
