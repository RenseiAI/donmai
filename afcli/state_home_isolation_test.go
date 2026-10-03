package afcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/testisolation"
)

// TestStateHomeIsolationKeepsTestsOffTheRealHome is the guard for the
// TestMain isolation: it fails when the package's tests resolve any CLI
// home-anchored path outside the isolated temp home. Removing the TestMain
// isolation makes every assertion below fail against the real home.
func TestStateHomeIsolationKeepsTestsOffTheRealHome(t *testing.T) {
	realHome := testisolation.RealHome()
	if realHome == "" {
		t.Skip("no resolvable real home to guard")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve test home: %v", err)
	}
	if home == realHome {
		t.Fatalf("test HOME %q is the real home %q: TestMain isolation is missing", home, realHome)
	}
	for name, p := range map[string]string{
		"daemon log":         expandHomePath(defaultDaemonLogFile),
		"signature catalog":  defaultSignatureCatalogPath(),
		"control token file": controlTokenPath(),
	} {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, home+string(os.PathSeparator)) {
			t.Errorf("CLI path %s = %q, want it under isolated HOME %q", name, p, home)
		}
		if strings.HasPrefix(p, realHome+string(os.PathSeparator)) {
			t.Errorf("CLI path %s resolves to the real home: %q", name, p)
		}
	}
	if got := filepath.Join(home, ".donmai", "daemon.jwt"); !strings.HasPrefix(got, home) {
		t.Errorf("canonical state path escapes the isolated home: %q", got)
	}
}
