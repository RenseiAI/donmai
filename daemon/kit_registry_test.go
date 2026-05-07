package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

const validKitTOML = `api = "rensei.dev/v1"

[kit]
id = "spring/java"
version = "1.0.0"
name = "Spring Java"
description = "Maven/Gradle Spring Boot projects"
author = "Spring Framework Team"
authorIdentity = "did:web:spring.io"
license = "Apache-2.0"
homepage = "https://spring.io"
priority = 80

[supports]
os = ["linux", "macos"]
arch = ["x86_64", "arm64"]

[requires]
rensei = "^1.0"
capabilities = ["workarea:toolchain"]

[detect]
files = ["pom.xml", "build.gradle"]
exec = "bin/detect"

[detect.toolchain]
java = "17"

[provide.commands]
build = "./mvnw compile"
test = "./mvnw test"

[[provide.tool_permissions]]
shell = "./mvnw *"

[[provide.prompt_fragments]]
partial = "spring-conventions"
when = ["development"]
file = "partials/spring-conventions.yaml"

[[provide.mcp_servers]]
name = "spring-context"
command = "./bin/spring-mcp"

[[provide.skills]]
file = "skills/spring-test-debugging/SKILL.md"

[[provide.agents]]
id = "spring-test-fixer"
template = "agents/spring-test-fixer.yaml"

[composition]
conflicts_with = ["maven-only"]
composes_with = ["docker-compose"]
order = "framework"
`

const malformedKitTOML = `this is not valid TOML at all = = = [
`

const emptyIDKitTOML = `api = "rensei.dev/v1"
[kit]
version = "1.0.0"
name = "no id"
`

// writeManifest writes content to dir/<name>.kit.toml for the test.
func writeManifest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name+".kit.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestKitRegistry_DefaultScanPath(t *testing.T) {
	r := NewKitRegistry(nil)
	paths := r.ScanPaths()
	if len(paths) != 1 {
		t.Fatalf("want 1 default scan path, got %d: %v", len(paths), paths)
	}
	if !strings.HasSuffix(paths[0], "/.rensei/kits") {
		t.Errorf("default scan path %q does not end in /.rensei/kits", paths[0])
	}
}

func TestKitRegistry_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	r := NewKitRegistry([]string{dir})
	kits := r.List()
	if len(kits) != 0 {
		t.Errorf("want empty list from empty dir, got %d kits", len(kits))
	}
}

func TestKitRegistry_ListMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	r := NewKitRegistry([]string{missing})
	kits := r.List()
	if len(kits) != 0 {
		t.Errorf("want empty list from missing dir, got %d kits", len(kits))
	}
}

func TestKitRegistry_ListValidManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "spring", validKitTOML)
	r := NewKitRegistry([]string{dir})

	kits := r.List()
	if len(kits) != 1 {
		t.Fatalf("want 1 kit, got %d", len(kits))
	}
	k := kits[0]
	if k.ID != "spring/java" {
		t.Errorf("ID: want spring/java, got %q", k.ID)
	}
	if k.Name != "Spring Java" {
		t.Errorf("Name: want Spring Java, got %q", k.Name)
	}
	if k.Version != "1.0.0" {
		t.Errorf("Version: want 1.0.0, got %q", k.Version)
	}
	if k.AuthorID != "did:web:spring.io" {
		t.Errorf("AuthorID: want did:web:spring.io, got %q", k.AuthorID)
	}
	if k.Priority != 80 {
		t.Errorf("Priority: want 80, got %d", k.Priority)
	}
	if k.Source != afclient.KitSourceLocal {
		t.Errorf("Source: want local, got %q", k.Source)
	}
	if k.Status != afclient.KitStatusActive {
		t.Errorf("Status: want active, got %q", k.Status)
	}
	if k.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned, got %q", k.Trust)
	}
	if !k.ProvidesCommands {
		t.Error("want ProvidesCommands=true")
	}
	if !k.ProvidesPrompts {
		t.Error("want ProvidesPrompts=true")
	}
	if !k.ProvidesTools {
		t.Error("want ProvidesTools=true")
	}
	if !k.ProvidesMCPServers {
		t.Error("want ProvidesMCPServers=true")
	}
	if !k.ProvidesSkills {
		t.Error("want ProvidesSkills=true")
	}
	if !k.ProvidesAgents {
		t.Error("want ProvidesAgents=true")
	}
	if k.ProvidesA2ASkills {
		t.Error("want ProvidesA2ASkills=false (none declared)")
	}
	if len(k.DetectFiles) != 2 {
		t.Errorf("DetectFiles: want 2, got %d", len(k.DetectFiles))
	}
	if k.DetectExec != "bin/detect" {
		t.Errorf("DetectExec: want bin/detect, got %q", k.DetectExec)
	}
}

func TestKitRegistry_MalformedExcluded(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "broken", malformedKitTOML)
	writeManifest(t, dir, "spring", validKitTOML)
	r := NewKitRegistry([]string{dir})

	kits := r.List()
	if len(kits) != 1 {
		t.Fatalf("want 1 kit (malformed excluded), got %d", len(kits))
	}
	if kits[0].ID != "spring/java" {
		t.Errorf("ID: want spring/java, got %q", kits[0].ID)
	}
}

func TestKitRegistry_EmptyIDExcluded(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "no-id", emptyIDKitTOML)
	r := NewKitRegistry([]string{dir})
	kits := r.List()
	if len(kits) != 0 {
		t.Errorf("want 0 kits (no id excluded), got %d", len(kits))
	}
}

func TestKitRegistry_MultipleScanPathsOverride(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	writeManifest(t, dirA, "spring", validKitTOML)
	// Second scan path overrides on id collision per 005 § "Daemon kit registry".
	override := strings.Replace(validKitTOML, `version = "1.0.0"`, `version = "2.0.0"`, 1)
	writeManifest(t, dirB, "spring", override)

	r := NewKitRegistry([]string{dirA, dirB})
	kits := r.List()
	if len(kits) != 1 {
		t.Fatalf("want 1 kit after override, got %d", len(kits))
	}
	if kits[0].Version != "2.0.0" {
		t.Errorf("override: want version 2.0.0, got %q", kits[0].Version)
	}
}

func TestKitRegistry_GetKnownAndUnknown(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "spring", validKitTOML)
	r := NewKitRegistry([]string{dir})

	m, err := r.Get("spring/java")
	if err != nil {
		t.Fatalf("Get known: %v", err)
	}
	if m.ID != "spring/java" {
		t.Errorf("ID: want spring/java, got %q", m.ID)
	}
	if len(m.SupportedOS) != 2 {
		t.Errorf("SupportedOS: want 2, got %d", len(m.SupportedOS))
	}
	if m.RequiresRensei != "^1.0" {
		t.Errorf("RequiresRensei: want ^1.0, got %q", m.RequiresRensei)
	}
	if m.Order != "framework" {
		t.Errorf("Order: want framework, got %q", m.Order)
	}
	if len(m.Commands) != 2 {
		t.Errorf("Commands: want 2, got %d", len(m.Commands))
	}
	if m.DetectToolchain["java"] != "17" {
		t.Errorf("DetectToolchain[java]: want 17, got %q", m.DetectToolchain["java"])
	}

	if _, err := r.Get("does/not/exist"); !errors.Is(err, ErrKitNotFound) {
		t.Errorf("Get unknown: want ErrKitNotFound, got %v", err)
	}
}

func TestKitRegistry_DisableEnable(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "spring", validKitTOML)
	r := NewKitRegistry([]string{dir})

	k, err := r.Disable("spring/java")
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if k.Status != afclient.KitStatusDisabled {
		t.Errorf("Disable: want disabled, got %q", k.Status)
	}

	// Persisted state survives a fresh registry.
	r2 := NewKitRegistry([]string{dir})
	listed := r2.List()
	if len(listed) != 1 || listed[0].Status != afclient.KitStatusDisabled {
		t.Fatalf("disabled state did not persist: %+v", listed)
	}

	k, err = r2.Enable("spring/java")
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if k.Status != afclient.KitStatusActive {
		t.Errorf("Enable: want active, got %q", k.Status)
	}

	if _, err := r2.Disable("does/not/exist"); !errors.Is(err, ErrKitNotFound) {
		t.Errorf("Disable unknown: want ErrKitNotFound, got %v", err)
	}
	if _, err := r2.Enable("does/not/exist"); !errors.Is(err, ErrKitNotFound) {
		t.Errorf("Enable unknown: want ErrKitNotFound, got %v", err)
	}
}

func TestKitRegistry_VerifySignature(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "spring", validKitTOML)
	r := NewKitRegistry([]string{dir})

	res, err := r.VerifySignature("spring/java")
	if err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
	if res.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned (Wave 9 caveat), got %q", res.Trust)
	}
	if !res.OK {
		t.Error("OK: want true (verification ran)")
	}
	if res.SignerID != "did:web:spring.io" {
		t.Errorf("SignerID: want did:web:spring.io, got %q", res.SignerID)
	}

	if _, err := r.VerifySignature("does/not/exist"); !errors.Is(err, ErrKitNotFound) {
		t.Errorf("VerifySignature unknown: want ErrKitNotFound, got %v", err)
	}
}

func TestKitRegistry_InstallUnimplemented(t *testing.T) {
	r := NewKitRegistry([]string{t.TempDir()})
	if _, err := r.Install("spring/java", afclient.KitInstallRequest{}); !errors.Is(err, ErrKitInstallUnimplemented) {
		t.Errorf("Install: want ErrKitInstallUnimplemented, got %v", err)
	}
}

func TestKitRegistry_ListSourcesAndToggle(t *testing.T) {
	dir := t.TempDir()
	r := NewKitRegistry([]string{dir})
	sources := r.ListSources()
	if len(sources) != 6 {
		t.Fatalf("want 6 sources (federation order), got %d", len(sources))
	}
	if sources[0].Name != "local" {
		t.Errorf("first source: want local, got %q", sources[0].Name)
	}
	for _, s := range sources {
		if !s.Enabled {
			t.Errorf("source %q: want enabled by default, got disabled", s.Name)
		}
	}

	src, err := r.DisableSource("tessl")
	if err != nil {
		t.Fatalf("DisableSource: %v", err)
	}
	if src.Enabled {
		t.Errorf("DisableSource: want Enabled=false, got true")
	}

	// Persistence
	r2 := NewKitRegistry([]string{dir})
	for _, s := range r2.ListSources() {
		if s.Name == "tessl" && s.Enabled {
			t.Error("disable did not persist for tessl")
		}
	}

	src, err = r2.EnableSource("tessl")
	if err != nil {
		t.Fatalf("EnableSource: %v", err)
	}
	if !src.Enabled {
		t.Error("EnableSource: want Enabled=true")
	}

	if _, err := r.EnableSource("nope"); !errors.Is(err, ErrKitSourceNotFound) {
		t.Errorf("EnableSource unknown: want ErrKitSourceNotFound, got %v", err)
	}
	if _, err := r.DisableSource("nope"); !errors.Is(err, ErrKitSourceNotFound) {
		t.Errorf("DisableSource unknown: want ErrKitSourceNotFound, got %v", err)
	}
}

func TestKitRegistry_HomePathExpansion(t *testing.T) {
	// Just verify the expansion is wired; full path correctness depends on
	// $HOME which we don't need to mutate.
	r := NewKitRegistry([]string{"~/agentfactory-test-kits"})
	for _, p := range r.ScanPaths() {
		if strings.HasPrefix(p, "~/") {
			t.Errorf("scan path not expanded: %q", p)
		}
	}
}

func TestKitRegistry_NonTOMLSuffixIgnored(t *testing.T) {
	dir := t.TempDir()
	// Drop a file with the wrong suffix; it must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "spring.toml"), []byte(validKitTOML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := NewKitRegistry([]string{dir})
	if got := r.List(); len(got) != 0 {
		t.Errorf("want 0 kits, got %d", len(got))
	}
}
