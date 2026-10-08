package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/kit"
)

const dependencyStoreManifestTOML = `api = "donmai.dev/v2"

[kit]
id = "node/pnpm"
version = "1.0.0"
name = "pnpm Node"
priority = 60

[supports]
os = ["linux", "macos"]

[detect]
files = ["pnpm-lock.yaml"]

[[provide.dependency_store]]
manager = "pnpm"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
inputs = ["pnpm-workspace.yaml"]
version = "pnpm --version"

[provide.dependency_store.store]
env = ["NPM_CONFIG_STORE_DIR"]
sharing = "content-addressed"
integrity = "on-use"
content = ["v10/files", "v10/index"]
bookkeeping = ["v10/projects"]
records_path = true

[provide.dependency_store.import]
env = { NPM_CONFIG_PACKAGE_IMPORT_METHOD = "clone-or-copy" }

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"
online = "pnpm install --prefer-offline --frozen-lockfile"

[provide.dependency_store.snapshot]
installed = ["node_modules"]
abi = ["os", "arch"]
relocation = "reconcile"
runs_package_code = true

[composition]
order = "foundation"
`

const goDependencyStoreManifestTOML = `api = "donmai.dev/v2"

[kit]
id = "golang/modules"
version = "1.0.0"
name = "Go modules"
priority = 60

[supports]
os = ["linux", "macos"]

[detect]
files = ["go.sum"]

[[provide.dependency_store]]
manager = "gomod"
ecosystem = "go"
lockfiles = ["go.sum"]
inputs = ["go.mod"]
version = "go version"

[provide.dependency_store.store]
env = ["GOMODCACHE"]
sharing = "content-addressed"
integrity = "on-fetch"
content = ["cache/download"]

[provide.dependency_store.commands]
fetch = "go mod download"
install = "go mod download"
verify = "go mod verify"

[composition]
order = "foundation"
`

func writeV2Kit(t *testing.T, dir, name, body string) {
	t.Helper()
	writeManifest(t, dir, name, body)
}

func TestKitRegistry_LegacyScanRoundTripDependencyStores(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeV2Kit(t, scanDir, "pnpm", dependencyStoreManifestTOML)
	writeV2Kit(t, scanDir, "gomod", goDependencyStoreManifestTOML)

	reg := NewKitRegistry([]string{scanDir})
	listed := reg.List()
	if len(listed) != 2 {
		t.Fatalf("List = %d kits, want 2", len(listed))
	}
	for _, summary := range listed {
		if !summary.ProvidesDependencyStores {
			t.Fatalf("kit %s missing dependency-stores summary flag", summary.ID)
		}
	}

	manifest, err := reg.Get("node/pnpm")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(manifest.DependencyStores) != 1 {
		t.Fatalf("stores = %d, want 1", len(manifest.DependencyStores))
	}
	store := manifest.DependencyStores[0]
	if store.Manager != "pnpm" || store.Ecosystem != "node" {
		t.Fatalf("store identity = %+v, want pnpm/node", store)
	}
	if len(store.Lockfiles) != 1 || store.Lockfiles[0] != "pnpm-lock.yaml" {
		t.Fatalf("lockfiles = %v, want [pnpm-lock.yaml]", store.Lockfiles)
	}
	if store.Store.Sharing != "content-addressed" || store.Store.Integrity != "on-use" {
		t.Fatalf("store policy = %+v, want content-addressed/on-use", store.Store)
	}
	if store.Commands["fetch"] != "pnpm fetch" || store.Commands["install"] != "pnpm install --offline --frozen-lockfile" {
		t.Fatalf("commands = %v, want fetch/install", store.Commands)
	}
	if !store.Snapshot.RunsPackageCode || store.Snapshot.Relocation != "reconcile" {
		t.Fatalf("snapshot = %+v, want reconcile + package code", store.Snapshot)
	}
	if store.ImportEnv["NPM_CONFIG_PACKAGE_IMPORT_METHOD"] != "clone-or-copy" {
		t.Fatalf("import env = %v, want clone-or-copy", store.ImportEnv)
	}

	// Round-trip: re-encode the envelope view and confirm the entry
	// survives the scan → wire → view path with every validated field.
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, err := reg.DetectForRepo(repo, kit.OSLinux)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(views) != 1 || len(views[0].DependencyStores) != 1 {
		t.Fatalf("detected views = %+v, want one kit with one store", views)
	}
	demand, err := kit.ComposeForTarget(views, kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."}, nil, nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(demand.DependencyStores) != 1 || demand.DependencyStores[0].Manager != "pnpm" {
		t.Fatalf("composed stores = %+v, want pnpm", demand.DependencyStores)
	}
	selected, err := kit.SelectDependencyStores(demand.DependencyStores, kit.LeafDependencyFacts{
		Files: []string{"pnpm-lock.yaml"},
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 || selected[0].Commands["fetch"] != "pnpm fetch" {
		t.Fatalf("selected = %+v, want pnpm fetch", selected)
	}
}

func TestKitRegistry_ScanRejectsV1ManifestWithDependencyStores(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	v1WithStores := strings.Replace(dependencyStoreManifestTOML, `api = "donmai.dev/v2"`, `api = "donmai.dev/v1"`, 1)
	writeV2Kit(t, scanDir, "pnpm", v1WithStores)

	reg := NewKitRegistry([]string{scanDir})
	if listed := reg.List(); len(listed) != 0 {
		t.Fatalf("List = %+v, want empty: v1+stores must be excluded, not half-applied", listed)
	}
	if _, err := reg.Get("node/pnpm"); !errors.Is(err, ErrKitNotFound) {
		t.Fatalf("Get error = %v, want ErrKitNotFound", err)
	}
}

func TestKitRegistry_ScanRejectsUnknownAPIRevision(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	unknown := strings.Replace(dependencyStoreManifestTOML, `api = "donmai.dev/v2"`, `api = "donmai.dev/v9"`, 1)
	writeV2Kit(t, scanDir, "pnpm", unknown)

	reg := NewKitRegistry([]string{scanDir})
	if listed := reg.List(); len(listed) != 0 {
		t.Fatalf("List = %+v, want empty: unknown revision must be rejected", listed)
	}
}

func TestKitRegistry_ScanRejectsInvalidDependencyStore(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	invalid := strings.Replace(dependencyStoreManifestTOML, `integrity = "on-use"`, `integrity = "none"`, 1)
	writeV2Kit(t, scanDir, "pnpm", invalid)

	reg := NewKitRegistry([]string{scanDir})
	if listed := reg.List(); len(listed) != 0 {
		t.Fatalf("List = %+v, want empty: integrity none without session-only must be excluded", listed)
	}
}

func TestKitRegistry_ComposeForRepoStoreConflictFailsClosed(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	second := strings.Replace(dependencyStoreManifestTOML, `id = "node/pnpm"`, `id = "node/pnpm-alt"`, 1)
	second = strings.Replace(second, `order = "foundation"`, `order = "framework"`, 1)
	first := strings.Replace(dependencyStoreManifestTOML, `order = "foundation"`, `order = "framework"`, 1)
	writeV2Kit(t, scanDir, "pnpm", first)
	writeV2Kit(t, scanDir, "pnpm-alt", second)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewKitRegistry([]string{scanDir})
	target := kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."}
	if _, err := reg.ComposeForRepo(repo, target, nil); !errors.Is(err, kit.ErrDependencyStoreConflict) {
		t.Fatalf("ComposeForRepo error = %v, want ErrDependencyStoreConflict", err)
	}

	views, err := reg.DetectForRepo(repo, kit.OSLinux)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("detected = %d kits, want 2", len(views))
	}
	lockRaw, err := kit.CanonicalCompositionLock(kit.CompositionLock{
		Targets: []kit.LockedCompositionTarget{{
			Target: target,
			Dependencies: []kit.LockedDependencyBinding{
				{Manager: "pnpm", SelectedKitID: "node/pnpm-alt"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("canonical lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scanDir, ".composition.lock.json"), lockRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	demand, err := reg.ComposeForRepo(repo, target, nil)
	if err != nil {
		t.Fatalf("locked ComposeForRepo: %v", err)
	}
	if len(demand.DependencyStores) != 1 || demand.DependencyStores[0].KitID != "node/pnpm-alt" {
		t.Fatalf("locked stores = %+v, want pnpm-alt", demand.DependencyStores)
	}
}

func TestKitRegistry_LegacyInstallRejectsV1ManifestWithStores(t *testing.T) {
	t.Parallel()
	v1WithStores := strings.Replace(dependencyStoreManifestTOML, `api = "donmai.dev/v2"`, `api = "donmai.dev/v1"`, 1)
	repoURL := newLocalGitFixture(t, fixtureFile{name: "node-pnpm.kit.toml", body: v1WithStores})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	_, err := r.Install("node/pnpm", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitInstallManifestNotFound) {
		t.Fatalf("Install error = %v, want ErrKitInstallManifestNotFound", err)
	}
	if listed := r.List(); len(listed) != 0 {
		t.Fatalf("List after rejected install = %+v, want empty", listed)
	}
}

func TestKitPackageV2StoreRelocateRequiresInventory(t *testing.T) {
	t.Parallel()
	// Positive: a v2 manifest whose relocate names an inventoried
	// payload verifies through the production package path.
	root := copyReleasedFixture(t, "go")
	relocateBody := []byte("#!/bin/sh\nexec pnpm-relocate \"$@\"\n")
	mustWrite(t, filepath.Join(root, "bin", "relocate"), relocateBody, 0o755)
	rewriteFixtureAsV2WithRelocate(t, root, "bin/relocate")
	registry := NewKitRegistry([]string{t.TempDir()})
	verified, err := registry.verifyKitPackage(root, kitPackageDescriptorName, "default/go", "1.0.0", "", nil)
	if err != nil {
		t.Fatalf("verify v2 package with inventoried relocate: %v", err)
	}
	if len(verified.Manifest.Provide.DependencyStores) != 1 {
		t.Fatalf("verified stores = %d, want 1", len(verified.Manifest.Provide.DependencyStores))
	}
	if verified.Signature.Trust != afclient.KitTrustUnsigned {
		t.Fatalf("trust = %q, want unsigned for the unsigned rebuilt fixture", verified.Signature.Trust)
	}

	// Negative: the same manifest naming a payload outside the
	// inventory fails closed through the same entry point.
	badRoot := copyReleasedFixture(t, "go")
	rewriteFixtureAsV2WithRelocate(t, badRoot, "bin/missing-relocate")
	badRegistry := NewKitRegistry([]string{t.TempDir()})
	if _, err := badRegistry.verifyKitPackage(badRoot, kitPackageDescriptorName, "default/go", "1.0.0", "", nil); !errors.Is(err, ErrKitPackageInvalid) {
		t.Fatalf("missing relocate error = %v, want ErrKitPackageInvalid", err)
	}
}

// rewriteFixtureAsV2WithRelocate rewrites the copied fixture's kit.toml
// as a v2 manifest declaring a gomod store whose relocate names
// relocateRef, then rebuilds the canonical descriptor so payload hashes
// match. The rebuilt descriptor is unsigned by construction; callers use
// a permissive registry so verification exercises the inventory and
// reference checks rather than the signature gate.
func rewriteFixtureAsV2WithRelocate(t *testing.T, root, relocateRef string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "kit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(string(raw), `api = "rensei.dev/v1"`, `api = "donmai.dev/v2"`, 1)
	manifest += "\n[[provide.dependency_store]]\n" +
		"manager = \"gomod\"\n" +
		"ecosystem = \"go\"\n" +
		"lockfiles = [\"go.sum\"]\n" +
		"inputs = [\"go.mod\"]\n" +
		"version = \"go version\"\n" +
		"\n[provide.dependency_store.store]\n" +
		"env = [\"GOMODCACHE\"]\n" +
		"sharing = \"content-addressed\"\n" +
		"integrity = \"on-fetch\"\n" +
		"content = [\"cache/download\"]\n" +
		"\n[provide.dependency_store.commands]\n" +
		"fetch = \"go mod download\"\n" +
		"install = \"go mod download\"\n" +
		"verify = \"go mod verify\"\n" +
		"relocate = \"" + relocateRef + "\"\n"
	mustWrite(t, filepath.Join(root, "kit.toml"), []byte(manifest), 0o644)
	// Drop the released signature envelopes before rebuilding: the
	// rebuilt descriptor is unsigned by construction and neither
	// envelope may remain in the inventory.
	mustRemove(t, filepath.Join(root, kitPackageSignatureName))
	mustRemove(t, filepath.Join(root, "kit.toml.sigstore"))
	rebuildFixtureDescriptor(t, root)
}

// rebuildFixtureDescriptor recomputes the canonical descriptor for every
// regular file under root (excluding the descriptor and its signature),
// mirroring the publisher's packaging step.
func rebuildFixtureDescriptor(t *testing.T, root string) {
	t.Helper()
	var entries []kitPackageEntry
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || name == root {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == kitPackageDescriptorName || rel == kitPackageSignatureName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("fixture has non-regular file %s", rel)
		}
		data, err := os.ReadFile(name) //nolint:gosec // test fixture under t.TempDir
		if err != nil {
			return err
		}
		mode := "0644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "0755"
		}
		entries = append(entries, kitPackageEntry{
			Mode:   mode,
			Path:   rel,
			SHA256: sha256Hex(data),
			Size:   int64(len(data)),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	descriptor := kitPackageDescriptor{
		Entries:   entries,
		Kit:       kitPackageID{ID: "default/go", Version: "1.0.0"},
		Manifest:  "kit.toml",
		Publisher: "did:web:donmai.dev",
		Schema:    kitPackageSchema,
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, kitPackageDescriptorName), canonical, 0o644)
}

func TestKitRegistry_LegacyInstallAcceptsV2Manifest(t *testing.T) {
	t.Parallel()
	repoURL := newLocalGitFixture(t, fixtureFile{name: "node-pnpm.kit.toml", body: dependencyStoreManifestTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	res, err := r.Install("node/pnpm", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if err != nil {
		t.Fatalf("Install v2: %v", err)
	}
	if !res.Kit.ProvidesDependencyStores {
		t.Fatal("installed kit missing dependency-stores summary flag")
	}
	manifest, err := r.Get("node/pnpm")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(manifest.DependencyStores) != 1 || manifest.DependencyStores[0].Manager != "pnpm" {
		t.Fatalf("installed stores = %+v, want pnpm", manifest.DependencyStores)
	}
}
