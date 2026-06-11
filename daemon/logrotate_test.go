package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateLogIfOver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(t *testing.T, dir string) string // returns path
		maxBytes    int64
		wantRotated bool
		wantErr     bool
	}{
		{
			name: "over threshold rotates",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				p := filepath.Join(dir, "daemon-error.log")
				if err := os.WriteFile(p, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			maxBytes:    100,
			wantRotated: true,
		},
		{
			name: "under threshold untouched",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				p := filepath.Join(dir, "daemon-error.log")
				if err := os.WriteFile(p, []byte("small"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			maxBytes:    100,
			wantRotated: false,
		},
		{
			name: "exactly at threshold untouched",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				p := filepath.Join(dir, "daemon-error.log")
				if err := os.WriteFile(p, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			maxBytes:    100,
			wantRotated: false,
		},
		{
			name: "missing file is a no-op",
			setup: func(_ *testing.T, dir string) string {
				return filepath.Join(dir, "nope.log")
			},
			maxBytes:    100,
			wantRotated: false,
		},
		{
			name:        "empty path is a no-op",
			setup:       func(_ *testing.T, _ string) string { return "" },
			maxBytes:    100,
			wantRotated: false,
		},
		{
			name: "non-positive max is a no-op",
			setup: func(t *testing.T, dir string) string {
				t.Helper()
				p := filepath.Join(dir, "daemon.log")
				if err := os.WriteFile(p, []byte(strings.Repeat("x", 200)), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			maxBytes:    0,
			wantRotated: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := tc.setup(t, dir)

			var before []byte
			if path != "" {
				before, _ = os.ReadFile(path) //nolint:gosec // test-owned path
			}

			rotated, err := RotateLogIfOver(path, tc.maxBytes)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if rotated != tc.wantRotated {
				t.Fatalf("rotated = %v, want %v", rotated, tc.wantRotated)
			}

			if !tc.wantRotated {
				// Live file (when present) must be byte-identical, and no
				// .1 generation may appear.
				if path != "" {
					after, _ := os.ReadFile(path) //nolint:gosec // test-owned path
					if string(after) != string(before) {
						t.Error("file modified without rotation")
					}
					if _, err := os.Stat(path + ".1"); err == nil {
						t.Error("rotated generation created without rotation")
					}
				}
				return
			}

			// Rotated: contents moved to .1, live file truncated to zero.
			gen, err := os.ReadFile(path + ".1") //nolint:gosec // test-owned path
			if err != nil {
				t.Fatalf("read rotated generation: %v", err)
			}
			if string(gen) != string(before) {
				t.Errorf("rotated generation lost content: got %d bytes, want %d", len(gen), len(before))
			}
			live, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat live file: %v", err)
			}
			if live.Size() != 0 {
				t.Errorf("live file not truncated: %d bytes", live.Size())
			}
		})
	}
}

// TestRotateLogIfOver_ReplacesPreviousGeneration pins single-generation
// retention: a second rotation overwrites the previous .1 file rather than
// accumulating .2/.3/… forever.
func TestRotateLogIfOver_ReplacesPreviousGeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	if err := os.WriteFile(path, []byte(strings.Repeat("a", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rotated, err := RotateLogIfOver(path, 100); err != nil || !rotated {
		t.Fatalf("first rotation: rotated=%v err=%v", rotated, err)
	}

	if err := os.WriteFile(path, []byte(strings.Repeat("b", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	if rotated, err := RotateLogIfOver(path, 100); err != nil || !rotated {
		t.Fatalf("second rotation: rotated=%v err=%v", rotated, err)
	}

	gen, err := os.ReadFile(path + ".1") //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(gen), "b") || len(gen) != 300 {
		t.Errorf("second rotation must replace the generation: got %d bytes starting %q", len(gen), gen[:1])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected exactly live + one generation, got %d entries", len(entries))
	}
}
