package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nestedCheckEnv re-executes the test binary to run Backend.Check from inside
// an outer profile.
const nestedCheckEnv = "DONMAI_CONFINEMENT_TEST_NESTED_CHECK"

func runNestedCheckFromEnv() (bool, int) {
	if os.Getenv(nestedCheckEnv) == "" {
		return false, 0
	}
	backend := DefaultBackend()
	if backend == nil {
		fmt.Println("reason=backend_absent")
		return true, 0
	}
	err := backend.Check()
	reason, _ := ReasonOf(err)
	fmt.Printf("reason=%s\n", reason)
	return true, 0
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
