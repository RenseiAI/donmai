package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/depprep"
)

// versionedPnpmManifestTOML is a v2 manifest carrying two pnpm entries with
// disjoint version ranges: pnpm 9 (store layout v3) and pnpm 10 (v10), both
// with the no_code and credentials blocks.
const versionedPnpmManifestTOML = `api = "donmai.dev/v2"

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
versions = ">=9 <10"

[provide.dependency_store.store]
env = ["NPM_CONFIG_STORE_DIR"]
sharing = "content-addressed"
integrity = "on-use"
content = ["v3/files", "v3/index"]
bookkeeping = ["v3/projects"]

[provide.dependency_store.no_code]
env = { npm_config_ignore_scripts = "true", npm_config_ignore_pnpmfile = "true" }

[provide.dependency_store.credentials]
config_env = "NPM_CONFIG_USERCONFIG"

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"

[[provide.dependency_store]]
manager = "pnpm"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
inputs = ["pnpm-workspace.yaml"]
version = "pnpm --version"
versions = ">=10 <11"

[provide.dependency_store.store]
env = ["NPM_CONFIG_STORE_DIR"]
sharing = "content-addressed"
integrity = "on-use"
content = ["v10/files", "v10/index"]
bookkeeping = ["v10/projects"]

[provide.dependency_store.no_code]
env = { npm_config_ignore_scripts = "true", npm_config_ignore_pnpmfile = "true" }

[provide.dependency_store.credentials]
config_env = "NPM_CONFIG_USERCONFIG"

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"

[composition]
order = "foundation"
`

func scanVersionedKit(t *testing.T, body string) *KitRegistry {
	t.Helper()
	scanDir := t.TempDir()
	writeV2Kit(t, scanDir, "pnpm", body)
	return NewKitRegistry([]string{scanDir})
}

func TestKitRegistry_ParsesVersionsNoCodeCredentials(t *testing.T) {
	t.Parallel()
	m, err := loadKitManifestFile(writeTempManifest(t, versionedPnpmManifestTOML))
	if err != nil {
		t.Fatal(err)
	}
	views, err := dependencyStoreViews(m)
	if err != nil {
		t.Fatalf("dependencyStoreViews: %v", err)
	}
	if len(views) != 2 || views[0].Versions != ">=9 <10" || views[1].Versions != ">=10 <11" {
		t.Fatalf("views = %+v", views)
	}
	for _, v := range views {
		if v.NoCode == nil || v.NoCode.Env["npm_config_ignore_scripts"] != "true" || v.NoCode.Env["npm_config_ignore_pnpmfile"] != "true" {
			t.Fatalf("no_code = %+v", v.NoCode)
		}
		if v.Credentials == nil || v.Credentials.ConfigEnv != "NPM_CONFIG_USERCONFIG" {
			t.Fatalf("credentials = %+v", v.Credentials)
		}
	}
	// The scan path accepts the manifest and lists it.
	if listed := scanVersionedKit(t, versionedPnpmManifestTOML).List(); len(listed) != 1 || !listed[0].ProvidesDependencyStores {
		t.Fatalf("List = %+v, want the two-entry kit", listed)
	}
}

func writeTempManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kit.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKitRegistry_EmptyNoCodeTableIsDeclared(t *testing.T) {
	t.Parallel()
	body := strings.Replace(goDependencyStoreManifestTOML, "[provide.dependency_store.commands]", "[provide.dependency_store.no_code]\n\n[provide.dependency_store.commands]", 1)
	m, err := loadKitManifestFile(writeTempManifest(t, body))
	if err != nil {
		t.Fatal(err)
	}
	views, err := dependencyStoreViews(m)
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %+v, %v", views, err)
	}
	if views[0].NoCode == nil || len(views[0].NoCode.Env) != 0 {
		t.Fatalf("an empty [no_code] table must read as declared with no env, got %+v", views[0].NoCode)
	}
	// Control: the unmodified Go manifest declares no block.
	plain, _ := loadKitManifestFile(writeTempManifest(t, goDependencyStoreManifestTOML))
	pv, err := dependencyStoreViews(plain)
	if err != nil || pv[0].NoCode != nil || pv[0].Credentials != nil {
		t.Fatalf("absent blocks must stay nil: %+v, %v", pv, err)
	}
}

// TestKitRegistry_OldShapeV2ManifestParsesAsBefore pins that a v2 manifest
// written before the amendment has no new field set after parsing, and that
// a v1 manifest with no store section is untouched.
func TestKitRegistry_OldShapeV2ManifestParsesAsBefore(t *testing.T) {
	t.Parallel()
	m, err := loadKitManifestFile(writeTempManifest(t, dependencyStoreManifestTOML))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifestAPIRevision(m); err != nil {
		t.Fatalf("old-shape v2 rejected: %v", err)
	}
	views, err := dependencyStoreViews(m)
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %+v, %v", views, err)
	}
	v := views[0]
	if v.Versions != "" || v.NoCode != nil || v.Credentials != nil || v.Manager != "pnpm" || v.Version != "pnpm --version" {
		t.Fatalf("old-shape view changed: %+v", v)
	}

	m1, err := loadKitManifestFile(writeTempManifest(t, minimalKitTOML))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifestAPIRevision(m1); err != nil {
		t.Fatalf("v1 manifest without stores rejected: %v", err)
	}
	if views, err := dependencyStoreViews(m1); err != nil || views != nil {
		t.Fatalf("v1 manifest produced stores: %+v, %v", views, err)
	}
}

func TestKitRegistry_V1ManifestCarryingNewKeysIsRejected(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"versions":    strings.Replace(dependencyStoreManifestTOML, `version = "pnpm --version"`, "version = \"pnpm --version\"\nversions = \">=10 <11\"", 1),
		"no_code":     strings.Replace(dependencyStoreManifestTOML, "[provide.dependency_store.commands]", "[provide.dependency_store.no_code]\nenv = { npm_config_ignore_scripts = \"true\" }\n\n[provide.dependency_store.commands]", 1),
		"credentials": strings.Replace(dependencyStoreManifestTOML, "[provide.dependency_store.commands]", "[provide.dependency_store.credentials]\nconfig_env = \"NPM_CONFIG_USERCONFIG\"\n\n[provide.dependency_store.commands]", 1),
	}
	for name, v2 := range cases {
		name, v2 := name, v2
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if v2 == dependencyStoreManifestTOML {
				t.Fatal("fixture rewrite did not apply")
			}
			// Control: as v2 it scans.
			if listed := scanVersionedKit(t, v2).List(); len(listed) != 1 {
				t.Fatalf("v2 control List = %d, want 1", len(listed))
			}
			for _, api := range []string{"donmai.dev/v1", "rensei.dev/v1", "v1"} {
				v1 := strings.Replace(v2, `api = "donmai.dev/v2"`, `api = "`+api+`"`, 1)
				if listed := scanVersionedKit(t, v1).List(); len(listed) != 0 {
					t.Fatalf("api %q carrying %s: List = %+v, want rejected", api, name, listed)
				}
				m, err := loadKitManifestFile(writeTempManifest(t, v1))
				if err != nil {
					t.Fatal(err)
				}
				if err := validateManifestAPIRevision(m); err == nil {
					t.Fatalf("api %q carrying %s passed the revision gate", api, name)
				}
			}
		})
	}
}

func TestKitRegistry_RejectsInvalidNewKeys(t *testing.T) {
	t.Parallel()
	two := versionedPnpmManifestTOML
	cases := []struct{ name, from, to string }{
		{"overlapping ranges", `versions = ">=10 <11"`, `versions = ">=9.5 <11"`},
		{"identical ranges", `versions = ">=10 <11"`, `versions = ">=9 <10"`},
		{"range not semver", `versions = ">=9 <10"`, `versions = "nine"`},
		{"empty range", `versions = ">=9 <10"`, `versions = ">=10 <9"`},
		{"credentials without config_env", `config_env = "NPM_CONFIG_USERCONFIG"`, `config_env = ""`},
		{"config_env collides with store.env", `config_env = "NPM_CONFIG_USERCONFIG"`, `config_env = "NPM_CONFIG_STORE_DIR"`},
		{"no_code loader variable", `env = { npm_config_ignore_scripts = "true", npm_config_ignore_pnpmfile = "true" }`, `env = { LD_PRELOAD = "x" }`},
		{"unknown key in no_code", `[provide.dependency_store.no_code]`, "[provide.dependency_store.no_code]\nscripts = \"off\""},
		{"unknown key in credentials", `[provide.dependency_store.credentials]`, "[provide.dependency_store.credentials]\nconfigEnv = \"X\""},
		{"no_code env is not a table", `env = { npm_config_ignore_scripts = "true", npm_config_ignore_pnpmfile = "true" }`, `env = ["npm_config_ignore_scripts"]`},
		{"no_code env value is not a string", `env = { npm_config_ignore_scripts = "true", npm_config_ignore_pnpmfile = "true" }`, `env = { npm_config_ignore_scripts = true }`},
	}
	// Control: the unmodified manifest scans.
	if listed := scanVersionedKit(t, two).List(); len(listed) != 1 {
		t.Fatalf("control List = %d, want 1", len(listed))
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(two, tc.from, tc.to, 1)
			if body == two {
				t.Fatalf("rewrite %q did not apply", tc.name)
			}
			if listed := scanVersionedKit(t, body).List(); len(listed) != 0 {
				t.Fatalf("List = %+v, want the manifest rejected", listed)
			}
		})
	}
}

// preRevisionDependencyStoreTOML is the entry shape a binary built before the
// amendment decodes: none of versions, no_code or credentials.
type preRevisionDependencyStoreTOML struct {
	Manager   string   `toml:"manager"`
	Ecosystem string   `toml:"ecosystem"`
	Lockfiles []string `toml:"lockfiles"`
	Inputs    []string `toml:"inputs"`
	Version   string   `toml:"version"`
	Store     struct {
		Env         []string          `toml:"env"`
		Default     map[string]string `toml:"default"`
		Sharing     string            `toml:"sharing"`
		Integrity   string            `toml:"integrity"`
		Content     []string          `toml:"content"`
		Bookkeeping []string          `toml:"bookkeeping"`
		Secrets     []string          `toml:"secrets"`
		NeverShare  []string          `toml:"never_share"`
		RecordsPath bool              `toml:"records_path"`
	} `toml:"store"`
	Import struct {
		Env map[string]string `toml:"env"`
	} `toml:"import"`
	Commands         map[string]string            `toml:"commands"`
	CommandsOverride map[string]map[string]string `toml:"commands_override"`
	Snapshot         struct {
		Installed       []string `toml:"installed"`
		ABI             []string `toml:"abi"`
		Relocation      string   `toml:"relocation"`
		RunsPackageCode bool     `toml:"runs_package_code"`
	} `toml:"snapshot"`
	Proxy struct {
		Env map[string]string `toml:"env"`
	} `toml:"proxy"`
}

// TestPreRevisionConsumerRejectsAmendedManifest is the compatibility
// premise of amending v2 in place: a released parser applies strict keys
// inside [[provide.dependency_store]], so a manifest carrying the new keys
// leaves them undecoded and is rejected, never half-applied. The
// pre-revision shape below is the released decoder's entry type; the check
// is the released tomlUnmarshalKit rule.
func TestPreRevisionConsumerRejectsAmendedManifest(t *testing.T) {
	t.Parallel()
	type oldManifest struct {
		API     string `toml:"api"`
		Provide struct {
			DependencyStores []preRevisionDependencyStoreTOML `toml:"dependency_store"`
		} `toml:"provide"`
	}
	undecoded := func(body string) []string {
		var m oldManifest
		meta, err := toml.Decode(body, &m)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, key := range meta.Undecoded() {
			if len(key) > 2 && key[0] == "provide" && key[1] == "dependency_store" {
				keys = append(keys, key.String())
			}
		}
		return keys
	}
	if keys := undecoded(dependencyStoreManifestTOML); len(keys) != 0 {
		t.Fatalf("control: the pre-amendment manifest leaves %v undecoded for a pre-revision consumer", keys)
	}
	keys := strings.Join(undecoded(versionedPnpmManifestTOML), " ")
	for _, want := range []string{"versions", "no_code", "credentials"} {
		if !strings.Contains(keys, want) {
			t.Fatalf("a pre-revision consumer must see %q undecoded (and so reject); undecoded = %q", want, keys)
		}
	}
}

func TestKitRegistry_ComposeForRepoKeysPnpmEntriesByRange(t *testing.T) {
	t.Parallel()
	reg := scanVersionedKit(t, versionedPnpmManifestTOML)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	demand, err := reg.ComposeForRepo(repo, kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."}, nil)
	if err != nil {
		t.Fatalf("ComposeForRepo: %v", err)
	}
	if len(demand.DependencyStores) != 2 {
		t.Fatalf("composed stores = %d, want the two pnpm entries (keyed by manager and range)", len(demand.DependencyStores))
	}
	// Which of the two entries a leaf gets is the dependency preparer's
	// selection (the pin, else the manager's reported version); see
	// TestKitRegistryEntrySourcePreparesTypescriptPnpm.
	if demand.DependencyStores[0].Versions != ">=10 <11" || demand.DependencyStores[1].Versions != ">=9 <10" {
		t.Fatalf("composed ranges = %q, %q", demand.DependencyStores[0].Versions, demand.DependencyStores[1].Versions)
	}
}

func TestKitRegistry_LegacyKitCarriesNoPackageRoot(t *testing.T) {
	t.Parallel()
	reg := scanVersionedKit(t, versionedPnpmManifestTOML)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "pnpm-lock.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	demand, err := reg.ComposeForRepo(repo, kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range demand.DependencyStores {
		if s.PackageRoot != "" {
			t.Fatalf("a flat manifest has no verified package root, got %q", s.PackageRoot)
		}
	}
}

// TestKitRegistry_PackageRootReachesComposedStore installs a package whose
// manifest declares a relocate payload and checks composition carries the
// package's own directory, and that relocate resolves inside it.
func TestKitRegistry_PackageRootReachesComposedStore(t *testing.T) {
	t.Parallel()
	root := copyReleasedFixture(t, "go")
	mustWrite(t, filepath.Join(root, "bin", "relocate"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	rewriteFixtureAsV2WithRelocate(t, root, "bin/relocate")
	registry := NewKitRegistryWithTrust([]string{t.TempDir()}, TrustConfig{Mode: TrustModePermissive})
	registry.fetcher = &packageFixtureFetcher{roots: map[string]string{"default/go": root}}
	result, err := registry.Install("default/go", afclient.KitInstallRequest{
		Version: "1.0.0",
		Source:  &afclient.KitInstallSource{Kind: "git", URL: "fixture"},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Kit.InstallKind != afclient.KitInstallKindPackage {
		t.Fatalf("install kind = %q, want a package", result.Kit.InstallKind)
	}
	wantRoot := filepath.Join(registry.packageStoreRoot(), "packages", "sha256", result.Kit.PackageDigest)

	demand, err := registry.ComposeForRepo(t.TempDir(), kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."},
		[]kit.Selection{{ID: "default/go", Version: "1.0.0"}})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(demand.DependencyStores) != 1 {
		t.Fatalf("stores = %+v", demand.DependencyStores)
	}
	store := demand.DependencyStores[0]
	if store.PackageRoot != wantRoot {
		t.Fatalf("PackageRoot = %q, want %q", store.PackageRoot, wantRoot)
	}
	resolved, ok, err := store.ResolveRelocate()
	if err != nil || !ok || resolved != filepath.Join(wantRoot, "bin", "relocate") {
		t.Fatalf("ResolveRelocate = %q, %v, %v", resolved, ok, err)
	}
	if _, err := os.Stat(resolved); err != nil {
		t.Fatalf("resolved relocate path is not the installed payload: %v", err)
	}

	// The entry source carries the same root to the dependency preparer.
	leaf := t.TempDir()
	mustWrite(t, filepath.Join(leaf, "go.sum"), []byte(""), 0o600)
	entries, err := registry.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux})
	if err != nil || len(entries) != 1 || entries[0].PackageRoot != wantRoot || entries[0].Commands.Relocate != "bin/relocate" {
		t.Fatalf("entries = %+v, %v; want the package root %q and the relative relocate path", entries, err, wantRoot)
	}
	if entries[0].KitDigestKind != "package" || entries[0].KitDigest != result.Kit.PackageDigest {
		t.Fatalf("entry owner = %s:%s, want the package digest", entries[0].KitDigestKind, entries[0].KitDigest)
	}
}
