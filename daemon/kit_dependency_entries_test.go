package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/depprep"
)

// typescriptKitManifest is the official TypeScript kit's manifest as the
// donmai-kits repository publishes it at version 2.0.0, verbatim: two pnpm
// entries (>=9 <10 on the v3 store layout, >=10 <11 on v10), each with
// no_code and credentials blocks, and no post_acquire hook.
const typescriptKitManifest = "testdata/kit-dependency-entries/typescript.kit.toml"

// installTypescriptKit copies the TypeScript kit's manifest into a fresh
// kits directory and returns a registry over it.
func installTypescriptKit(t *testing.T) (*KitRegistry, string) {
	t.Helper()
	raw, err := os.ReadFile(typescriptKitManifest)
	if err != nil {
		t.Fatal(err)
	}
	kits := t.TempDir()
	mustWrite(t, filepath.Join(kits, "typescript.kit.toml"), raw, 0o600)
	return NewKitRegistry([]string{kits}), kits
}

// plainNodeLeaf is a JavaScript repository with a pnpm lockfile and no
// tsconfig.json: the TypeScript kit's detect rules do not match it.
func plainNodeLeaf(t *testing.T, packageManager string) string {
	t.Helper()
	leaf := t.TempDir()
	pkg := `{"name":"leaf","version":"1.0.0"}`
	if packageManager != "" {
		pkg = fmt.Sprintf(`{"name":"leaf","version":"1.0.0","packageManager":%q}`, packageManager)
	}
	mustWrite(t, filepath.Join(leaf, "package.json"), []byte(pkg), 0o600)
	mustWrite(t, filepath.Join(leaf, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600)
	return leaf
}

// TestKitRegistryEntriesByLockfileWithoutDetection is the eligibility rule:
// a leaf holding pnpm-lock.yaml and no tsconfig.json gets the TypeScript
// kit's pnpm entries, although the kit's detect rules do not match it.
func TestKitRegistryEntriesByLockfileWithoutDetection(t *testing.T) {
	t.Parallel()
	reg, _ := installTypescriptKit(t)
	leaf := plainNodeLeaf(t, "")

	detected, err := reg.DetectForRepo(leaf, kit.OSLinux)
	if err != nil {
		t.Fatal(err)
	}
	if len(detected) != 0 {
		t.Fatalf("control: the TypeScript kit detected a leaf without tsconfig.json: %+v", detected)
	}

	entries, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the TypeScript kit's two pnpm entries", len(entries))
	}
	wantVersions := []string{">=10 <11", ">=9 <10"}
	wantContent := [][]string{{"v10/files", "v10/index"}, {"v3/files"}}
	for i, e := range entries {
		if e.Manager != "pnpm" || e.Ecosystem != "node" || e.KitID != "default/typescript" {
			t.Fatalf("entry %d identity = %s/%s from %s", i, e.Manager, e.Ecosystem, e.KitID)
		}
		if e.Versions != wantVersions[i] || !reflect.DeepEqual(e.Store.Content, wantContent[i]) {
			t.Fatalf("entry %d = versions %q content %v, want %q %v", i, e.Versions, e.Store.Content, wantVersions[i], wantContent[i])
		}
		if e.KitDigestKind != "legacy-manifest" || len(e.KitDigest) != 64 || e.PackageRoot != "" {
			t.Fatalf("entry %d owner = %s:%s root %q, want a flat manifest's digest and no package root", i, e.KitDigestKind, e.KitDigest, e.PackageRoot)
		}
		want := depprep.Commands{
			Fetch:     "pnpm fetch",
			Install:   "pnpm install --offline --frozen-lockfile",
			Online:    "pnpm install --prefer-offline --frozen-lockfile",
			Reconcile: "pnpm install --offline --frozen-lockfile",
		}
		if e.Commands != want || e.VersionCommand != "pnpm --version" {
			t.Fatalf("entry %d commands = %+v / %q", i, e.Commands, e.VersionCommand)
		}
		if e.NoCode == nil || !reflect.DeepEqual(e.NoCode.Env, map[string]string{
			"npm_config_ignore_scripts": "true", "npm_config_ignore_pnpmfile": "true",
		}) {
			t.Fatalf("entry %d no_code = %+v", i, e.NoCode)
		}
		if e.Credentials == nil || e.Credentials.ConfigEnv != "NPM_CONFIG_USERCONFIG" {
			t.Fatalf("entry %d credentials = %+v", i, e.Credentials)
		}
		if !reflect.DeepEqual(e.Store.Env, []string{"NPM_CONFIG_STORE_DIR"}) ||
			e.ImportEnv["NPM_CONFIG_PACKAGE_IMPORT_METHOD"] != "clone-or-copy" ||
			!reflect.DeepEqual(e.Installed, []string{"node_modules", "**/node_modules"}) || !e.RunsPackageCode {
			t.Fatalf("entry %d store/import/installed = %+v", i, e)
		}
	}

	// A leaf holding no lockfile any kit names gets nothing, without error.
	bare := t.TempDir()
	mustWrite(t, filepath.Join(bare, "package.json"), []byte(`{}`), 0o600)
	if none, err := reg.Entries(context.Background(), depprep.Leaf{Root: bare, OS: kit.OSLinux}); err != nil || len(none) != 0 {
		t.Fatalf("lockfile-less leaf = %+v, %v; want none", none, err)
	}
}

// TestKitRegistryEntriesContributingKits states which installed kits
// contribute entries when no kit's detect rules match: every enabled kit that
// supports the leaf's OS and declares an entry whose lockfiles the leaf
// holds, and only those entries of it.
func TestKitRegistryEntriesContributingKits(t *testing.T) {
	t.Parallel()
	reg, kits := installTypescriptKit(t)
	// Declares gomod (go.sum, absent from the leaf): contributes nothing.
	writeV2Kit(t, kits, "gomod", goDependencyStoreManifestTOML)
	// Declares pnpm for windows only: unsupported OS, contributes nothing.
	windowsOnly := strings.Replace(strings.Replace(versionedPnpmManifestTOML,
		`os = ["linux", "macos"]`, `os = ["windows"]`, 1), `id = "node/pnpm"`, `id = "node/pnpm-windows"`, 1)
	writeV2Kit(t, kits, "pnpm-windows", windowsOnly)
	// Declares pnpm with ranges overlapping the TypeScript kit's: disabled,
	// so contributes nothing and does not conflict.
	writeV2Kit(t, kits, "pnpm-overlap", versionedPnpmManifestTOML)
	if _, err := reg.Disable("node/pnpm"); err != nil {
		t.Fatal(err)
	}
	// Declares bun (bun.lock, absent from the leaf) and pnpm >=11 <12
	// (disjoint from the TypeScript kit's ranges); its detect rules do not
	// match either. It contributes its pnpm entry and not its bun entry.
	writeV2Kit(t, kits, "bun-and-pnpm11", bunAndPnpm11Manifest)

	leaf := plainNodeLeaf(t, "")
	if detected, err := reg.DetectForRepo(leaf, kit.OSLinux); err != nil || len(detected) != 0 {
		t.Fatalf("control: detected %+v, %v; want no kit to detect the leaf", detected, err)
	}
	entries, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.KitID+" "+e.Manager+" "+e.Versions)
	}
	want := []string{
		"default/typescript pnpm >=10 <11",
		"node/bun-and-pnpm11 pnpm >=11 <12",
		"default/typescript pnpm >=9 <10",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("contributing entries = %q, want %q", got, want)
	}
	// A declared, empty [no_code] block stays declared; an absent
	// [credentials] block stays absent.
	if nc := entries[1].NoCode; nc == nil || len(nc.Env) != 0 || entries[1].Credentials != nil {
		t.Fatalf("pnpm 11 entry no_code = %+v credentials = %+v, want declared-empty and absent", nc, entries[1].Credentials)
	}

	// Enabling the overlapping kit makes the composition conflict, which the
	// preparer reports as entries_unavailable before running anything.
	if _, err := reg.Enable("node/pnpm"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux}); !errors.Is(err, kit.ErrDependencyStoreConflict) {
		t.Fatalf("overlapping kits error = %v, want ErrDependencyStoreConflict", err)
	}
}

const bunAndPnpm11Manifest = `api = "donmai.dev/v2"

[kit]
id = "node/bun-and-pnpm11"
version = "1.0.0"
name = "bun and pnpm 11"

[supports]
os = ["linux", "macos"]

[detect]
files = ["bunfig.toml"]

[[provide.dependency_store]]
manager = "bun"
ecosystem = "node"
lockfiles = ["bun.lock"]
version = "bun --version"

[provide.dependency_store.store]
env = ["BUN_INSTALL_CACHE_DIR"]
sharing = "session-only"
integrity = "on-use"

[provide.dependency_store.commands]
fetch = "bun install --frozen-lockfile --ignore-scripts"
install = "bun install --frozen-lockfile --ignore-scripts"

[[provide.dependency_store]]
manager = "pnpm"
ecosystem = "node"
lockfiles = ["pnpm-lock.yaml"]
version = "pnpm --version"
versions = ">=11 <12"

[provide.dependency_store.store]
env = ["PNPM_CONFIG_STORE_DIR"]
sharing = "session-only"
integrity = "on-use"

[provide.dependency_store.no_code]

[provide.dependency_store.commands]
fetch = "pnpm fetch"
install = "pnpm install --offline --frozen-lockfile"
`

// TestKitRegistryEntriesNarrowLockToLeaf: an operator lock binding for a
// manager the leaf does not use is not stale for this leaf, and a binding
// for one it does use still pins the kit digest.
func TestKitRegistryEntriesNarrowLockToLeaf(t *testing.T) {
	t.Parallel()
	reg, kits := installTypescriptKit(t)
	leaf := plainNodeLeaf(t, "")
	raw, err := os.ReadFile(filepath.Join(kits, "typescript.kit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex(raw)
	writeLock := func(bindings ...kit.LockedDependencyBinding) {
		lock := kit.CompositionLock{Schema: kit.CompositionLockSchema, Targets: []kit.LockedCompositionTarget{{
			Target:       kit.CompositionTarget{OS: kit.OSLinux, PathScope: "."},
			Dependencies: bindings,
		}}}
		body, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		if body, err = jsoncanonicalizer.Transform(body); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(kits, kitCompositionLockName), body, 0o600)
	}
	other := kit.LockedDependencyBinding{Manager: "bun", SelectedKitID: "node/bun", SelectedDigestKind: "legacy-manifest", SelectedDigest: strings.Repeat("a", 64)}
	writeLock(other)
	if entries, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux}); err != nil || len(entries) != 2 {
		t.Fatalf("with an unrelated binding = %d entries, %v; want the two pnpm entries", len(entries), err)
	}
	writeLock(other, kit.LockedDependencyBinding{Manager: "pnpm", SelectedKitID: "default/typescript", SelectedDigestKind: "legacy-manifest", SelectedDigest: digest})
	if entries, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux}); err != nil || len(entries) != 2 {
		t.Fatalf("with a matching pnpm binding = %d entries, %v", len(entries), err)
	}
	writeLock(kit.LockedDependencyBinding{Manager: "pnpm", SelectedKitID: "default/typescript", SelectedDigestKind: "legacy-manifest", SelectedDigest: strings.Repeat("b", 64)})
	if _, err := reg.Entries(context.Background(), depprep.Leaf{Root: leaf, OS: kit.OSLinux}); !errors.Is(err, kit.ErrCompositionLockInvalid) {
		t.Fatalf("moved digest error = %v, want ErrCompositionLockInvalid", err)
	}
}

// fakePnpm writes a fake pnpm into a fresh toolchain directory. It logs each
// invocation's arguments and each phase's environment, answers --version with
// the leaf's packageManager pin (as a self-switching pnpm does once it has
// provisioned itself) or hostVersion, and creates node_modules on install.
func fakePnpm(t *testing.T, hostVersion string) (toolchain, logs string) {
	t.Helper()
	base := t.TempDir()
	toolchain, logs = filepath.Join(base, "toolchain"), filepath.Join(base, "logs")
	for _, d := range []string{toolchain, logs} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
logs=%q
echo "$*" >> "$logs/argv.log"
case "$1" in
--version)
  pin=$(sed -n 's/.*"packageManager":"pnpm@\([0-9][0-9.]*\).*/\1/p' package.json 2>/dev/null)
  if [ -n "$pin" ]; then echo "$pin"; else echo %q; fi
  exit 0 ;;
fetch)
  env > "$logs/fetch.env"
  mkdir -p "$NPM_CONFIG_STORE_DIR" && touch "$NPM_CONFIG_STORE_DIR/fetched"
  exit 0 ;;
install)
  env > "$logs/install.env"
  [ -f "$NPM_CONFIG_STORE_DIR/fetched" ] || { echo "store not fetched" >&2; exit 3; }
  mkdir -p node_modules/dep
  if [ "$npm_config_ignore_scripts" != "true" ]; then touch node_modules/dep/postinstall-ran; fi
  exit 0 ;;
esac
echo "unexpected: $*" >&2
exit 64
`, logs, hostVersion)
	mustWrite(t, filepath.Join(toolchain, "pnpm"), []byte(script), 0o755)
	return toolchain, logs
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test log under t.TempDir
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func envHas(t *testing.T, path, key, value string) bool {
	t.Helper()
	for _, line := range readLines(t, path) {
		if line == key+"="+value {
			return true
		}
	}
	return false
}

// TestKitRegistryEntrySourcePreparesTypescriptPnpm runs Prepare over the
// registry with the TypeScript kit installed, on a plain Node leaf, with a
// fake pnpm on the toolchain path. The leaf's packageManager pin selects
// the entry; an uncovered pin is toolchain_mismatch with nothing run. The
// commands the kit declares run without a shell, as argument vectors.
func TestKitRegistryEntrySourcePreparesTypescriptPnpm(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the leaf hygiene check")
	}
	for _, tc := range []struct {
		name         string
		pin          string
		hostVersion  string
		wantVersions string
		wantVersion  string
		wantArgv     []string
		wantCode     depprep.Code
	}{
		{
			name: "pnpm 9 pin selects the >=9 <10 entry", pin: "pnpm@9.15.9", hostVersion: "12.16.1",
			wantVersions: ">=9 <10", wantVersion: "9.15.9",
			wantArgv: []string{"fetch", "--version", "install --offline --frozen-lockfile"},
		},
		{
			name: "pnpm 10 pin selects the >=10 <11 entry", pin: "pnpm@10.4.1", hostVersion: "12.16.1",
			wantVersions: ">=10 <11", wantVersion: "10.4.1",
			wantArgv: []string{"fetch", "--version", "install --offline --frozen-lockfile"},
		},
		{
			name: "unpinned leaf asks the manager", hostVersion: "10.2.0",
			wantVersions: ">=10 <11", wantVersion: "10.2.0",
			wantArgv: []string{"--version", "fetch", "install --offline --frozen-lockfile"},
		},
		{name: "pnpm 8 pin is toolchain_mismatch", pin: "pnpm@8.15.0", hostVersion: "12.16.1", wantCode: depprep.CodeToolchainMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, _ := installTypescriptKit(t)
			leaf := plainNodeLeaf(t, tc.pin)
			mustWrite(t, filepath.Join(leaf, ".gitignore"), []byte("node_modules\n"), 0o600)
			if out, err := exec.Command("git", "init", "-q", leaf).CombinedOutput(); err != nil { //nolint:gosec // fresh temp directory
				t.Fatalf("git init: %v: %s", err, out)
			}
			toolchain, logs := fakePnpm(t, tc.hostVersion)
			p, err := depprep.New(depprep.Config{Entries: reg})
			if err != nil {
				t.Fatal(err)
			}
			rep, err := p.Prepare(context.Background(), depprep.Request{
				Root:      leaf,
				Subject:   depprep.Subject{Kind: depprep.SubjectInvocation, ID: "invocation-1"},
				Toolchain: []string{toolchain},
				Scratch:   t.TempDir(),
				Network:   depprep.NetworkAllow,
			})
			if tc.wantCode != "" {
				var e *depprep.Error
				if !errors.As(err, &e) || e.Code != tc.wantCode || !errors.Is(err, depprep.ErrToolchainMismatch) {
					t.Fatalf("Prepare error = %v, want %s", err, tc.wantCode)
				}
				if rep.Outcome != depprep.OutcomeFailed || rep.Entries[0].MissReason != depprep.MissToolchainMismatch {
					t.Fatalf("report = %+v", rep)
				}
				if argv := readLines(t, filepath.Join(logs, "argv.log")); len(argv) != 0 {
					t.Fatalf("a mismatched pin ran pnpm: %q", argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v (report %+v)", err, rep)
			}
			if rep.Outcome != depprep.OutcomeReady || len(rep.Entries) != 1 {
				t.Fatalf("report = %+v", rep)
			}
			er := rep.Entries[0]
			if er.KitID != "default/typescript" || er.Versions != tc.wantVersions || er.Version != tc.wantVersion ||
				er.Path != depprep.PathOnline || er.PackageCode != depprep.PackageCodeSkipped {
				t.Fatalf("entry report = %+v", er)
			}
			if argv := readLines(t, filepath.Join(logs, "argv.log")); !reflect.DeepEqual(argv, tc.wantArgv) {
				t.Fatalf("pnpm invocations = %q, want %q", argv, tc.wantArgv)
			}
			for _, phase := range []string{"fetch.env", "install.env"} {
				for _, kv := range [][2]string{
					{"npm_config_ignore_scripts", "true"},
					{"npm_config_ignore_pnpmfile", "true"},
					{"NPM_CONFIG_PACKAGE_IMPORT_METHOD", "clone-or-copy"},
					{"CI", "1"},
				} {
					if !envHas(t, filepath.Join(logs, phase), kv[0], kv[1]) {
						t.Fatalf("%s lacks %s=%s", phase, kv[0], kv[1])
					}
				}
			}
			if _, err := os.Stat(filepath.Join(leaf, "node_modules", "dep", "postinstall-ran")); err == nil {
				t.Fatal("package code ran unconfined")
			}
			if rep.Leaf.Status != depprep.LeafCheckClean {
				t.Fatalf("leaf check = %+v", rep.Leaf)
			}
		})
	}
}
