// Package testisolation keeps the test suites of packages whose tests can
// reach the daemon's on-disk state from touching the developer's real home
// directory.
//
// A test binary calls Isolate in its TestMain before m.Run: HOME (plus the
// platform state-home overrides) is pointed at a fresh temp dir that is
// deleted when the test binary exits. Tests that re-exec the test binary
// inherit the temp HOME through the environment, so helper children stay
// isolated too.
package testisolation

import (
	"os"
	"path/filepath"
	"runtime"
)

// Isolate redirects the home-anchored state paths for the whole test binary
// at a fresh temp dir. Call it from TestMain before m.Run. It returns a
// cleanup that removes the temp dir; TestMain runs it after m.Run returns
// so the dir survives until every test (including re-execed helpers) has
// finished.
func Isolate() func() {
	return IsolateWithPrefix("test-home-")
}

// IsolateWithPrefix is Isolate with an explicit temp-dir name prefix so
// parallel `go test` binaries for different packages do not share a
// directory name pattern in TMPDIR listings.
func IsolateWithPrefix(prefix string) func() {
	if mark := os.Getenv("DONMAI_TEST_HOME_ISOLATED"); mark != "" {
		// A re-execed helper child: it already inherited the isolated
		// HOME through the environment, so keep it untouched.
		return func() {}
	}
	parent := os.TempDir()
	// Keep the temp home off TMPDIR itself: several tests set TMPDIR to
	// their own t.TempDir and some helpers derive paths from it.
	dir, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		// TestMain has no *testing.T; failing fast here is the only safe
		// option — running the suite against the real home would write
		// test residue next to the developer's live daemon state.
		panic("testisolation: create temp home: " + err.Error())
	}
	if err := os.Setenv("DONMAI_TEST_HOME_ISOLATED", "1"); err != nil {
		panic("testisolation: mark isolated home: " + err.Error())
	}
	if err := os.Setenv("DONMAI_TEST_REAL_HOME", realHome()); err != nil {
		panic("testisolation: stash real home: " + err.Error())
	}
	if err := os.Setenv("HOME", dir); err != nil {
		panic("testisolation: set HOME: " + err.Error())
	}
	// os.UserHomeDir consults $HOME on unix and %USERPROFILE% on
	// windows; the machine-identity fallback reads XDG_STATE_HOME /
	// LOCALAPPDATA on the non-darwin / windows paths.
	if err := os.Setenv("XDG_STATE_HOME", filepath.Join(dir, ".local", "state")); err != nil {
		panic("testisolation: set XDG_STATE_HOME: " + err.Error())
	}
	if runtime.GOOS == "windows" {
		if err := os.Setenv("USERPROFILE", dir); err != nil {
			panic("testisolation: set USERPROFILE: " + err.Error())
		}
		if err := os.Setenv("LOCALAPPDATA", filepath.Join(dir, "AppData", "Local")); err != nil {
			panic("testisolation: set LOCALAPPDATA: " + err.Error())
		}
	}
	return func() { _ = os.RemoveAll(dir) }
}

// RealHome returns the developer's real home directory stashed by Isolate,
// or "" when isolation did not run (helper children that skipped it still
// carry the stashed value in the environment).
func RealHome() string {
	if stash := os.Getenv("DONMAI_TEST_REAL_HOME"); stash != "" {
		return stash
	}
	return realHome()
}

func realHome() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if runtime.GOOS == "windows" {
		if home := os.Getenv("USERPROFILE"); home != "" {
			return home
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
