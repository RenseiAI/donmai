// Package kit — demand.go: pre-placement toolchain-demand derivation (K1.4).
//
// 001-layered-execution-model.md § "Layer 4 — Composition" names a Kit's
// toolchain demand as a signal to the scheduler. Until this file, the only
// place that signal was evaluated was Compose/ComposeForTarget — both of
// which require a target OS to already be chosen (005-kit-manifest-spec.md
// § "Platform compatibility" gates [supports].os strictly AFTER a target is
// picked). That makes the [supports] short-circuit a placement-time
// correctness check, never a placement INPUT: a scheduler cannot ask "which
// of my candidates can even run this composition" before it has already
// picked one.
//
// PlacementDemand closes that gap. DeriveDemand computes the OS/arch set a
// whole kit composition can run on — and any narrower OS/arch lanes within
// it — from the kit set ALONE, with no target OS required. A caller (the
// daemon's composition surface, an embedding platform, or the local
// scheduler) derives this once the session's kit set is known and applies
// it as a stage-2 viability filter (ADR-2026-08-12-placement-composition-
// law-and-single-fallback-rule.md D1.2) before landing on a candidate,
// instead of discovering the incompatibility only once Compose/
// ComposeForTarget runs against an already-chosen (and possibly wrong) OS.
//
// # Proposed manifest declaration shape for OS-locked command lanes
//
// The swift kit (../../donmai-kits/kits/swift/kit.toml) documents an
// iOS-app-build/test/screenshot lane that exists ONLY on the macOS capacity
// pool, purely in prose — nothing in the manifest schema lets a kit declare
// a NAMED command lane that exists on a subset of its own [supports].os.
// [provide.commands_override.<os>] cannot express this: an override
// SPECIALIZES a command that already has a same-owner base in
// [provide.commands] (command_compose.go rejects an override with no base),
// so it can only change a lane's shell text per OS, never say "this lane
// doesn't exist on Linux at all."
//
// The shape this file's LaneView / DeriveDemand consumes — proposed for a
// future manifest-spec revision, adopted in real kit.toml files by a
// downstream change, not this one:
//
//	# A lane is a NAMED command surface gated to a subset of this kit's own
//	# [supports].os — distinct from commands_override, which specializes a
//	# command that exists everywhere the kit itself does. Declare a lane
//	# when the command has no meaningful equivalent on an unsupported OS.
//	[[provide.lanes]]
//	name = "ios-app-build"
//	os   = ["macos"]           # required; subset of [supports].os
//	arch = ["arm64", "x86_64"] # optional; omit for "any" of the kit's arches
//
//	# Lane command bodies are deliberately out of scope here — DeriveDemand
//	# only needs the lane's platform demand, not its shell implementation.
//	# A future change composes [provide.lanes.commands] into the command
//	# plan the same way commands_override is composed today.
//
// DeriveDemand supports LaneView today so the moment a manifest starts
// emitting [[provide.lanes]], the engine has a working consumer — daemon/
// kit_registry.go parses the TOML block, daemon/kit_detect.go projects it
// into ManifestView.Lanes.
package kit

import "sort"

// Architecture identifiers used as values in [supports].arch (005 §
// "Platform compatibility"). Mirrors the OSLinux/OSMacOS/OSWindows trio in
// compose.go.
const (
	ArchX86_64 = "x86_64"
	ArchARM64  = "arm64"
)

// knownOS and knownArch are the full universe of platform values the kit
// manifest schema keys on today (005 § "Platform compatibility" — windows
// support is architecturally admitted but OSS-deferred; it is still part of
// the universe DeriveDemand intersects against, so a kit set that never
// mentions an OS still yields a well-defined, non-narrowing demand). Sorted
// so DeriveDemand's output is deterministic without a separate sort step
// when no kit constrains anything.
var (
	knownOS   = []string{OSLinux, OSMacOS, OSWindows}
	knownArch = []string{ArchARM64, ArchX86_64}
)

// LaneView is one [[provide.lanes]] declaration — a named command lane
// gated to a subset of the owning kit's own SupportedOS/SupportedArch. See
// the package doc for the proposed manifest shape this projects from.
type LaneView struct {
	// Name is the lane's identifier (e.g. "ios-app-build"). A generic-alias
	// collision with [provide.commands] is not validated here — lane
	// command composition is a separate, not-yet-implemented step.
	Name string
	// OS is the set of operating systems this lane runs on. Should be a
	// subset of the owning kit's SupportedOS (narrower, never wider);
	// DeriveDemand does not enforce that today. Empty means "every OS the
	// kit itself supports" (the same permissive-empty convention as
	// SupportedOS) — an empty-OS lane never narrows anything.
	OS []string
	// Arch is the analogous architecture subset.
	Arch []string
}

// LaneDemand is one composing kit's OS/arch-locked command lane, surfaced
// at the composition level so a caller that plans to engage this lane can
// apply its (typically narrower) platform demand — see
// PlacementDemand.EffectiveOS / EffectiveArch.
type LaneDemand struct {
	// Kit is the id of the declaring kit.
	Kit string `json:"kit"`
	// Lane is the lane's name.
	Lane string `json:"lane"`
	// OS is the lane's own OS set, already resolved against the
	// permissive-empty convention: never empty unless the declaring kit's
	// composition-wide OS demand is itself empty.
	OS []string `json:"os"`
	// Arch is the analogous architecture set.
	Arch []string `json:"arch,omitempty"`
}

// PlacementDemand is the pre-placement toolchain demand for a kit
// composition — the Layer-4 signal named in
// 001-layered-execution-model.md § "Layer 4 — Composition" and given its
// evaluation seam by
// ADR-2026-08-12-placement-composition-law-and-single-fallback-rule.md
// D1.2: "kit / toolchain demands (os, arch, OS-locked command lanes)" is
// one slot of the stage-2 viability tuple, alongside model, harness, auth
// binding, execution-host capabilities, lane, serving endpoint, and health.
//
// Unlike ToolchainDemand (compose.go), which requires a target OS to
// already be chosen and describes WHAT TO RUN, PlacementDemand describes
// WHERE THIS CAN RUN AT ALL and needs nothing but the kit set. A scheduler
// uses it as a hard candidate filter BEFORE placement.
type PlacementDemand struct {
	// OS is every operating system on which the WHOLE composition can run:
	// the intersection of each composing kit's SupportedOS. A kit that
	// declares no [supports].os does not narrow this (005's
	// permissive-empty rule) — DeriveDemand always returns a non-nil,
	// non-narrowed (full-universe) OS set for a kit set with no OS
	// constraints, so an unconstrained composition filters nothing
	// (additive: no behavior change for kits without constraints).
	//
	// A non-nil EMPTY slice means the composition is unsatisfiable — no OS
	// is common to every composing kit's declared support. Per ADR
	// D1.2 ("∅ is always loud"), callers MUST treat that as a typed,
	// reported exclusion of every candidate, never a silent no-op.
	OS []string `json:"os"`
	// Arch is the analogous intersection over SupportedArch.
	Arch []string `json:"arch"`
	// Lanes is every OS/arch-locked command lane declared by a composing
	// kit. Nil when no composing kit declares any [[provide.lanes]] entry
	// (every kit shipped today) — EffectiveOS/EffectiveArch degrade to OS/
	// Arch unchanged in that case.
	Lanes []LaneDemand `json:"lanes,omitempty"`
}

// IsUnsatisfiable reports the ADR-2026-08-12 D1.2 "∅ is always loud" case:
// no OS is common to every composing kit's declared support. Never conflate
// with "unconstrained" — DeriveDemand always returns a non-empty OS set for
// a kit set that declares no OS constraints at all.
func (d PlacementDemand) IsUnsatisfiable() bool {
	return d.OS != nil && len(d.OS) == 0
}

// NarrowsOS reports whether OS excludes at least one value from the known
// platform universe — i.e. whether reporting it as a capability filter
// would actually remove a candidate. A demand that was never derived
// (zero value) or that covers the full universe reports false.
func (d PlacementDemand) NarrowsOS() bool {
	return d.OS != nil && !sameSet(d.OS, knownOS)
}

// NarrowsArch is the Arch analogue of NarrowsOS.
func (d PlacementDemand) NarrowsArch() bool {
	return d.Arch != nil && !sameSet(d.Arch, knownArch)
}

// EffectiveOS returns the OS set that must hold for a session engaging the
// named lanes IN ADDITION TO the composition's own kit-level OS demand —
// the top-level OS intersected with every named-and-declared lane's OS.
// Unknown lane names (not declared by any composing kit) are ignored, so a
// caller can pass a work mode's full command set without first checking
// which of them happen to be lane-gated.
//
// Returns nil when the demand was never derived (OS is nil) — the caller's
// signal to apply no OS filter at all, distinct from an empty, non-nil
// slice (composition — or the engaged lane subset of it — is unsatisfiable
// on any OS).
func (d PlacementDemand) EffectiveOS(engagedLanes ...string) []string {
	if d.OS == nil {
		return nil
	}
	// make+copy, NOT append([]string(nil), d.OS...): appending a
	// zero-length slice to a nil slice returns nil (no growth needed), which
	// would silently destroy the non-nil-empty "unsatisfiable" signal when
	// d.OS is []string{}.
	out := make([]string, len(d.OS))
	copy(out, d.OS)
	if len(engagedLanes) == 0 {
		return out
	}
	engaged := make(map[string]struct{}, len(engagedLanes))
	for _, l := range engagedLanes {
		engaged[l] = struct{}{}
	}
	for _, lane := range d.Lanes {
		if _, ok := engaged[lane.Lane]; !ok {
			continue
		}
		if len(lane.OS) == 0 {
			continue // permissive-empty: lane runs on any OS the kit already supports
		}
		out = intersectStrings(out, lane.OS)
	}
	return out
}

// EffectiveArch is the Arch analogue of EffectiveOS.
func (d PlacementDemand) EffectiveArch(engagedLanes ...string) []string {
	if d.Arch == nil {
		return nil
	}
	// See EffectiveOS: make+copy preserves the non-nil-empty signal.
	out := make([]string, len(d.Arch))
	copy(out, d.Arch)
	if len(engagedLanes) == 0 {
		return out
	}
	engaged := make(map[string]struct{}, len(engagedLanes))
	for _, l := range engagedLanes {
		engaged[l] = struct{}{}
	}
	for _, lane := range d.Lanes {
		if _, ok := engaged[lane.Lane]; !ok {
			continue
		}
		if len(lane.Arch) == 0 {
			continue
		}
		out = intersectStrings(out, lane.Arch)
	}
	return out
}

// DeriveDemand computes the pre-placement PlacementDemand for a kit set.
// Order-independent — unlike Compose/ComposeForTarget, DeriveDemand doesn't
// read [provide.*] contribution ordering, only each view's own [supports]
// and [[provide.lanes]] declarations, so callers may pass views in any
// order (SortManifests is not required first).
//
// Additive: a kit set where no view declares SupportedOS, SupportedArch, or
// Lanes returns the full known-universe OS/Arch sets and no lanes —
// identical (non-)filtering behaviour to every session before this signal
// existed (005 "no behaviour change for kits without constraints").
func DeriveDemand(views []ManifestView) PlacementDemand {
	os := append([]string(nil), knownOS...)
	arch := append([]string(nil), knownArch...)
	var lanes []LaneDemand

	for _, v := range views {
		if len(v.SupportedOS) > 0 {
			os = intersectStrings(os, v.SupportedOS)
		}
		if len(v.SupportedArch) > 0 {
			arch = intersectStrings(arch, v.SupportedArch)
		}
		for _, lane := range v.Lanes {
			lanes = append(lanes, LaneDemand{
				Kit:  v.ID,
				Lane: lane.Name,
				OS:   sortedCopy(lane.OS),
				Arch: sortedCopy(lane.Arch),
			})
		}
	}

	sort.Slice(lanes, func(i, j int) bool {
		if lanes[i].Kit != lanes[j].Kit {
			return lanes[i].Kit < lanes[j].Kit
		}
		return lanes[i].Lane < lanes[j].Lane
	})

	return PlacementDemand{OS: os, Arch: arch, Lanes: lanes}
}

// intersectStrings returns the sorted intersection of a and b, always as a
// non-nil slice (even when the intersection is empty) so callers can rely
// on nil-vs-empty to distinguish "never computed" from "computed empty".
func intersectStrings(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := set[v]; ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// sortedCopy returns a sorted copy of in, or nil when in is empty — used
// for lane OS/Arch so LaneDemand output is deterministic without adopting
// intersectStrings's "always non-nil" contract (a lane's own OS/Arch has no
// "never computed" state to distinguish).
func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// sameSet reports whether a and b contain the same elements, ignoring
// order and duplicates. Both knownOS and knownArch are already
// duplicate-free and sorted, so callers pass a pre-sorted b.
func sameSet(a, sortedB []string) bool {
	if len(a) != len(sortedB) {
		return false
	}
	sortedA := append([]string(nil), a...)
	sort.Strings(sortedA)
	for i, v := range sortedA {
		if v != sortedB[i] {
			return false
		}
	}
	return true
}
