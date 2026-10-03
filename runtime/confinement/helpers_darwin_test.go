//go:build darwin

package confinement

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chmodBackend makes the read-only leaves and protected paths read-only by
// permission bits under the same identity, which ADR-2026-08-22 D6.7 rules
// out: the harness can undo it. The self-test must turn red against it too.
type chmodBackend struct{ noopBackend }

func (chmodBackend) Apply(req ApplyRequest) (Applied, error) {
	paths := append(append([]string{}, req.Resolved.ReadOnly...), req.Resolved.Protected...)
	for _, path := range paths {
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			return os.Chmod(p, 0o444) //nolint:gosec // G302: the double makes files read-only by permission bits.
		})
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o555) //nolint:gosec // G302: the double makes directories read-only by permission bits.
			}
			return nil
		})
	}
	return Applied{Rendered: []byte("chmod"), Wrap: func(argv []string) []string { return argv }}, nil
}

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
