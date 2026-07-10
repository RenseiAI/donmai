package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/kit"
)

const nextCommandManifestTOML = `
api = "donmai.dev/v1"
[kit]
id = "ts-nextjs"
version = "1.0.0"
name = "Next.js"
priority = 40
[supports]
os = ["linux", "macos"]
[detect]
files = ["next.config.ts"]
files_all = ["package.json"]
[provide.commands]
build = "pnpm build"
test = "pnpm test"
validate = "pnpm typecheck"
[provide.commands_override.macos]
build = "pnpm build:mac"
[composition]
order = "framework"
`

func TestComposeForRepoTypescriptNextRequiresExactLock(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)
	writeManifest(t, scanDir, "ts-nextjs.kit.toml", nextCommandManifestTOML)
	repo := t.TempDir()
	for _, name := range []string{"package.json", "tsconfig.json", "next.config.ts"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewKitRegistry([]string{scanDir})
	target := kit.CompositionTarget{OS: kit.OSLinux, WorkType: "implementation", PathScope: "."}
	_, err := reg.ComposeForRepo(repo, target, nil)
	if !errors.Is(err, kit.ErrCommandCompositionConflict) {
		t.Fatalf("unlocked composition error = %v", err)
	}
	for _, want := range []string{"typescript", "ts-nextjs", "build", "operator lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}

	views, err := reg.DetectForRepo(repo, kit.OSLinux)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	identities := map[string]kit.CommandIdentity{}
	for _, view := range views {
		identity := kit.CommandIdentity{KitID: view.ID, DigestKind: "package", Digest: view.PackageDigest}
		if identity.Digest == "" {
			identity.DigestKind = "legacy-manifest"
			identity.Digest = view.LegacyManifestDigest
		}
		identities[view.ID] = identity
	}
	var bindings []kit.LockedCommandBinding
	for _, alias := range []string{"build", "test"} {
		bindings = append(bindings, kit.LockedCommandBinding{
			Alias: alias, PathScope: ".",
			Selected: kit.CommandIdentity{KitID: "ts-nextjs", Name: alias, DigestKind: identities["ts-nextjs"].DigestKind, Digest: identities["ts-nextjs"].Digest},
		})
	}
	// TypeScript alone owns validate in this fixture, so no lock row is needed.
	raw, err := kit.CanonicalCompositionLock(kit.CompositionLock{Targets: []kit.LockedCompositionTarget{{Target: target, Bindings: bindings}}})
	if err != nil {
		t.Fatalf("canonical lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scanDir, kitCompositionLockName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	demand, err := reg.ComposeForRepo(repo, target, nil)
	if err != nil {
		t.Fatalf("locked composition: %v", err)
	}
	if len(demand.Commands) != 5 || len(demand.CommandBindings) != 3 || len(demand.CompositionDigest) != 64 {
		t.Fatalf("demand commands/bindings/digest = %d/%d/%q", len(demand.Commands), len(demand.CommandBindings), demand.CompositionDigest)
	}
	for _, binding := range demand.CommandBindings {
		if binding.Alias != "validate" && binding.Selected.KitID != "ts-nextjs" {
			t.Fatalf("binding %+v did not select ts-nextjs", binding)
		}
	}
}

func TestLoadCompositionLockRejectsNonCanonicalBytes(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, kitCompositionLockName), []byte("{ \"schema\": \"donmai.dev/kit-composition-lock/v1\", \"targets\": [] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewKitRegistry([]string{repo}).loadCompositionLock()
	if !errors.Is(err, kit.ErrCompositionLockInvalid) {
		t.Fatalf("noncanonical lock error = %v", err)
	}
}

func TestSelectExactKitViews(t *testing.T) {
	t.Parallel()
	views := []kit.ManifestView{{ID: "go", Version: "1.0.0"}, {ID: "go", Version: "2.0.0"}, {ID: "node", Version: "1.0.0"}}
	selected, err := selectExactKitViews(views, []kit.Selection{{ID: "go", Version: "2.0.0"}})
	if err != nil || len(selected) != 1 || selected[0].Version != "2.0.0" {
		t.Fatalf("selected = %+v, err=%v", selected, err)
	}
	for name, refs := range map[string][]kit.Selection{
		"missing":   {{ID: "go", Version: "3.0.0"}},
		"duplicate": {{ID: "go", Version: "1.0.0"}, {ID: "go", Version: "1.0.0"}},
		"non-exact": {{ID: "go"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := selectExactKitViews(views, refs); err == nil {
				t.Fatalf("refs %+v unexpectedly accepted", refs)
			}
		})
	}
}

func TestComposeForRepoExactPlatformSelectionBypassesDetectPredicates(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)
	repo := t.TempDir() // Deliberately has no package.json or tsconfig.json.
	reg := NewKitRegistry([]string{scanDir})

	detected, err := reg.DetectForRepo(repo, kit.OSLinux)
	if err != nil || len(detected) != 0 {
		t.Fatalf("detection should not match: views=%+v err=%v", detected, err)
	}
	demand, err := reg.ComposeForRepo(repo, kit.CompositionTarget{OS: kit.OSLinux, WorkType: "implementation", PathScope: "."}, []kit.Selection{{ID: "typescript", Version: "1.0.0"}})
	if err != nil {
		t.Fatalf("exact selected composition: %v", err)
	}
	if len(demand.Commands) != 2 || demand.Kits[0] != "typescript@1.0.0" || len(demand.CompositionDigest) != 64 {
		t.Fatalf("exact selected demand = %+v", demand)
	}
	_, err = reg.ComposeForRepo(repo, kit.CompositionTarget{OS: kit.OSWindows, PathScope: "."}, []kit.Selection{{ID: "typescript", Version: "1.0.0"}})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("OS-incompatible exact selection error = %v", err)
	}
}

func TestReleasedTypescriptAndNextPackagesConflictWithoutOwnerLock(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	for _, name := range []string{"package.json", "tsconfig.json", "next.config.ts"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var views []kit.ManifestView
	for _, name := range []string{"typescript", "ts-nextjs"} {
		root := filepath.Join("testdata", "released-kit-packages", name)
		manifestPath := filepath.Join(root, "kit.toml")
		manifest, err := loadKitManifestFile(manifestPath)
		if err != nil {
			t.Fatalf("load released %s manifest: %v", name, err)
		}
		if !detectMatches(manifest, repo) {
			t.Fatalf("released %s package did not detect against Next fixture", name)
		}
		descriptor, err := os.ReadFile(filepath.Join(root, kitPackageDescriptorName))
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, manifestToView(manifest, sha256Hex(descriptor), ""))
	}
	views = kit.SortManifests(views)
	_, err := kit.ComposeForTarget(views, kit.CompositionTarget{OS: kit.OSLinux, WorkType: "implementation", PathScope: "."}, nil, nil)
	if !errors.Is(err, kit.ErrCommandCompositionConflict) {
		t.Fatalf("released package collision = %v", err)
	}
	for _, want := range []string{"default/typescript", "default/ts-nextjs", "build"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("released collision %q missing %q", err, want)
		}
	}
}
