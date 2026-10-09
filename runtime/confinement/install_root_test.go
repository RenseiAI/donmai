package confinement

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"npm":       "prefix/lib/node_modules/@scope/agent/dist/cli.js",
		"store":     "global/node_modules/.pnpm/agent@1/node_modules/agent/dist/cli.js",
		"nested":    "prefix/lib/node_modules/agent/node_modules/dep/bin/x.js",
		"native":    "opt/bin/agent",
		"linkedbin": "prefix/lib/node_modules/@scope/agent/bin/agent.js",
	}
	for _, rel := range files {
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "prefix", "bin", "agent")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, files["linkedbin"]), link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, binary, want string
	}{
		{"scoped npm package", filepath.Join(base, files["npm"]), filepath.Join(base, "prefix/lib/node_modules")},
		{"virtual store", filepath.Join(base, files["store"]), filepath.Join(base, "global/node_modules")},
		{"nested dependency", filepath.Join(base, files["nested"]), filepath.Join(base, "prefix/lib/node_modules")},
		{"native binary", filepath.Join(base, files["native"]), filepath.Join(base, "opt/bin")},
		{"link on PATH into a package", link, filepath.Join(base, "prefix/lib/node_modules")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InstallRoot(tt.binary)
			if err != nil || got != tt.want {
				t.Fatalf("InstallRoot = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := InstallRoot(filepath.Join(base, "absent")); err == nil {
		t.Fatal("InstallRoot of a missing binary succeeded")
	}
}
