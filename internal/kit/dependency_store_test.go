package kit

import (
	"errors"
	"path"
	"strings"
	"testing"
)

const (
	storeDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	storeDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func pnpmStoreView() DependencyStoreView {
	return DependencyStoreView{
		Manager:   "pnpm",
		Ecosystem: "node",
		Lockfiles: []string{"pnpm-lock.yaml"},
		Inputs:    []string{"pnpm-workspace.yaml"},
		Version:   "pnpm --version",
		Store: DependencyStoreLayout{
			Env:         []string{"NPM_CONFIG_STORE_DIR"},
			Sharing:     DependencyStoreSharingContentAddressed,
			Integrity:   DependencyStoreIntegrityOnUse,
			Content:     []string{"v10/files", "v10/index"},
			Bookkeeping: []string{"v10/projects"},
		},
		ImportEnv: map[string]string{"NPM_CONFIG_PACKAGE_IMPORT_METHOD": "clone-or-copy"},
		Commands: map[string]string{
			"fetch":   "pnpm fetch",
			"install": "pnpm install --offline --frozen-lockfile",
			"online":  "pnpm install --prefer-offline --frozen-lockfile",
		},
		Snapshot: DependencyStoreSnapshot{
			Installed:       []string{"node_modules"},
			ABI:             []string{"os", "arch"},
			Relocation:      "reconcile",
			RunsPackageCode: true,
		},
	}
}

func goStoreView() DependencyStoreView {
	return DependencyStoreView{
		Manager:   "gomod",
		Ecosystem: "go",
		Lockfiles: []string{"go.sum"},
		Inputs:    []string{"go.mod"},
		Version:   "go version",
		Store: DependencyStoreLayout{
			Env:       []string{"GOMODCACHE"},
			Sharing:   DependencyStoreSharingContentAddressed,
			Integrity: DependencyStoreIntegrityOnFetch,
			Content:   []string{"cache/download"},
		},
		Commands: map[string]string{
			"fetch":   "go mod download",
			"install": "go mod download",
			"verify":  "go mod verify",
		},
		ProxyEnv: map[string]string{"GOPROXY": "file://<generation>/cache/download"},
	}
}

func storeKitView(id, digest string, stores ...DependencyStoreView) ManifestView {
	return ManifestView{
		ID: id, PackageDigest: digest, PathScope: ".",
		SupportedOS:      []string{OSLinux, OSMacOS},
		DependencyStores: stores,
	}
}

func storeTarget() CompositionTarget {
	return CompositionTarget{OS: OSLinux, PathScope: "."}
}

func TestComposeDependencyStoresUnionAcrossEcosystems(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("node-kit", storeDigestA, pnpmStoreView()),
		storeKitView("go-kit", storeDigestB, goStoreView()),
	}
	plan, err := ComposeDependencyStores(views, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(plan.Stores) != 2 {
		t.Fatalf("stores = %d, want 2", len(plan.Stores))
	}
	if plan.Stores[0].Manager != "gomod" || plan.Stores[1].Manager != "pnpm" {
		t.Fatalf("managers = [%s %s], want [gomod pnpm] sorted", plan.Stores[0].Manager, plan.Stores[1].Manager)
	}
	if plan.Stores[1].KitID != "node-kit" || plan.Stores[1].Digest != storeDigestA {
		t.Fatalf("pnpm owner = %+v, want node-kit@%s", plan.Stores[1], storeDigestA)
	}
	// reconcile defaults to install when the entry omits it.
	if plan.Stores[1].Commands["reconcile"] != "pnpm install --offline --frozen-lockfile" {
		t.Fatalf("reconcile = %q, want install default", plan.Stores[1].Commands["reconcile"])
	}
	if len(plan.Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", plan.Digest)
	}
}

func TestComposeDependencyStoresSameManagerConflicts(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("kit-a", storeDigestA, pnpmStoreView()),
		storeKitView("kit-b", storeDigestB, pnpmStoreView()),
	}
	_, err := ComposeDependencyStores(views, storeTarget(), nil)
	if !errors.Is(err, ErrDependencyStoreConflict) {
		t.Fatalf("conflict error = %v, want ErrDependencyStoreConflict", err)
	}
	for _, want := range []string{"pnpm", "kit-a", "kit-b", "operator lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestComposeDependencyStoresLockSelectsOne(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("kit-a", storeDigestA, pnpmStoreView()),
		storeKitView("kit-b", storeDigestB, pnpmStoreView()),
	}
	target := storeTarget()
	lock := &CompositionLock{
		Schema: CompositionLockSchema,
		Targets: []LockedCompositionTarget{{
			Target: target,
			Dependencies: []LockedDependencyBinding{
				{Manager: "pnpm", SelectedKitID: "kit-b"},
			},
		}},
	}
	plan, err := ComposeDependencyStores(views, target, lock)
	if err != nil {
		t.Fatalf("locked compose: %v", err)
	}
	if len(plan.Stores) != 1 || plan.Stores[0].KitID != "kit-b" || plan.Stores[0].Digest != storeDigestB {
		t.Fatalf("locked stores = %+v, want kit-b only", plan.Stores)
	}
}

func TestComposeDependencyStoresLockSelectingMissingKitFails(t *testing.T) {
	t.Parallel()
	views := []ManifestView{storeKitView("kit-a", storeDigestA, pnpmStoreView())}
	target := storeTarget()
	lock := &CompositionLock{
		Schema: CompositionLockSchema,
		Targets: []LockedCompositionTarget{{
			Target: target,
			Dependencies: []LockedDependencyBinding{
				{Manager: "pnpm", SelectedKitID: "kit-ghost"},
			},
		}},
	}
	_, err := ComposeDependencyStores(views, target, lock)
	if !errors.Is(err, ErrCompositionLockInvalid) {
		t.Fatalf("stale lock error = %v, want ErrCompositionLockInvalid", err)
	}
}

func TestComposeDependencyStoresStaleLockRowFails(t *testing.T) {
	t.Parallel()
	views := []ManifestView{storeKitView("node-kit", storeDigestA, pnpmStoreView())}
	target := storeTarget()
	lock := &CompositionLock{
		Schema: CompositionLockSchema,
		Targets: []LockedCompositionTarget{{
			Target: target,
			Dependencies: []LockedDependencyBinding{
				{Manager: "pnpm", SelectedKitID: "node-kit"},
				{Manager: "bun", SelectedKitID: "node-kit"},
			},
		}},
	}
	_, err := ComposeDependencyStores(views, target, lock)
	if !errors.Is(err, ErrCompositionLockInvalid) {
		t.Fatalf("stale row error = %v, want ErrCompositionLockInvalid", err)
	}
}

func TestValidateDependencyStoreTable(t *testing.T) {
	t.Parallel()
	valid := pnpmStoreView()
	cases := []struct {
		name   string
		mutate func(*DependencyStoreView)
		want   string
	}{
		{"valid", func(*DependencyStoreView) {}, ""},
		{"manager required", func(s *DependencyStoreView) { s.Manager = "" }, "manager is required"},
		{"lockfiles required", func(s *DependencyStoreView) { s.Lockfiles = nil }, "lockfiles are required"},
		{"fetch required", func(s *DependencyStoreView) { delete(s.Commands, "fetch") }, "fetch command is required"},
		{"install required", func(s *DependencyStoreView) { s.Commands["install"] = "" }, "install command is required"},
		{"unknown integrity", func(s *DependencyStoreView) { s.Store.Integrity = "sometimes" }, "unknown integrity"},
		{"none forces session-only", func(s *DependencyStoreView) {
			s.Store.Integrity = DependencyStoreIntegrityNone
		}, "session-only"},
		{"none with session-only valid", func(s *DependencyStoreView) {
			s.Store.Integrity = DependencyStoreIntegrityNone
			s.Store.Sharing = DependencyStoreSharingSessionOnly
		}, ""},
		{"unknown sharing", func(s *DependencyStoreView) { s.Store.Sharing = "shared-ish" }, "unknown sharing"},
		{"absolute store path", func(s *DependencyStoreView) { s.Store.Content = []string{"/abs"} }, "not relative"},
		{"escaping store path", func(s *DependencyStoreView) { s.Store.Content = []string{"../escape"} }, "escapes the store"},
		{"secrets overlap content", func(s *DependencyStoreView) {
			s.Store.Secrets = []string{"v10/files/token"}
		}, "overlaps shared content"},
		{"never share overlap content", func(s *DependencyStoreView) {
			s.Store.NeverShare = []string{"v10/index"}
		}, "overlaps shared content"},
		{"unknown command", func(s *DependencyStoreView) { s.Commands["frobnicate"] = "x" }, "unknown command"},
		{"unknown override OS", func(s *DependencyStoreView) {
			s.CommandsOverride = map[string]map[string]string{"plan9": {"fetch": "x"}}
		}, "unknown OS"},
		{"override without base", func(s *DependencyStoreView) {
			s.CommandsOverride = map[string]map[string]string{OSLinux: {"relocate": "bin/relocate"}}
		}, "no same-entry base command"},
		{"bad relocation", func(s *DependencyStoreView) { s.Snapshot.Relocation = "teleport" }, "unknown snapshot relocation"},
		// Store paths that clean to the store root would make content
		// cover every secrets path while the overlap check misses it.
		{"content ./ is the store root", func(s *DependencyStoreView) {
			s.Store.Content, s.Store.Secrets = []string{"./"}, []string{"credentials.toml"}
		}, "not in canonical form"},
		{"content a/.. is the store root", func(s *DependencyStoreView) {
			s.Store.Content, s.Store.Secrets = []string{"a/.."}, []string{"credentials.toml"}
		}, "not in canonical form"},
		{"never_share x/../ is the store root", func(s *DependencyStoreView) {
			s.Store.Content, s.Store.NeverShare = []string{"x/../"}, []string{"bin"}
		}, "not in canonical form"},
		{"content glob", func(s *DependencyStoreView) {
			s.Store.Content, s.Store.Secrets = []string{"*"}, []string{"credentials.toml"}
		}, "not a literal path"},
		{"non-canonical secrets", func(s *DependencyStoreView) { s.Store.Secrets = []string{"./credentials.toml"} }, "not in canonical form"},
		{"manager traversal", func(s *DependencyStoreView) { s.Manager = "../../tmp/evil" }, "not an identifier"},
		{"manager with slash", func(s *DependencyStoreView) { s.Manager = "pnpm/10" }, "not an identifier"},
		{"manager dot-dot", func(s *DependencyStoreView) { s.Manager = ".." }, "not an identifier"},
		{"manager with version dot valid", func(s *DependencyStoreView) { s.Manager = "pnpm-10.x" }, ""},
		{"empty lockfile", func(s *DependencyStoreView) { s.Lockfiles = []string{""} }, "empty lockfile entry"},
		{"absolute lockfile", func(s *DependencyStoreView) { s.Lockfiles = []string{"/etc/passwd"} }, "not a canonical relative"},
		{"escaping lockfile", func(s *DependencyStoreView) { s.Lockfiles = []string{"../../.ssh/id_rsa"} }, "escapes the leaf"},
		{"absolute input", func(s *DependencyStoreView) { s.Inputs = []string{"/home/u/.npmrc"} }, "not a canonical relative"},
		{"escaping input glob", func(s *DependencyStoreView) { s.Inputs = []string{"**/../../.aws/credentials"} }, "not a canonical relative"},
		{"escaping input", func(s *DependencyStoreView) { s.Inputs = []string{"../.npmrc"} }, "escapes the leaf"},
		{"input globs valid", func(s *DependencyStoreView) {
			s.Inputs = []string{"pnpm-workspace.yaml", "**/package.json", ".npmrc", "patches/**"}
		}, ""},
		{"escaping installed", func(s *DependencyStoreView) { s.Snapshot.Installed = []string{"../.."} }, "escapes the leaf"},
		{"absolute installed", func(s *DependencyStoreView) { s.Snapshot.Installed = []string{"/"} }, "not a canonical relative"},
		{"installed glob valid", func(s *DependencyStoreView) { s.Snapshot.Installed = []string{"node_modules", "**/node_modules"} }, ""},
		{"store env bad name", func(s *DependencyStoreView) { s.Store.Env = []string{"BAD NAME=1"} }, "not an environment variable name"},
		{"store env empty", func(s *DependencyStoreView) { s.Store.Env = []string{""} }, "not an environment variable name"},
		{"store env leading digit", func(s *DependencyStoreView) { s.Store.Env = []string{"1STORE"} }, "not an environment variable name"},
		{"import env bad key", func(s *DependencyStoreView) { s.ImportEnv = map[string]string{"A=B": "c"} }, "not an environment variable name"},
		{"proxy env empty key", func(s *DependencyStoreView) { s.ProxyEnv = map[string]string{"": "x"} }, "not an environment variable name"},
		{"store default unknown OS", func(s *DependencyStoreView) { s.Store.Default = map[string]string{"darwin": "~/x"} }, "unknown OS"},
		{"store default valid", func(s *DependencyStoreView) {
			s.Store.Default = map[string]string{OSMacOS: "~/Library/pnpm/store", OSLinux: "~/.local/share/pnpm/store"}
		}, ""},
		{"relocate package path valid", func(s *DependencyStoreView) { s.Commands["relocate"] = "bin/pnpm-relocate" }, ""},
		{"relocate with arguments", func(s *DependencyStoreView) { s.Commands["relocate"] = "bin/relocate --flag" }, "must be a package path"},
		{"relocate absolute", func(s *DependencyStoreView) { s.Commands["relocate"] = "/usr/bin/relocate" }, "must be a package path"},
		{"relocate escaping", func(s *DependencyStoreView) { s.Commands["relocate"] = "../relocate" }, "escapes the package"},
		{"relocate variable", func(s *DependencyStoreView) { s.Commands["relocate"] = "$HOME/relocate" }, "must be a package path"},
		{"relocate override with arguments", func(s *DependencyStoreView) {
			s.Commands["relocate"] = "bin/relocate"
			s.CommandsOverride = map[string]map[string]string{OSMacOS: {"relocate": "bin/relocate --macos"}}
		}, "must be a package path"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry := pnpmStoreView()
			tc.mutate(&entry)
			err := ValidateDependencyStore(entry)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid entry rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
			if !errors.Is(err, ErrDependencyStoreInvalid) {
				t.Fatalf("error = %v, want ErrDependencyStoreInvalid", err)
			}
		})
	}
	if err := ValidateDependencyStores([]DependencyStoreView{valid, pnpmStoreView()}); err == nil {
		t.Fatal("duplicate manager in one kit accepted")
	} else if !errors.Is(err, ErrDependencyStoreInvalid) {
		t.Fatalf("duplicate manager error = %v, want ErrDependencyStoreInvalid", err)
	}
}

func TestSelectDependencyStoresByLockfile(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("node-kit", storeDigestA, pnpmStoreView()),
		storeKitView("go-kit", storeDigestB, goStoreView()),
	}
	plan, err := ComposeDependencyStores(views, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	selected, err := SelectDependencyStores(plan.Stores, LeafDependencyFacts{
		Files: []string{"pnpm-lock.yaml", "package.json"},
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 || selected[0].Manager != "pnpm" {
		t.Fatalf("selected = %+v, want pnpm only", selected)
	}
	empty, err := SelectDependencyStores(plan.Stores, LeafDependencyFacts{
		Files: []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("no-lockfile select: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("no-lockfile selected = %+v, want none", empty)
	}
}

func TestSelectDependencyStoresAmbiguousWithoutLeafDeclaration(t *testing.T) {
	t.Parallel()
	npmEntry := pnpmStoreView()
	npmEntry.Manager = "npm"
	npmEntry.Lockfiles = []string{"package-lock.json"}
	bunEntry := pnpmStoreView()
	bunEntry.Manager = "bun"
	bunEntry.Lockfiles = []string{"bun.lock"}
	views := []ManifestView{
		storeKitView("npm-kit", storeDigestA, npmEntry),
		storeKitView("bun-kit", storeDigestB, bunEntry),
	}
	plan, err := ComposeDependencyStores(views, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	leaf := LeafDependencyFacts{Files: []string{"package-lock.json", "bun.lock", "package.json"}}
	if _, err := SelectDependencyStores(plan.Stores, leaf); !errors.Is(err, ErrDependencyStoreAmbiguous) {
		t.Fatalf("ambiguous error = %v, want ErrDependencyStoreAmbiguous", err)
	} else if !strings.Contains(err.Error(), "manager_ambiguous") {
		t.Fatalf("ambiguous error %q missing manager_ambiguous token", err)
	}
	leaf.PackageManager = "bun@1.2.0"
	selected, err := SelectDependencyStores(plan.Stores, leaf)
	if err != nil {
		t.Fatalf("declared select: %v", err)
	}
	if len(selected) != 1 || selected[0].Manager != "bun" {
		t.Fatalf("selected = %+v, want bun via packageManager", selected)
	}
}

func TestComposeForTargetResolvesOSKeyedStoreCommands(t *testing.T) {
	t.Parallel()
	entry := pnpmStoreView()
	entry.Commands["relocate"] = "bin/pnpm-relocate"
	entry.CommandsOverride = map[string]map[string]string{
		OSLinux: {"fetch": "pnpm fetch --linux"},
		OSMacOS: {"fetch": "pnpm fetch --macos"},
	}
	views := []ManifestView{storeKitView("node-kit", storeDigestA, entry)}
	demand, err := ComposeForTarget(views, CompositionTarget{OS: OSLinux, PathScope: "."}, nil, nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(demand.DependencyStores) != 1 {
		t.Fatalf("stores = %d, want 1", len(demand.DependencyStores))
	}
	got := demand.DependencyStores[0].Commands
	if got["fetch"] != "pnpm fetch --linux" {
		t.Fatalf("linux fetch = %q, want OS override", got["fetch"])
	}
	if got["relocate"] != "bin/pnpm-relocate" {
		t.Fatalf("relocate = %q, want generic form", got["relocate"])
	}
	if demand.DependencyStoresDigest == "" {
		t.Fatal("dependency stores digest empty")
	}

	macDemand, err := ComposeForTarget(views, CompositionTarget{OS: OSMacOS, PathScope: "."}, nil, nil)
	if err != nil {
		t.Fatalf("macos compose: %v", err)
	}
	if macDemand.DependencyStores[0].Commands["fetch"] != "pnpm fetch --macos" {
		t.Fatalf("macos fetch = %q, want OS override", macDemand.DependencyStores[0].Commands["fetch"])
	}
}

func TestComposeForTargetStoreConflictFailsDemand(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("kit-a", storeDigestA, pnpmStoreView()),
		storeKitView("kit-b", storeDigestB, pnpmStoreView()),
	}
	if _, err := ComposeForTarget(views, storeTarget(), nil, nil); !errors.Is(err, ErrDependencyStoreConflict) {
		t.Fatalf("demand conflict error = %v, want ErrDependencyStoreConflict", err)
	}
}

func TestSelectDependencyStoresDigestDeterminism(t *testing.T) {
	t.Parallel()
	views := []ManifestView{
		storeKitView("go-kit", storeDigestB, goStoreView()),
		storeKitView("node-kit", storeDigestA, pnpmStoreView()),
	}
	first, err := ComposeDependencyStores(views, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	second, err := ComposeDependencyStores([]ManifestView{views[1], views[0]}, storeTarget(), nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ: %s vs %s", first.Digest, second.Digest)
	}
}

// FuzzDependencyStorePathContainment holds the classification invariant
// for any accepted spelling: a content path and a secrets path resolve
// strictly inside the store, and content never covers the secret. The
// seeds include the spellings that once cleaned to the store root.
func FuzzDependencyStorePathContainment(f *testing.F) {
	for _, seed := range [][2]string{
		{"v10/files", "credentials.toml"},
		{"./", "credentials.toml"},
		{"a/..", "x"},
		{"./.", "credentials.toml"},
		{"*", "credentials.toml"},
		{"registry", "registry/credentials.toml"},
		{"../x", "y"},
		{"a//b", "a/b/c"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, content, secret string) {
		entry := pnpmStoreView()
		entry.Store.Content = []string{content}
		entry.Store.Secrets = []string{secret}
		if ValidateDependencyStore(entry) != nil {
			return
		}
		root := "/store"
		c, s := path.Join(root, content), path.Join(root, secret)
		if !strings.HasPrefix(c, root+"/") || !strings.HasPrefix(s, root+"/") {
			t.Fatalf("accepted content %q / secret %q resolving outside or at the store root", content, secret)
		}
		if c == s || strings.HasPrefix(s, c+"/") || strings.HasPrefix(c, s+"/") {
			t.Fatalf("accepted content %q covering secret %q", content, secret)
		}
		if strings.ContainsAny(content, "*?[") {
			t.Fatalf("accepted glob content %q", content)
		}
	})
}
