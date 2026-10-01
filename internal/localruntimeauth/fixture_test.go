package localruntimeauth

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// replaceFixtureFile uses a test-owned directory root for malformed records.
func replaceFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := file.Write(data); err != nil || written != len(data) {
		_ = file.Close()
		t.Fatalf("write test fixture: %v, %v", err, io.ErrShortWrite)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func setFixtureMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
