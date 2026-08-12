package kit

import (
	"reflect"
	"testing"
)

func TestDeriveDemand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		views     []ManifestView
		wantOS    []string
		wantArch  []string
		wantLanes []LaneDemand
	}{
		{
			name:     "no kits: full known universe, additive no-op",
			views:    nil,
			wantOS:   []string{OSLinux, OSMacOS, OSWindows},
			wantArch: []string{ArchARM64, ArchX86_64},
		},
		{
			name: "kits declare no supports: full known universe (permissive-empty)",
			views: []ManifestView{
				{ID: "a"},
				{ID: "b"},
			},
			wantOS:   []string{OSLinux, OSMacOS, OSWindows},
			wantArch: []string{ArchARM64, ArchX86_64},
		},
		{
			name: "single kit narrows to its own supports",
			views: []ManifestView{
				{ID: "swift", SupportedOS: []string{OSLinux, OSMacOS}, SupportedArch: []string{ArchARM64, ArchX86_64}},
			},
			wantOS:   []string{OSLinux, OSMacOS},
			wantArch: []string{ArchARM64, ArchX86_64},
		},
		{
			name: "two kits intersect",
			views: []ManifestView{
				{ID: "swift", SupportedOS: []string{OSLinux, OSMacOS}},
				{ID: "xcode-tools", SupportedOS: []string{OSMacOS}},
			},
			wantOS:   []string{OSMacOS},
			wantArch: []string{ArchARM64, ArchX86_64}, // neither kit constrains arch
		},
		{
			name: "conflicting kits: unsatisfiable, non-nil empty OS",
			views: []ManifestView{
				{ID: "linux-only", SupportedOS: []string{OSLinux}},
				{ID: "macos-only", SupportedOS: []string{OSMacOS}},
			},
			wantOS:   []string{},
			wantArch: []string{ArchARM64, ArchX86_64},
		},
		{
			name: "unconstrained kit does not narrow a constrained sibling",
			views: []ManifestView{
				{ID: "generic"}, // no supports declared -> "any"
				{ID: "macos-only", SupportedOS: []string{OSMacOS}},
			},
			wantOS:   []string{OSMacOS},
			wantArch: []string{ArchARM64, ArchX86_64},
		},
		{
			name: "kit with a lane surfaces LaneDemand, does not narrow top-level OS",
			views: []ManifestView{
				{
					ID:          "default/swift",
					SupportedOS: []string{OSLinux, OSMacOS},
					Lanes: []LaneView{
						{Name: "ios-app-build", OS: []string{OSMacOS}},
					},
				},
			},
			wantOS:   []string{OSLinux, OSMacOS},
			wantArch: []string{ArchARM64, ArchX86_64},
			wantLanes: []LaneDemand{
				{Kit: "default/swift", Lane: "ios-app-build", OS: []string{OSMacOS}},
			},
		},
		{
			name: "lanes sorted by kit then lane for determinism",
			views: []ManifestView{
				{ID: "b-kit", Lanes: []LaneView{{Name: "z-lane", OS: []string{OSMacOS}}}},
				{ID: "a-kit", Lanes: []LaneView{{Name: "y-lane", OS: []string{OSLinux}}, {Name: "a-lane", OS: []string{OSWindows}}}},
			},
			wantOS:   []string{OSLinux, OSMacOS, OSWindows},
			wantArch: []string{ArchARM64, ArchX86_64},
			wantLanes: []LaneDemand{
				{Kit: "a-kit", Lane: "a-lane", OS: []string{OSWindows}},
				{Kit: "a-kit", Lane: "y-lane", OS: []string{OSLinux}},
				{Kit: "b-kit", Lane: "z-lane", OS: []string{OSMacOS}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := DeriveDemand(tt.views)
			if !reflect.DeepEqual(got.OS, tt.wantOS) {
				t.Errorf("OS = %#v, want %#v", got.OS, tt.wantOS)
			}
			if !reflect.DeepEqual(got.Arch, tt.wantArch) {
				t.Errorf("Arch = %#v, want %#v", got.Arch, tt.wantArch)
			}
			if len(tt.wantLanes) == 0 {
				if len(got.Lanes) != 0 {
					t.Errorf("Lanes = %+v, want none", got.Lanes)
				}
				return
			}
			if !reflect.DeepEqual(got.Lanes, tt.wantLanes) {
				t.Errorf("Lanes = %+v, want %+v", got.Lanes, tt.wantLanes)
			}
		})
	}
}

func TestPlacementDemand_IsUnsatisfiable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    PlacementDemand
		want bool
	}{
		{"zero value (never derived)", PlacementDemand{}, false},
		{"unconstrained", PlacementDemand{OS: []string{OSLinux, OSMacOS, OSWindows}}, false},
		{"narrowed but non-empty", PlacementDemand{OS: []string{OSMacOS}}, false},
		{"non-nil empty: unsatisfiable", PlacementDemand{OS: []string{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.d.IsUnsatisfiable(); got != tt.want {
				t.Errorf("IsUnsatisfiable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlacementDemand_NarrowsOSAndArch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		d            PlacementDemand
		wantNarrowOS bool
		wantNarrowAr bool
	}{
		{"zero value never derived", PlacementDemand{}, false, false},
		{"full universe", PlacementDemand{OS: []string{OSLinux, OSMacOS, OSWindows}, Arch: []string{ArchARM64, ArchX86_64}}, false, false},
		{"os narrowed", PlacementDemand{OS: []string{OSMacOS}, Arch: []string{ArchARM64, ArchX86_64}}, true, false},
		{"arch narrowed", PlacementDemand{OS: []string{OSLinux, OSMacOS, OSWindows}, Arch: []string{ArchARM64}}, false, true},
		{"unsatisfiable os narrows", PlacementDemand{OS: []string{}}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.d.NarrowsOS(); got != tt.wantNarrowOS {
				t.Errorf("NarrowsOS() = %v, want %v", got, tt.wantNarrowOS)
			}
			if got := tt.d.NarrowsArch(); got != tt.wantNarrowAr {
				t.Errorf("NarrowsArch() = %v, want %v", got, tt.wantNarrowAr)
			}
		})
	}
}

func TestPlacementDemand_EffectiveOS(t *testing.T) {
	t.Parallel()

	swiftKitDemand := DeriveDemand([]ManifestView{
		{
			ID:          "default/swift",
			SupportedOS: []string{OSLinux, OSMacOS},
			Lanes: []LaneView{
				{Name: "ios-app-build", OS: []string{OSMacOS}},
			},
		},
	})

	tests := []struct {
		name    string
		d       PlacementDemand
		engaged []string
		want    []string
	}{
		{
			name: "never derived: nil signals no filter",
			d:    PlacementDemand{},
			want: nil,
		},
		{
			name: "no engaged lanes: top-level OS unchanged",
			d:    swiftKitDemand,
			want: []string{OSLinux, OSMacOS},
		},
		{
			name:    "engaging the macOS-locked lane narrows to macOS",
			d:       swiftKitDemand,
			engaged: []string{"ios-app-build"},
			want:    []string{OSMacOS},
		},
		{
			name:    "unknown engaged lane is ignored",
			d:       swiftKitDemand,
			engaged: []string{"does-not-exist"},
			want:    []string{OSLinux, OSMacOS},
		},
		{
			name: "lane with empty OS does not narrow (permissive-empty)",
			d: PlacementDemand{
				OS:    []string{OSLinux, OSMacOS},
				Lanes: []LaneDemand{{Kit: "k", Lane: "any-lane"}},
			},
			engaged: []string{"any-lane"},
			want:    []string{OSLinux, OSMacOS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.d.EffectiveOS(tt.engaged...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EffectiveOS(%v) = %#v, want %#v", tt.engaged, got, tt.want)
			}
		})
	}
}

// TestPlacementDemand_EffectiveOS_PreservesUnsatisfiableNonNilEmpty is a
// regression test: append([]string(nil), emptySlice...) returns nil in Go
// (no growth needed), which would silently turn the unsatisfiable
// non-nil-empty OS signal into "never derived" (nil) — a real bug caught by
// the daemon-level FilterCandidatesByDemand test.
func TestPlacementDemand_EffectiveOS_PreservesUnsatisfiableNonNilEmpty(t *testing.T) {
	t.Parallel()
	d := PlacementDemand{OS: []string{}}
	got := d.EffectiveOS()
	if got == nil {
		t.Fatal("EffectiveOS() = nil, want non-nil empty slice (unsatisfiable, not 'never derived')")
	}
	if len(got) != 0 {
		t.Errorf("EffectiveOS() = %v, want empty", got)
	}
}

func TestPlacementDemand_EffectiveArch(t *testing.T) {
	t.Parallel()
	d := PlacementDemand{
		Arch: []string{ArchARM64, ArchX86_64},
		Lanes: []LaneDemand{
			{Kit: "k", Lane: "gpu-lane", Arch: []string{ArchARM64}},
		},
	}
	if got := d.EffectiveArch(); !reflect.DeepEqual(got, []string{ArchARM64, ArchX86_64}) {
		t.Errorf("EffectiveArch() unengaged = %#v, want full arch set", got)
	}
	if got := d.EffectiveArch("gpu-lane"); !reflect.DeepEqual(got, []string{ArchARM64}) {
		t.Errorf("EffectiveArch(gpu-lane) = %#v, want [arm64]", got)
	}
}
