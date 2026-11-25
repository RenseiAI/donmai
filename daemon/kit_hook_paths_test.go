package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/kit"
)

// TestKitRegistry_InstalledPackageHookResolvesInsidePackage installs the
// released Go kit package, whose post_acquire is the relative path bin/setup.sh,
// and checks the composed command is that file inside the installed package,
// not the repository's same-named file.
func TestKitRegistry_InstalledPackageHookResolvesInsidePackage(t *testing.T) {
	t.Parallel()
	root := copyReleasedFixture(t, "go")
	registry := NewKitRegistryWithTrust([]string{t.TempDir()}, TrustConfig{Mode: TrustModePermissive})
	registry.fetcher = &packageFixtureFetcher{roots: map[string]string{"default/go": root}}
	result, err := registry.Install("default/go", afclient.KitInstallRequest{
		Version: "1.0.0",
		Source:  &afclient.KitInstallSource{Kind: "git", URL: "fixture"},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "bin", "setup.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755)

	demand, err := registry.ComposeForRepo(repo, kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."},
		[]kit.Selection{{ID: "default/go", Version: "1.0.0"}})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	pkg := filepath.Join(registry.packageStoreRoot(), "packages", "sha256", result.Kit.PackageDigest)
	pkg, err = filepath.EvalSymlinks(pkg)
	if err != nil {
		t.Fatal(err)
	}
	want := "'" + filepath.Join(pkg, "bin", "setup.sh") + "'"
	if len(demand.PostAcquire) != 1 || demand.PostAcquire[0] != want {
		t.Fatalf("PostAcquire = %q, want [%s]", demand.PostAcquire, want)
	}
	if strings.Contains(demand.PostAcquire[0], repo) {
		t.Fatalf("hook resolved into the repository: %q", demand.PostAcquire[0])
	}
}

// TestKitRegistry_FlatManifestRelativeHookIsRefused pins that a flat manifest
// (no verified package root) whose hook is a relative path is refused at
// composition, while inline shell text still composes.
func TestKitRegistry_FlatManifestRelativeHookIsRefused(t *testing.T) {
	t.Parallel()
	view := kit.ManifestView{ID: "flat/kit", Version: "1.0.0", Hooks: kit.HooksView{PostAcquire: "bin/setup.sh"}}
	_, err := kit.Compose([]kit.ManifestView{view}, kit.OSLinux)
	if !errors.Is(err, kit.ErrHookPathInvalid) {
		t.Fatalf("err = %v, want ErrHookPathInvalid", err)
	}
	view.Hooks.PostAcquire = "npm ci"
	if _, err := kit.Compose([]kit.ManifestView{view}, kit.OSLinux); err != nil {
		t.Fatalf("inline hook refused: %v", err)
	}
}
