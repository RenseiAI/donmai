//go:build linux

package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTempDir returns a short temporary directory: the self-test binds a
// socket in its session tmp, and socket paths are limited to about a hundred
// bytes.
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = removeAllForce(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return resolved
}

// hostDirs returns a throwaway operator home and host state home plus a
// profile directory under the state home.
func hostDirs(t *testing.T) (home, stateHome, profileDir string) {
	t.Helper()
	base := shortTempDir(t, "dch")
	home = filepath.Join(base, "home")
	stateHome = filepath.Join(home, ".state")
	profileDir = filepath.Join(stateHome, "profiles")
	for _, dir := range []string{home, stateHome, profileDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	return home, stateHome, profileDir
}

func failureIDs(record SelfTestRecord) string {
	var ids []string
	for _, probe := range record.Failures() {
		ids = append(ids, fmt.Sprintf("%s/%s (expected %s, observed %s: %s)", probe.Mode, probe.ID, probe.Expected, probe.Observed, probe.Detail))
	}
	return strings.Join(ids, "\n")
}
