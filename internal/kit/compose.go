// Package kit — compose.go: Kit toolchain-demand composition (K1.1 / K1.2).
//
// This is the Go port of the canonical TS reference
// donmai-libraries/packages/core/src/kits/compose.ts
// (`resolveToolchainInstall` + the hooks portion of `composeKits`),
// adapted to the Seam 2 contract in
// donmai-architecture/006-cross-provider-interactions.md
// § "Seam 2 — Kit toolchain demand → Workarea/Sandbox supply".
//
// Two deliberate divergences from the TS `resolveToolchainInstall`:
//
//  1. Composition operates on an ordered slice (foundation → framework →
//     project, per 005:335) and FLATTENS the per-OS install map into an
//     ordered []string so the provisioner can run the commands in a
//     deterministic, foundation-first sequence.
//  2. toolchain_install merging is CONJUNCTIVE UNION across kits (006:58
//     "Demand resolution is conjunctive across kits"), not last-write-wins.
//     The TS helper returns a key→cmd map where later kits overwrite the
//     same key; here we union over (kitOrder, key) so every distinct
//     install command from every selected kit runs. Within a single kit
//     the per-OS map keys are sorted for determinism (Go map iteration is
//     random).
//
// Base toolchains only: a kit's toolchain_install installs base toolchains
// (the workarea/sandbox provider's job per 006:61); framework deps belong
// in the post_acquire hook. The composer keeps the two streams separate so
// the provisioner runs install-then-hook in the right order (005:357).
package kit

import (
	"fmt"
	"runtime"
	"sort"
)

// Operating-system identifiers used as keys in
// [provide.toolchain_install.<os>] / [provide.hooks.os.<os>] (005:296).
const (
	OSLinux   = "linux"
	OSMacOS   = "macos"
	OSWindows = "windows"
)

// ManifestView is the exported, daemon-package-agnostic slice of a parsed
// kit manifest that Compose needs. The daemon's KitRegistry produces these
// via DetectForRepo so the runner can compose without importing the
// daemon-private kitManifestTOML type (avoids a daemon↔runner import
// cycle; mirrors how KitSkillSource keeps the skill loader decoupled).
type ManifestView struct {
	// ID is the kit's canonical id (e.g. "typescript").
	ID string
	// Version is the kit's [kit].version, used to stamp the demand.
	Version string
	// Priority is [kit].priority — the deterministic tie-break within an
	// order group (005 § "Confidence and selection").
	Priority int
	// Order is [composition].order: "foundation" | "framework" |
	// "project". Empty is treated as "project" (most-specific, runs last).
	Order string
	// SupportedOS is [supports].os; empty means "any".
	SupportedOS []string
	// PackageDigest is the immutable package digest used in command identity.
	PackageDigest string
	// LegacyManifestDigest is used only when no package digest exists; keeping
	// the fields separate prevents a legacy manifest from being serialized as a
	// verified package identity.
	LegacyManifestDigest string
	// PathScope is the repository-relative contribution scope. Empty means the
	// repository root. WorkTypes optionally narrows command applicability.
	PathScope string
	WorkTypes []string
	// Commands and CommandsOverride are the v1 owner-local command map and its
	// same-owner OS specializations.
	Commands         map[string]string
	CommandsOverride map[string]map[string]string
	// ToolchainInstall is [provide.toolchain_install.<os>] → {key: cmd}.
	ToolchainInstall map[string]map[string]string
	// Hooks is [provide.hooks] (generic + OS-keyed overlay).
	Hooks HooksView
	// PromptFragments is the slice of [provide.prompt_fragments] entries.
	// Each entry names a partial to inject, a workType filter ([when]),
	// and the file that contains the fragment body (relative to manifest dir).
	// The runtime loader (kit.LoadPromptFragments) applies workType filtering
	// and resolves file paths.
	PromptFragments []PromptFragmentEntry
}

// PromptFragmentEntry mirrors one [[provide.prompt_fragments]] TOML
// declaration projected into the daemon-independent view type.
type PromptFragmentEntry struct {
	// Partial is the logical name of the fragment (e.g. "spring-conventions").
	Partial string
	// When is the list of workType values for which this fragment is injected.
	// An empty slice means "always inject" (no workType filter).
	When []string
	// File is the fragment body path relative to the kit manifest directory.
	File string
}

// HooksView mirrors [provide.hooks] for composition.
type HooksView struct {
	PostAcquire string
	PreRelease  string
	OS          map[string]HookOSView
}

// HookOSView is one OS-keyed hook overlay.
type HookOSView struct {
	PostAcquire string
	PreRelease  string
}

// ToolchainDemand is the composed, ready-to-execute demand the
// KitProvisioner runs against the workarea (Seam 2 boundary; identical in
// spirit to the TS ToolchainDemand). It is JSON-serialisable so it can ride
// the spawner→child QueuedWork payload in K2 / the platform SandboxSpec.
type ToolchainDemand struct {
	// Kits is the contributing kits as "id@version", in composition order.
	Kits []string `json:"kits"`
	// OS is the resolved target OS ("linux" | "macos" | "windows").
	OS string `json:"os"`
	// ToolchainInstall is the ordered, unioned base-toolchain install
	// commands (foundation → framework → project; keys sorted within a
	// kit). Run first.
	ToolchainInstall []string `json:"toolchain_install"`
	// PostAcquire is the ordered post_acquire hook scripts
	// (foundation-first). Run after ToolchainInstall.
	PostAcquire []string `json:"post_acquire"`
	// PreRelease is the ordered pre_release hook scripts (foundation-first)
	// run best-effort on teardown.
	PreRelease []string `json:"pre_release"`
	// Env is optional environment threaded into every command (e.g. a kit
	// that installs to ~/.cargo can export PATH). Reserved for K3 kits;
	// currently always nil from Compose.
	Env map[string]string `json:"env,omitempty"`
	// Commands retain every owner-qualified command. CommandBindings resolve
	// generic aliases without erasing ownership; CompositionDigest binds the
	// exact target, commands, and bindings for diagnostics/audit evidence.
	Commands          []QualifiedCommand      `json:"commands,omitempty"`
	CommandBindings   []GenericCommandBinding `json:"command_bindings,omitempty"`
	CompositionDigest string                  `json:"composition_digest,omitempty"`
}

// IsEmpty reports whether the demand has nothing to execute. The runner
// uses this to skip the provision step entirely (additive: zero-kit
// sessions behave exactly as before K1).
func (d *ToolchainDemand) IsEmpty() bool {
	if d == nil {
		return true
	}
	return len(d.ToolchainInstall) == 0 && len(d.PostAcquire) == 0 && len(d.PreRelease) == 0 && len(d.Commands) == 0
}

// orderRank maps a [composition].order value to a sort rank so manifests
// compose foundation → framework → project (005:335). Unknown / empty
// orders rank as "project" (last) so a kit that forgets to declare order
// never jumps ahead of an explicit foundation kit.
func orderRank(order string) int {
	switch order {
	case "foundation":
		return 0
	case "framework":
		return 1
	case "project":
		return 2
	default:
		return 2
	}
}

// SortManifests returns a copy of views ordered foundation → framework →
// project, then by descending priority, then by id — the deterministic
// selection order from 005 § "Confidence and selection" (priority,
// then kit.id tie-break) applied within each order group. Callers that
// already have an ordered slice may skip this; Compose does NOT re-sort
// (it trusts the caller per the TS contract "kits MUST be ordered"), so
// DetectForRepo runs this before handing manifests to Compose.
func SortManifests(views []ManifestView) []ManifestView {
	out := make([]ManifestView, len(views))
	copy(out, views)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := orderRank(out[i].Order), orderRank(out[j].Order)
		if ri != rj {
			return ri < rj
		}
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority // higher priority first
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Compose merges the selected manifests into a single ToolchainDemand for
// targetOS. The manifests MUST already be ordered foundation → framework →
// project (use SortManifests / DetectForRepo). Mirrors compose.ts but with
// conjunctive-union semantics for toolchain_install (006:58) and
// foundation-first hook concatenation (005:357).
//
// Determinism: within one kit the per-OS install map keys are sorted, so a
// kit declaring {node: …, pnpm: …} always emits node before pnpm.
// Duplicate exact (key, command) pairs across kits are de-duplicated to
// avoid re-running an identical install; a different command under the same
// key is kept (both run) — that is the union, not a conflict. True version
// conflicts (two exact pins of the same toolchain) are a kit-authoring
// concern resolved upstream at version-pin resolution (006:59), not here.
func Compose(views []ManifestView, targetOS string) (*ToolchainDemand, error) {
	return ComposeForTarget(views, CompositionTarget{OS: targetOS, PathScope: "."}, nil, nil)
}

// ComposeForTarget composes executable lifecycle demand and owner-qualified
// commands for the exact session target. Generic command ownership fails closed
// unless one owner exists, an authenticated delegation chain has one terminal,
// or an exact operator lock binding selects the owner.
func ComposeForTarget(views []ManifestView, target CompositionTarget, lock *CompositionLock, delegations []CommandDelegation) (*ToolchainDemand, error) {
	targetOS := target.OS
	if targetOS == "" {
		return nil, fmt.Errorf("kit compose: targetOS is required")
	}
	d := &ToolchainDemand{OS: targetOS}
	seenInstall := map[string]struct{}{} // dedup identical commands

	for _, v := range views {
		// Respect [supports].os — a kit that does not support targetOS
		// contributes nothing (005:284-298 short-circuit).
		if !supportsOS(v.SupportedOS, targetOS) {
			continue
		}

		d.Kits = append(d.Kits, kitRef(v))

		// toolchain_install[targetOS] → ordered, unioned commands.
		if osMap := v.ToolchainInstall[targetOS]; len(osMap) > 0 {
			keys := make([]string, 0, len(osMap))
			for k := range osMap {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				cmd := osMap[k]
				if cmd == "" {
					continue
				}
				if _, dup := seenInstall[cmd]; dup {
					continue
				}
				seenInstall[cmd] = struct{}{}
				d.ToolchainInstall = append(d.ToolchainInstall, cmd)
			}
		}

		// hooks — OS-keyed overlay wins over generic (005:296).
		if pa := resolveHook(v.Hooks, targetOS, true); pa != "" {
			d.PostAcquire = append(d.PostAcquire, pa)
		}
		if pr := resolveHook(v.Hooks, targetOS, false); pr != "" {
			d.PreRelease = append(d.PreRelease, pr)
		}
	}
	commands, err := ResolveCommandComposition(views, target, lock, delegations)
	if err != nil {
		return nil, err
	}
	d.Commands = commands.Commands
	d.CommandBindings = commands.Bindings
	d.CompositionDigest = commands.Digest

	return d, nil
}

// resolveHook returns the effective hook script for targetOS. When
// postAcquire is true it resolves post_acquire, otherwise pre_release.
// OS-keyed override wins over the generic form (most-specific-match-wins,
// mirrors compose.ts resolveHook).
func resolveHook(h HooksView, targetOS string, postAcquire bool) string {
	if e, ok := h.OS[targetOS]; ok {
		if postAcquire && e.PostAcquire != "" {
			return e.PostAcquire
		}
		if !postAcquire && e.PreRelease != "" {
			return e.PreRelease
		}
	}
	if postAcquire {
		return h.PostAcquire
	}
	return h.PreRelease
}

// supportsOS reports whether a kit declaring supported = SupportedOS
// applies on targetOS. An empty/nil supported set means "any OS"
// (permissive: a kit that omits [supports].os is not gated out).
func supportsOS(supported []string, targetOS string) bool {
	if len(supported) == 0 {
		return true
	}
	for _, s := range supported {
		if s == targetOS {
			return true
		}
	}
	return false
}

// kitRef formats a ManifestView as "id@version" (or just "id" when no
// version is declared).
func kitRef(v ManifestView) string {
	if v.Version == "" {
		return v.ID
	}
	return v.ID + "@" + v.Version
}

// ResolveOS maps a Go GOOS value to a kit-manifest OS key. Returns an
// error for OSes the manifest schema does not key on (the kit system keys
// install scripts by linux | macos | windows only, 005:296). Cloud
// sandboxes are Linux even on a macOS host, so callers targeting a cloud
// sandbox pass OSLinux directly rather than going through ResolveOS.
func ResolveOS(goos string) (string, error) {
	switch goos {
	case "linux":
		return OSLinux, nil
	case "darwin":
		return OSMacOS, nil
	case "windows":
		return OSWindows, nil
	default:
		return "", fmt.Errorf("kit compose: unsupported GOOS %q (no toolchain_install key)", goos)
	}
}

// MustResolveOS resolves the host GOOS to a manifest OS key, falling back
// to "linux" for any unexpected GOOS so a misconfigured host degrades to
// the cloud-default OS rather than aborting. Used by the local runner path.
func MustResolveOS() string {
	if os, err := ResolveOS(runtime.GOOS); err == nil {
		return os
	}
	return OSLinux
}
