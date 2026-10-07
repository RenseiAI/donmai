package servicepriority

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if mode, found, err := Load(); err != nil || found || mode != "" {
		t.Fatalf("Load() before Save = (%q, %v, %v), want (\"\", false, nil)", mode, found, err)
	}

	for _, want := range Modes() {
		if err := Save(want); err != nil {
			t.Fatalf("Save(%q): %v", want, err)
		}
		got, found, err := Load()
		if err != nil || !found || got != want {
			t.Fatalf("Load() after Save(%q) = (%q, %v, %v)", want, got, found, err)
		}
	}

	info, err := os.Stat(StatePath())
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file perm = %v, want 0600", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(StatePath()))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}

func TestSaveRejectsEmptyAndInvalid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save(""); err == nil {
		t.Error("Save(\"\") succeeded, want error")
	}
	if err := Save(Mode("utility")); err == nil {
		t.Error("Save(utility) succeeded, want error")
	}
	if _, err := os.Stat(StatePath()); err == nil {
		t.Error("a rejected Save left a state file behind")
	}
}

func TestLoadRejectsCorruptState(t *testing.T) {
	for _, content := range []string{"", "\n", "utility\n", "garbage"} {
		t.Run(strings.TrimSpace(content), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := os.MkdirAll(filepath.Dir(StatePath()), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(StatePath(), []byte(content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if mode, found, err := Load(); err == nil {
				t.Fatalf("Load() = (%q, %v, nil), want an error for corrupt state %q", mode, found, content)
			}
		})
	}
}
