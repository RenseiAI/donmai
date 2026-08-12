package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/internal/kit"
)

const macosOnlyManifestTOML = `
api = "donmai.dev/v1"
[kit]
id = "default/xcode-tools"
version = "1.0.0"
name = "Xcode Tools"
priority = 60
[supports]
os = ["macos"]
[detect]
files = ["Package.swift"]
[composition]
order = "framework"
`

const swiftManifestWithLaneTOML = `
api = "donmai.dev/v1"
[kit]
id = "default/swift"
version = "1.0.0"
name = "Swift"
priority = 70
[supports]
os = ["linux", "macos"]
arch = ["x86_64", "arm64"]
[detect]
files = ["Package.swift"]
[provide.commands]
build = "swift build"
[[provide.lanes]]
name = "ios-app-build"
os = ["macos"]
[composition]
order = "foundation"
`

func TestDetectForRepoAnyOS_IgnoresOSShortCircuit(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML) // supports linux, macos

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewKitRegistry([]string{scanDir})

	// DetectForRepo short-circuits: windows isn't in [supports].os.
	viaOSGated, err := reg.DetectForRepo(repo, "windows")
	if err != nil {
		t.Fatalf("DetectForRepo: %v", err)
	}
	if len(viaOSGated) != 0 {
		t.Fatalf("DetectForRepo(windows) = %+v, want none (short-circuit)", viaOSGated)
	}

	// DetectForRepoAnyOS must still see it — no OS filter applied.
	viaAnyOS, err := reg.DetectForRepoAnyOS(repo)
	if err != nil {
		t.Fatalf("DetectForRepoAnyOS: %v", err)
	}
	if len(viaAnyOS) != 1 || viaAnyOS[0].ID != "typescript" {
		t.Fatalf("DetectForRepoAnyOS = %+v, want [typescript]", viaAnyOS)
	}
	if !reflect.DeepEqual(viaAnyOS[0].SupportedOS, []string{"linux", "macos"}) {
		t.Errorf("SupportedOS = %v, want [linux macos]", viaAnyOS[0].SupportedOS)
	}
}

func TestDemandForRepo_NoKitsMatch_Unconstrained(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)

	repo := t.TempDir() // no package.json -> nothing detects

	reg := NewKitRegistry([]string{scanDir})
	demand, err := reg.DemandForRepo(repo)
	if err != nil {
		t.Fatalf("DemandForRepo: %v", err)
	}
	want := kit.DeriveDemand(nil)
	if !reflect.DeepEqual(demand, want) {
		t.Errorf("DemandForRepo (no kits matched) = %+v, want %+v (unconstrained)", demand, want)
	}
	if demand.NarrowsOS() {
		t.Errorf("unconstrained demand must not narrow OS: %+v", demand)
	}
}

func TestDemandForRepo_IntersectsAcrossOSConstrainedKits(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	// Foundation kit supports linux+macos; framework kit is macos-only.
	// Detection for both targets Package.swift so a single repo matches both.
	writeManifest(t, scanDir, "swift.kit.toml", swiftManifestWithLaneTOML)
	writeManifest(t, scanDir, "xcode-tools.kit.toml", macosOnlyManifestTOML)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Package.swift"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewKitRegistry([]string{scanDir})
	demand, err := reg.DemandForRepo(repo)
	if err != nil {
		t.Fatalf("DemandForRepo: %v", err)
	}
	if !reflect.DeepEqual(demand.OS, []string{"macos"}) {
		t.Fatalf("demand.OS = %v, want [macos] (intersection of [linux macos] and [macos])", demand.OS)
	}
	if !demand.NarrowsOS() {
		t.Error("NarrowsOS() = false, want true (macos-only is a proper subset of the known universe)")
	}
}

func TestDemandForRepo_SurfacesLaneFromParsedManifest(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "swift.kit.toml", swiftManifestWithLaneTOML)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Package.swift"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewKitRegistry([]string{scanDir})
	demand, err := reg.DemandForRepo(repo)
	if err != nil {
		t.Fatalf("DemandForRepo: %v", err)
	}
	if len(demand.Lanes) != 1 {
		t.Fatalf("Lanes = %+v, want 1 entry parsed from [[provide.lanes]]", demand.Lanes)
	}
	got := demand.Lanes[0]
	want := kit.LaneDemand{Kit: "default/swift", Lane: "ios-app-build", OS: []string{"macos"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lanes[0] = %+v, want %+v", got, want)
	}

	// Top-level demand stays broad (swift itself supports linux+macos);
	// only engaging the lane narrows it.
	if !reflect.DeepEqual(demand.OS, []string{"linux", "macos"}) {
		t.Errorf("demand.OS = %v, want [linux macos] (lane does not narrow top-level demand)", demand.OS)
	}
	if got := demand.EffectiveOS("ios-app-build"); !reflect.DeepEqual(got, []string{"macos"}) {
		t.Errorf("EffectiveOS(ios-app-build) = %v, want [macos]", got)
	}
}

func TestDemandForRepo_FoundationConflictPropagates(t *testing.T) {
	t.Parallel()
	scanDir := t.TempDir()
	writeManifest(t, scanDir, "typescript.kit.toml", toolchainManifestTOML)
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
	if _, err := reg.DemandForRepo(repo); err == nil {
		t.Fatal("expected ErrKitFoundationConflict to propagate from DetectForRepoAnyOS")
	}
}
