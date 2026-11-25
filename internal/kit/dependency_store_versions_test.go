package kit

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// pnpm9StoreView is the pnpm 9 entry: store layout v3, versions >=9 <10.
func pnpm9StoreView() DependencyStoreView {
	v := pnpmStoreView()
	v.Versions = ">=9 <10"
	v.Store.Content = []string{"v3/files", "v3/index"}
	v.Store.Bookkeeping = []string{"v3/projects"}
	v.NoCode = &DependencyStoreNoCode{Env: map[string]string{
		"npm_config_ignore_scripts": "true", "npm_config_ignore_pnpmfile": "true",
	}}
	v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_USERCONFIG"}
	return v
}

// pnpm10StoreView is the pnpm 10 entry: store layout v10, versions >=10 <11.
func pnpm10StoreView() DependencyStoreView {
	v := pnpmStoreView()
	v.Versions = ">=10 <11"
	v.NoCode = &DependencyStoreNoCode{Env: map[string]string{
		"npm_config_ignore_scripts": "true", "npm_config_ignore_pnpmfile": "true",
	}}
	v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_USERCONFIG"}
	return v
}

func TestValidateDependencyStoreVersionsNoCodeCredentialsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*DependencyStoreView)
		wantErr error  // nil: must validate
		wantMsg string // substring of the message
	}{
		{"no new keys", func(*DependencyStoreView) {}, nil, ""},
		{"valid range", func(v *DependencyStoreView) { v.Versions = ">=9 <10" }, nil, ""},
		{"union range", func(v *DependencyStoreView) { v.Versions = ">=8 <9 || >=10 <11" }, nil, ""},
		{"range is not semver", func(v *DependencyStoreView) { v.Versions = "latest" }, ErrDependencyStoreVersions, "versions"},
		{"empty range set", func(v *DependencyStoreView) { v.Versions = ">=10 <9" }, ErrDependencyStoreVersions, "matches no version"},
		{"prerelease bound", func(v *DependencyStoreView) { v.Versions = ">=10.0.0-rc.1" }, ErrDependencyStoreVersions, "prerelease"},
		{"no_code declared empty", func(v *DependencyStoreView) { v.NoCode = &DependencyStoreNoCode{} }, nil, ""},
		{"no_code env ok", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"npm_config_ignore_scripts": "true"}}
		}, nil, ""},
		{"no_code bad env name", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"1BAD": "x"}}
		}, ErrDependencyStoreInvalid, "no_code.env key"},
		{"no_code env name with dash", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"A-B": "x"}}
		}, ErrDependencyStoreInvalid, "no_code.env key"},
		{"no_code sets NODE_OPTIONS", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"NODE_OPTIONS": "--x"}}
		}, ErrDependencyStoreInvalid, "NODE_OPTIONS"},
		{"no_code sets DYLD_", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"DYLD_INSERT_LIBRARIES": "x"}}
		}, ErrDependencyStoreInvalid, "DYLD_INSERT_LIBRARIES"},
		{"no_code sets LD_", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"LD_PRELOAD": "x"}}
		}, ErrDependencyStoreInvalid, "LD_PRELOAD"},
		{"no_code NUL value", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"A": "x\x00y"}}
		}, ErrDependencyStoreInvalid, "NUL"},
		{"credentials ok", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_USERCONFIG"}
		}, nil, ""},
		{"credentials block without config_env", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{}
		}, ErrDependencyStoreInvalid, "config_env is required"},
		{"credentials bad env name", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "not valid"}
		}, ErrDependencyStoreInvalid, "config_env"},
		{"credentials LD_PRELOAD", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "LD_PRELOAD"}
		}, ErrDependencyStoreInvalid, "config_env"},
		{"credentials collides with store.env", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_STORE_DIR"}
		}, ErrDependencyStoreInvalid, "store.env"},
		{"credentials collides with import.env", func(v *DependencyStoreView) {
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_PACKAGE_IMPORT_METHOD"}
		}, ErrDependencyStoreInvalid, "import.env"},
		{"credentials collides with no_code.env", func(v *DependencyStoreView) {
			v.NoCode = &DependencyStoreNoCode{Env: map[string]string{"NPM_CONFIG_USERCONFIG": "x"}}
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_USERCONFIG"}
		}, ErrDependencyStoreInvalid, "no_code.env"},
		{"credentials collides with proxy.env", func(v *DependencyStoreView) {
			v.ProxyEnv = map[string]string{"NPM_CONFIG_USERCONFIG": "x"}
			v.Credentials = &DependencyStoreCredentials{ConfigEnv: "NPM_CONFIG_USERCONFIG"}
		}, ErrDependencyStoreInvalid, "proxy.env"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := pnpmStoreView()
			tc.mutate(&v)
			err := ValidateDependencyStore(v)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if !errors.Is(err, ErrDependencyStoreInvalid) {
				t.Fatalf("error = %v, want it to wrap ErrDependencyStoreInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %q, want substring %q", err, tc.wantMsg)
			}
		})
	}
}

// TestValidateDependencyStoresTwoPnpmDisjointRanges is the driving case: one
// kit declares pnpm for 9 and for 10 with disjoint ranges. The pre-amendment
// rule rejected any kit that named a manager twice.
func TestValidateDependencyStoresTwoPnpmDisjointRanges(t *testing.T) {
	t.Parallel()
	if err := ValidateDependencyStores([]DependencyStoreView{pnpm9StoreView(), pnpm10StoreView()}); err != nil {
		t.Fatalf("two pnpm entries with >=9 <10 and >=10 <11 must validate: %v", err)
	}
}

func TestValidateDependencyStoresRangeOverlapTable(t *testing.T) {
	t.Parallel()
	with := func(rng string) DependencyStoreView { v := pnpm10StoreView(); v.Versions = rng; return v }
	cases := []struct {
		name   string
		stores []DependencyStoreView
		ok     bool
	}{
		{"disjoint", []DependencyStoreView{with(">=9 <10"), with(">=10 <11")}, true},
		{"overlapping", []DependencyStoreView{with(">=9 <10"), with(">=9.5 <11")}, false},
		{"identical", []DependencyStoreView{with(">=10 <11"), with(">=10 <11")}, false},
		{"unranged beside ranged", []DependencyStoreView{with(""), with(">=10 <11")}, false},
		{"two unranged (the old duplicate rule)", []DependencyStoreView{with(""), with("")}, false},
		{"three disjoint", []DependencyStoreView{with("<9"), with(">=9 <10"), with(">=10")}, true},
		{"overlap is the third entry", []DependencyStoreView{with("<9"), with(">=9 <10"), with(">=9.9")}, false},
		{"different managers need no ranges", []DependencyStoreView{pnpmStoreView(), goStoreView()}, true},
	}
	for _, tc := range cases {
		err := ValidateDependencyStores(tc.stores)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrDependencyStoreRangeOverlap) || !errors.Is(err, ErrDependencyStoreInvalid) {
			t.Errorf("%s: error = %v, want ErrDependencyStoreRangeOverlap wrapping ErrDependencyStoreInvalid", tc.name, err)
		}
	}
}

func TestComposeDependencyStoresKeyedByManagerAndRange(t *testing.T) {
	t.Parallel()
	view := storeKitView("default/typescript", storeDigestA, pnpm10StoreView(), pnpm9StoreView())
	view.PackageRoot = "/packages/sha256/" + storeDigestA
	plan, err := ComposeDependencyStores([]ManifestView{view}, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(plan.Stores) != 2 {
		t.Fatalf("stores = %d, want one per (manager, range)", len(plan.Stores))
	}
	if plan.Stores[0].Versions != ">=10 <11" || plan.Stores[1].Versions != ">=9 <10" {
		t.Fatalf("order = %q, %q; want deterministic by range text", plan.Stores[0].Versions, plan.Stores[1].Versions)
	}
	for _, s := range plan.Stores {
		if s.Manager != "pnpm" || s.KitID != "default/typescript" || s.NoCode == nil || s.Credentials == nil {
			t.Fatalf("composed entry lost fields: %+v", s)
		}
		if s.Credentials.ConfigEnv != "NPM_CONFIG_USERCONFIG" || s.NoCode.Env["npm_config_ignore_scripts"] != "true" {
			t.Fatalf("composed no_code or credentials wrong: %+v", s)
		}
	}
	// Input order must not change the plan.
	swapped := storeKitView("default/typescript", storeDigestA, pnpm9StoreView(), pnpm10StoreView())
	swapped.PackageRoot = view.PackageRoot
	other, err := ComposeDependencyStores([]ManifestView{swapped}, storeTarget(), nil)
	if err != nil || other.Digest != plan.Digest {
		t.Fatalf("digest depends on declaration order: %v %v", err, other)
	}
}

func TestComposeDependencyStoresRejectsSameKitOverlap(t *testing.T) {
	t.Parallel()
	a, b := pnpm9StoreView(), pnpm10StoreView()
	b.Versions = ">=9.5 <11"
	_, err := ComposeDependencyStores([]ManifestView{storeKitView("k", storeDigestA, a, b)}, storeTarget(), nil)
	if !errors.Is(err, ErrDependencyStoreRangeOverlap) {
		t.Fatalf("error = %v, want ErrDependencyStoreRangeOverlap (a view need not have come through the parser)", err)
	}
}

func TestComposeDependencyStoresAcrossKits(t *testing.T) {
	t.Parallel()
	// Disjoint ranges from different kits compose; overlapping ones conflict.
	nine := storeKitView("kit/nine", storeDigestA, pnpm9StoreView())
	ten := storeKitView("kit/ten", storeDigestB, pnpm10StoreView())
	plan, err := ComposeDependencyStores([]ManifestView{nine, ten}, storeTarget(), nil)
	if err != nil || len(plan.Stores) != 2 {
		t.Fatalf("disjoint cross-kit compose = %v, %+v", err, plan)
	}

	wide := pnpm10StoreView()
	wide.Versions = ">=9.5 <11"
	over := storeKitView("kit/wide", storeDigestB, wide)
	_, err = ComposeDependencyStores([]ManifestView{nine, over}, storeTarget(), nil)
	if !errors.Is(err, ErrDependencyStoreConflict) {
		t.Fatalf("overlapping cross-kit error = %v, want ErrDependencyStoreConflict", err)
	}
	if !strings.Contains(err.Error(), "kit/nine") || !strings.Contains(err.Error(), "kit/wide") {
		t.Fatalf("conflict names no owners: %v", err)
	}

	// A kit with no versions overlaps any other kit's ranged entry.
	plain := storeKitView("kit/plain", storeDigestB, pnpmStoreView())
	if _, err = ComposeDependencyStores([]ManifestView{nine, plain}, storeTarget(), nil); !errors.Is(err, ErrDependencyStoreConflict) {
		t.Fatalf("unranged versus ranged error = %v, want ErrDependencyStoreConflict", err)
	}
}

func TestComposeDependencyStoresLockSelectsAmongOverlappingKits(t *testing.T) {
	t.Parallel()
	nine := storeKitView("kit/nine", storeDigestA, pnpm9StoreView())
	wide := pnpm10StoreView()
	wide.Versions = ">=9.5 <11"
	over := storeKitView("kit/wide", storeDigestB, wide)
	lock := &CompositionLock{Schema: CompositionLockSchema, Targets: []LockedCompositionTarget{{
		Target:       storeTarget(),
		Dependencies: []LockedDependencyBinding{dependencyPin("pnpm", "kit/nine", storeDigestA)},
	}}}
	plan, err := ComposeDependencyStores([]ManifestView{nine, over}, storeTarget(), lock)
	if err != nil {
		t.Fatalf("locked compose: %v", err)
	}
	if len(plan.Stores) != 1 || plan.Stores[0].KitID != "kit/nine" {
		t.Fatalf("lock must keep only the selected kit's entries: %+v", plan.Stores)
	}

	// A kit with several entries keeps all of them when the lock selects it.
	both := storeKitView("kit/both", storeDigestB, pnpm9StoreView(), pnpm10StoreView())
	lock.Targets[0].Dependencies[0] = dependencyPin("pnpm", "kit/both", storeDigestB)
	plan, err = ComposeDependencyStores([]ManifestView{nine, both}, storeTarget(), lock)
	if err != nil || len(plan.Stores) != 2 {
		t.Fatalf("locked multi-entry kit = %v, %+v", err, plan)
	}
}

func TestComposeDependencyStoresCarriesPackageRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "kits", "sha256", storeDigestA)
	withRoot := storeKitView("kit/a", storeDigestA, pnpm9StoreView())
	withRoot.PackageRoot = root
	legacy := storeKitView("kit/legacy", storeDigestB, goStoreView())
	plan, err := ComposeDependencyStores([]ManifestView{withRoot, legacy}, storeTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byKit := map[string]ComposedDependencyStore{}
	for _, s := range plan.Stores {
		byKit[s.KitID] = s
	}
	if byKit["kit/a"].PackageRoot != root {
		t.Fatalf("PackageRoot = %q, want %q", byKit["kit/a"].PackageRoot, root)
	}
	if byKit["kit/legacy"].PackageRoot != "" {
		t.Fatalf("legacy kit carries a package root %q", byKit["kit/legacy"].PackageRoot)
	}

	// The root is a host path: it must not move the composition digest.
	moved := withRoot
	moved.PackageRoot = filepath.Join(string(filepath.Separator), "elsewhere", storeDigestA)
	other, err := ComposeDependencyStores([]ManifestView{moved, legacy}, storeTarget(), nil)
	if err != nil || other.Digest != plan.Digest {
		t.Fatalf("package root changed the digest: %v", err)
	}
}

func TestComposedStoreResolvesRelocateAgainstPackageRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "kits", "pkg")
	entry := pnpm9StoreView()
	entry.Commands["relocate"] = "bin/relocate"
	view := storeKitView("kit/a", storeDigestA, entry)
	view.PackageRoot = root
	plan, err := ComposeDependencyStores([]ManifestView{view}, storeTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := plan.Stores[0].ResolveRelocate()
	if err != nil || !ok || got != filepath.Join(root, "bin", "relocate") {
		t.Fatalf("ResolveRelocate = %q, %v, %v", got, ok, err)
	}
	if _, ok, err := (ComposedDependencyStore{PackageRoot: root, Commands: map[string]string{"fetch": "x"}}).ResolveRelocate(); ok || err != nil {
		t.Fatalf("an entry with no relocate = %v, %v", ok, err)
	}

	noRoot := plan.Stores[0]
	noRoot.PackageRoot = ""
	if _, _, err := noRoot.ResolveRelocate(); !errors.Is(err, ErrPackagePathInvalid) {
		t.Fatalf("no package root error = %v, want ErrPackagePathInvalid", err)
	}
	for _, rel := range []string{"../escape", "/abs/path", "a/../../b", "bin/relocate --flag", "", "."} {
		if _, err := plan.Stores[0].ResolvePackagePath(rel); !errors.Is(err, ErrPackagePathInvalid) {
			t.Errorf("ResolvePackagePath(%q) = %v, want ErrPackagePathInvalid", rel, err)
		}
	}
}

// TestV1CompatibleViewComposesAsBefore pins that an entry with none of the
// new keys composes exactly as before: one entry, empty new fields, and the
// same JSON shape (the new fields are omitted).
func TestEntryWithoutNewKeysComposesAsBefore(t *testing.T) {
	t.Parallel()
	plan, err := ComposeDependencyStores([]ManifestView{storeKitView("k", storeDigestA, pnpmStoreView())}, storeTarget(), nil)
	if err != nil || len(plan.Stores) != 1 {
		t.Fatalf("compose = %v, %+v", err, plan)
	}
	s := plan.Stores[0]
	if s.Versions != "" || s.NoCode != nil || s.Credentials != nil || s.PackageRoot != "" {
		t.Fatalf("new fields set without the keys: %+v", s)
	}
}
