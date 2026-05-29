package kit

import (
	"reflect"
	"testing"
)

func tsView() ManifestView {
	return ManifestView{
		ID:          "typescript",
		Version:     "1.0.0",
		Priority:    50,
		Order:       "foundation",
		SupportedOS: []string{"linux", "macos"},
		ToolchainInstall: map[string]map[string]string{
			"linux": {"node": "install-node-linux"},
			"macos": {"node": "brew install node@20"},
		},
		Hooks: HooksView{
			PostAcquire: "npm ci",
			PreRelease:  "rm -rf node_modules/.cache",
		},
	}
}

func nextView() ManifestView {
	return ManifestView{
		ID:          "nextjs",
		Version:     "1.0.0",
		Priority:    40,
		Order:       "framework",
		SupportedOS: []string{"linux", "macos"},
		ToolchainInstall: map[string]map[string]string{
			"linux": {"pnpm": "install-pnpm-linux"},
		},
		Hooks: HooksView{PostAcquire: "pnpm install"},
	}
}

func TestComposeUnionAndOrder(t *testing.T) {
	// Pass framework before foundation to prove Compose trusts caller
	// order — DetectForRepo is the one that sorts. Here we feed sorted.
	views := SortManifests([]ManifestView{nextView(), tsView()})
	if views[0].ID != "typescript" {
		t.Fatalf("SortManifests: foundation should sort first, got %q", views[0].ID)
	}

	d, err := Compose(views, OSLinux)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	// Union across kits: both node (foundation) and pnpm (framework) run,
	// foundation first.
	wantInstall := []string{"install-node-linux", "install-pnpm-linux"}
	if !reflect.DeepEqual(d.ToolchainInstall, wantInstall) {
		t.Errorf("ToolchainInstall = %v, want %v (foundation-first union)", d.ToolchainInstall, wantInstall)
	}
	wantPost := []string{"npm ci", "pnpm install"}
	if !reflect.DeepEqual(d.PostAcquire, wantPost) {
		t.Errorf("PostAcquire = %v, want %v (foundation-first)", d.PostAcquire, wantPost)
	}
	// pre_release from foundation only.
	wantPre := []string{"rm -rf node_modules/.cache"}
	if !reflect.DeepEqual(d.PreRelease, wantPre) {
		t.Errorf("PreRelease = %v, want %v", d.PreRelease, wantPre)
	}
	wantKits := []string{"typescript@1.0.0", "nextjs@1.0.0"}
	if !reflect.DeepEqual(d.Kits, wantKits) {
		t.Errorf("Kits = %v, want %v", d.Kits, wantKits)
	}
}

func TestComposeOSSelection(t *testing.T) {
	d, err := Compose([]ManifestView{tsView()}, OSMacOS)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(d.ToolchainInstall) != 1 || d.ToolchainInstall[0] != "brew install node@20" {
		t.Errorf("macos install = %v, want [brew install node@20]", d.ToolchainInstall)
	}
}

func TestComposeUnsupportedOSContributesNothing(t *testing.T) {
	// A windows target: tsView supports only linux/macos → no contribution.
	d, err := Compose([]ManifestView{tsView()}, OSWindows)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if !d.IsEmpty() {
		t.Errorf("expected empty demand for unsupported OS, got %+v", d)
	}
	if len(d.Kits) != 0 {
		t.Errorf("unsupported-OS kit should not appear in Kits, got %v", d.Kits)
	}
}

func TestComposeDeterministicKeyOrder(t *testing.T) {
	v := ManifestView{
		ID:    "multi",
		Order: "foundation",
		ToolchainInstall: map[string]map[string]string{
			"linux": {"zzz": "cmd-z", "aaa": "cmd-a", "mmm": "cmd-m"},
		},
	}
	d, err := Compose([]ManifestView{v}, OSLinux)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// Keys sorted aaa, mmm, zzz → commands in that order, deterministically.
	want := []string{"cmd-a", "cmd-m", "cmd-z"}
	if !reflect.DeepEqual(d.ToolchainInstall, want) {
		t.Errorf("install order = %v, want %v (sorted keys)", d.ToolchainInstall, want)
	}
}

func TestComposeDedupIdenticalCommand(t *testing.T) {
	a := ManifestView{ID: "a", Order: "foundation", ToolchainInstall: map[string]map[string]string{"linux": {"node": "install-node"}}}
	b := ManifestView{ID: "b", Order: "framework", ToolchainInstall: map[string]map[string]string{"linux": {"node": "install-node"}}}
	d, err := Compose([]ManifestView{a, b}, OSLinux)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(d.ToolchainInstall) != 1 {
		t.Errorf("identical commands across kits should dedup, got %v", d.ToolchainInstall)
	}
}

func TestComposeOSKeyedHookOverride(t *testing.T) {
	v := ManifestView{
		ID:    "k",
		Order: "foundation",
		Hooks: HooksView{
			PostAcquire: "generic-post",
			OS: map[string]HookOSView{
				"linux": {PostAcquire: "linux-post"},
			},
		},
	}
	d, err := Compose([]ManifestView{v}, OSLinux)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(d.PostAcquire) != 1 || d.PostAcquire[0] != "linux-post" {
		t.Errorf("OS-keyed hook should win, got %v", d.PostAcquire)
	}
	// macos has no override → generic.
	dm, _ := Compose([]ManifestView{v}, OSMacOS)
	if len(dm.PostAcquire) != 1 || dm.PostAcquire[0] != "generic-post" {
		t.Errorf("generic hook should apply for un-overridden OS, got %v", dm.PostAcquire)
	}
}

func TestComposeEmptyTargetOSErrors(t *testing.T) {
	if _, err := Compose([]ManifestView{tsView()}, ""); err == nil {
		t.Error("expected error for empty targetOS")
	}
}

func TestResolveOS(t *testing.T) {
	cases := map[string]string{"linux": OSLinux, "darwin": OSMacOS, "windows": OSWindows}
	for goos, want := range cases {
		got, err := ResolveOS(goos)
		if err != nil || got != want {
			t.Errorf("ResolveOS(%q) = (%q,%v), want (%q,nil)", goos, got, err, want)
		}
	}
	if _, err := ResolveOS("plan9"); err == nil {
		t.Error("expected error for unsupported GOOS")
	}
}

func TestSortManifestsTieBreak(t *testing.T) {
	// Same order group, different priority then id.
	views := []ManifestView{
		{ID: "b", Order: "foundation", Priority: 10},
		{ID: "a", Order: "foundation", Priority: 10},
		{ID: "c", Order: "foundation", Priority: 20},
	}
	got := SortManifests(views)
	wantIDs := []string{"c", "a", "b"} // priority 20 first; then id a<b
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("sorted[%d] = %q, want %q (full order: %v)", i, got[i].ID, w, ids(got))
		}
	}
}

func ids(vs []ManifestView) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.ID
	}
	return out
}

func TestToolchainDemandIsEmpty(t *testing.T) {
	if !(&ToolchainDemand{}).IsEmpty() {
		t.Error("zero demand should be empty")
	}
	if (&ToolchainDemand{ToolchainInstall: []string{"x"}}).IsEmpty() {
		t.Error("demand with install step should not be empty")
	}
	var nilD *ToolchainDemand
	if !nilD.IsEmpty() {
		t.Error("nil demand should be empty")
	}
}
