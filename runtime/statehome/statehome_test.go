package statehome_test

import (
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/runtime/statehome"
)

// These tests mutate process-global seam state, so they must NOT run in
// parallel with one another. Each resets the seam and pins an explicit
// base-home so the assertions never depend on the real $HOME.

func TestDefaultBrandPaths(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	base := t.TempDir()
	statehome.SetBaseHome(base)

	if got, want := statehome.Brand(), "donmai"; got != want {
		t.Fatalf("default Brand() = %q, want %q", got, want)
	}

	if got, want := statehome.StateDir("daemon.yaml"), filepath.Join(base, ".donmai", "daemon.yaml"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := statehome.LogDir(), filepath.Join(base, "Library", "Logs", "donmai"); got != want {
		t.Errorf("LogDir = %q, want %q", got, want)
	}
	if got, want := statehome.LogPath(), filepath.Join(base, "Library", "Logs", "donmai", "daemon.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if got, want := statehome.ErrorLogPath(), filepath.Join(base, "Library", "Logs", "donmai", "daemon-error.log"); got != want {
		t.Errorf("ErrorLogPath = %q, want %q", got, want)
	}
}

func TestSetBrandRebrandsPaths(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	base := t.TempDir()
	statehome.SetBaseHome(base)
	statehome.SetBrand("x")

	if got, want := statehome.Brand(), "x"; got != want {
		t.Fatalf("Brand() = %q, want %q", got, want)
	}
	if got, want := statehome.StateDir("kits"), filepath.Join(base, ".x", "kits"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := statehome.LogDir(), filepath.Join(base, "Library", "Logs", "x"); got != want {
		t.Errorf("LogDir = %q, want %q", got, want)
	}
	if got, want := statehome.LogPath(), filepath.Join(base, "Library", "Logs", "x", "daemon.log"); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if got, want := statehome.ErrorLogPath(), filepath.Join(base, "Library", "Logs", "x", "daemon-error.log"); got != want {
		t.Errorf("ErrorLogPath = %q, want %q", got, want)
	}
}

func TestSetBrandEmptyIgnored(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	statehome.SetBaseHome(t.TempDir())
	statehome.SetBrand("custom")
	statehome.SetBrand("") // must be a no-op, not blank the leaf

	if got, want := statehome.Brand(), "custom"; got != want {
		t.Fatalf("Brand() after empty SetBrand = %q, want %q", got, want)
	}
}

func TestSetBaseHomeOverride(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	override := filepath.Join(t.TempDir(), "explicit-home")
	statehome.SetBaseHome(override)

	if got, want := statehome.BaseHome(), override; got != want {
		t.Fatalf("BaseHome() = %q, want %q", got, want)
	}
	if got, want := statehome.StateDir("token"), filepath.Join(override, ".donmai", "token"); got != want {
		t.Errorf("StateDir under override = %q, want %q", got, want)
	}
}

func TestMultiSegmentSuffix(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)

	base := t.TempDir()
	statehome.SetBaseHome(base)

	if got, want := statehome.StateDir(filepath.Join("kits", "abc")), filepath.Join(base, ".donmai", "kits", "abc"); got != want {
		t.Errorf("StateDir multi-segment = %q, want %q", got, want)
	}
}
