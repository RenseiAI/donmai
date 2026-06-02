package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

const toolchainManifestTOML = `
api = "donmai.dev/v1"
[kit]
id = "typescript"
version = "1.0.0"
name = "TypeScript / Next.js"
priority = 50
[supports]
os = ["linux", "macos"]
arch = ["x86_64", "arm64"]
[detect]
files = ["package.json", "tsconfig.json"]
not_files = ["go.mod"]
[detect.toolchain]
node = "20"
[provide.commands]
build = "npm run build"
test = "npm test"
[provide.commands_override.macos]
build = "npm run build:mac"
[provide.toolchain_install.linux]
node = "curl -fsSL setup_20 | bash - && apt-get install -y nodejs"
[provide.toolchain_install.macos]
node = "brew install node@20"
[provide.hooks]
post_acquire = "npm ci"
pre_release = "rm -rf node_modules/.cache"
[provide.hooks.os.windows]
post_acquire = "npm ci --windows"
[composition]
order = "foundation"
`

func TestParseToolchainInstallAndHooks(t *testing.T) {
	var m kitManifestTOML
	if err := toml.Unmarshal([]byte(toolchainManifestTOML), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// toolchain_install.<os> now parsed (previously dropped).
	if got := m.Provide.ToolchainInstall["linux"]["node"]; got == "" {
		t.Error("toolchain_install.linux.node not parsed")
	}
	if got := m.Provide.ToolchainInstall["macos"]["node"]; got != "brew install node@20" {
		t.Errorf("toolchain_install.macos.node = %q, want brew install node@20", got)
	}

	// commands_override.<os> parsed.
	if got := m.Provide.CommandsOverride["macos"]["build"]; got != "npm run build:mac" {
		t.Errorf("commands_override.macos.build = %q", got)
	}

	// hooks parsed (generic + OS-keyed overlay).
	if m.Provide.Hooks.PostAcquire != "npm ci" {
		t.Errorf("hooks.post_acquire = %q, want npm ci", m.Provide.Hooks.PostAcquire)
	}
	if m.Provide.Hooks.PreRelease != "rm -rf node_modules/.cache" {
		t.Errorf("hooks.pre_release = %q", m.Provide.Hooks.PreRelease)
	}
	if got := m.Provide.Hooks.OS["windows"].PostAcquire; got != "npm ci --windows" {
		t.Errorf("hooks.os.windows.post_acquire = %q", got)
	}

	// not_files parsed.
	if len(m.Detect.NotFiles) != 1 || m.Detect.NotFiles[0] != "go.mod" {
		t.Errorf("detect.not_files = %v", m.Detect.NotFiles)
	}
}

func TestManifestToKitManifestCarriesToolchain(t *testing.T) {
	var m kitManifestTOML
	if err := toml.Unmarshal([]byte(toolchainManifestTOML), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	k := manifestToKit(m)
	mf := manifestToKitManifest(m, k)
	if mf.ToolchainInstall["linux"]["node"] == "" {
		t.Error("wire KitManifest missing toolchain_install.linux.node")
	}
	if mf.Hooks == nil || mf.Hooks.PostAcquire != "npm ci" {
		t.Errorf("wire KitManifest hooks not carried: %+v", mf.Hooks)
	}
	if mf.Hooks.OS["windows"].PostAcquire != "npm ci --windows" {
		t.Error("wire KitManifest OS-keyed hook not carried")
	}
}

func TestDetectForRepoMatchesAndOrders(t *testing.T) {
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)

	// A framework kit that detects on next.config.ts.
	const nextManifest = `
api = "donmai.dev/v1"
[kit]
id = "nextjs"
version = "1.0.0"
name = "Next.js"
priority = 40
[supports]
os = ["linux", "macos"]
[detect]
files = ["next.config.ts"]
[provide.toolchain_install.linux]
pnpm = "npm i -g pnpm"
[composition]
order = "framework"
`
	writeManifest(t, scanDir, "nextjs.kit.toml", nextManifest)

	// Repo root with package.json (matches TS) + next.config.ts (matches next).
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "next.config.ts"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewKitRegistry([]string{scanDir})
	views, err := reg.DetectForRepo(repo, "linux")
	if err != nil {
		t.Fatalf("DetectForRepo: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("expected 2 matched kits, got %d: %+v", len(views), views)
	}
	// foundation (typescript) before framework (nextjs).
	if views[0].ID != "typescript" || views[1].ID != "nextjs" {
		t.Errorf("order = [%s, %s], want [typescript, nextjs]", views[0].ID, views[1].ID)
	}
}

func TestDetectForRepoNotFilesExcludes(t *testing.T) {
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)

	// Repo has package.json AND go.mod → not_files=["go.mod"] excludes TS.
	repo := t.TempDir()
	for _, f := range []string{"package.json", "go.mod"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewKitRegistry([]string{scanDir})
	views, err := reg.DetectForRepo(repo, "linux")
	if err != nil {
		t.Fatalf("DetectForRepo: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("not_files should exclude the kit, got %+v", views)
	}
}

func TestDetectForRepoSupportsOSShortCircuit(t *testing.T) {
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewKitRegistry([]string{scanDir})
	// windows not in [supports].os (linux, macos) → no match.
	views, err := reg.DetectForRepo(repo, "windows")
	if err != nil {
		t.Fatalf("DetectForRepo: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("supports-os short-circuit failed, got %+v", views)
	}
}

func TestDetectForRepoFoundationConflict(t *testing.T) {
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)
	// Second foundation kit that also matches package.json.
	const otherFoundation = `
api = "donmai.dev/v1"
[kit]
id = "other-foundation"
version = "1.0.0"
[supports]
os = ["linux"]
[detect]
files = ["package.json"]
[composition]
order = "foundation"
`
	writeManifest(t, scanDir, "other.kit.toml", otherFoundation)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewKitRegistry([]string{scanDir})
	_, err := reg.DetectForRepo(repo, "linux")
	if err == nil {
		t.Fatal("expected ErrKitFoundationConflict for two matching foundation kits")
	}
}
