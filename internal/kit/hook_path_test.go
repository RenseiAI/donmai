package kit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shExecer runs each command with `sh -c` in dir, like the runner's execer,
// and exposes MARKER_DIR so hook scripts can record which of them ran.
type shExecer struct{ markerDir string }

func (e shExecer) Exec(ctx context.Context, dir, command string, _ map[string]string) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // test execer
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MARKER_DIR="+e.markerDir)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
}

func hookView(root, post, pre string) ManifestView {
	return ManifestView{
		ID: "test/kit", Version: "1.0.0", PackageRoot: root,
		Hooks: HooksView{PostAcquire: post, PreRelease: pre},
	}
}

// TestKitHookRunsKitFileNotRepositoryFile is the D5 RED: the repository
// carries its own bin/setup.sh, the kit's hook names bin/setup.sh, and the
// kit's file must be the one that runs.
func TestKitHookRunsKitFileNotRepositoryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh hooks")
	}
	root, repo, marks := t.TempDir(), t.TempDir(), t.TempDir()
	writeScript(t, filepath.Join(root, "bin", "setup.sh"), `printf kit > "$MARKER_DIR/who"`)
	writeScript(t, filepath.Join(repo, "bin", "setup.sh"), `printf repo > "$MARKER_DIR/who"`)

	d, err := Compose([]ManifestView{hookView(root, "bin/setup.sh", "")}, OSLinux)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err := (&Provisioner{}).Provision(context.Background(), shExecer{marks}, repo, d); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(marks, "who"))
	if err != nil {
		t.Fatalf("no hook ran: %v", err)
	}
	if string(got) != "kit" {
		t.Fatalf("the %s script ran, want the kit's", got)
	}
}

// TestKitHookNeedsNoRepositoryFile pins the other half: a repository with no
// bin/setup.sh no longer fails provisioning.
func TestKitHookNeedsNoRepositoryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh hooks")
	}
	root, repo, marks := t.TempDir(), t.TempDir(), t.TempDir()
	writeScript(t, filepath.Join(root, "bin", "setup.sh"), `printf kit > "$MARKER_DIR/who"`)
	d, err := Compose([]ManifestView{hookView(root, "bin/setup.sh", "")}, OSLinux)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Provisioner{}).Provision(context.Background(), shExecer{marks}, repo, d); err != nil {
		t.Fatalf("Provision: %v", err)
	}
}

func TestKitHookPathResolution(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeScript(t, filepath.Join(root, "bin", "setup.sh"), "true")
	writeScript(t, filepath.Join(root, "setup"), "true")
	writeScript(t, filepath.Join(outside, "evil.sh"), "true")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.sh"), filepath.Join(root, "bin", "evil.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}

	refused := []struct{ name, hook, root string }{
		{"parent escape", "../evil.sh", root},
		{"nested parent escape", "bin/../../evil.sh", root},
		{"bare parent", "..", root},
		{"absolute path", "/usr/local/bin/setup.sh", root},
		{"absolute path into package", filepath.Join(root, "bin", "setup.sh"), root},
		{"symlinked directory escape", "link/evil.sh", root},
		{"symlinked file escape", "bin/evil.sh", root},
		{"missing file", "bin/missing.sh", root},
		{"directory", "dir/x", root},
		{"rootless manifest", "bin/setup.sh", ""},
		{"backslash separator off windows", `bin\setup.cmd`, root},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			for _, view := range []ManifestView{hookView(tc.root, tc.hook, ""), hookView(tc.root, "", tc.hook)} {
				_, err := Compose([]ManifestView{view}, OSLinux)
				if !errors.Is(err, ErrHookPathInvalid) || !errors.Is(err, ErrPackagePathInvalid) {
					t.Fatalf("Compose(%q) err = %v, want ErrHookPathInvalid wrapping ErrPackagePathInvalid", tc.hook, err)
				}
			}
		})
	}

	inline := []string{"make setup", "bin/setup.sh --fast", "npm ci", "setup", "echo hi && true", "./bin/x; y", "$HOME/bin/x"}
	for _, hook := range inline {
		t.Run("inline/"+hook, func(t *testing.T) {
			// Rootless and rooted manifests both keep inline text verbatim.
			for _, r := range []string{"", root} {
				d, err := Compose([]ManifestView{hookView(r, hook, hook)}, OSLinux)
				if err != nil {
					t.Fatalf("Compose: %v", err)
				}
				if len(d.PostAcquire) != 1 || d.PostAcquire[0] != hook || len(d.PreRelease) != 1 || d.PreRelease[0] != hook {
					t.Fatalf("inline hook changed: %+v", d)
				}
			}
		})
	}

	t.Run("resolved path is quoted absolute inside root", func(t *testing.T) {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, hook := range []string{"bin/setup.sh", "./bin/setup.sh"} {
			d, err := Compose([]ManifestView{hookView(root, hook, hook)}, OSLinux)
			if err != nil {
				t.Fatalf("Compose(%q): %v", hook, err)
			}
			want := "'" + filepath.Join(resolvedRoot, "bin", "setup.sh") + "'"
			if d.PostAcquire[0] != want || d.PreRelease[0] != want {
				t.Fatalf("hook %q resolved to %q / %q, want %q", hook, d.PostAcquire[0], d.PreRelease[0], want)
			}
		}
	})

	t.Run("OS-keyed hook resolves too", func(t *testing.T) {
		v := hookView(root, "", "")
		v.Hooks.OS = map[string]HookOSView{OSLinux: {PostAcquire: "../escape.sh"}}
		if _, err := Compose([]ManifestView{v}, OSLinux); !errors.Is(err, ErrHookPathInvalid) {
			t.Fatalf("err = %v, want ErrHookPathInvalid", err)
		}
		// A hook for another OS is never composed, so never refused.
		if _, err := Compose([]ManifestView{v}, OSMacOS); err != nil {
			t.Fatalf("hook for another OS refused: %v", err)
		}
	})

	t.Run("error names kit and hook", func(t *testing.T) {
		_, err := Compose([]ManifestView{hookView("", "bin/setup.sh", "")}, OSLinux)
		if err == nil || !strings.Contains(err.Error(), "test/kit") || !strings.Contains(err.Error(), "bin/setup.sh") {
			t.Fatalf("err = %v", err)
		}
	})
}
