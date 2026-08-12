// Package daemon kit_demand.go — pre-placement kit toolchain-demand
// derivation on the daemon's composition surface (K1.4).
//
// ComposeForRepo (kit_command_compose.go) and DetectForRepo (kit_detect.go)
// both require a target OS to already be chosen: [supports].os gates
// candidate kits BEFORE detection even runs. That is correct once placement
// has already happened — it stops an incompatible kit from materializing
// against the wrong workarea — but it means nothing upstream of placement
// can ask "which platforms can this repo's kit set even run on" (005 §
// "Platform compatibility"; the gap this file closes, named in
// 001-layered-execution-model.md § "Layer 4 — Composition" and given its
// evaluation seam by
// ADR-2026-08-12-placement-composition-law-and-single-fallback-rule.md
// D1.2).
package daemon

import "github.com/RenseiAI/donmai/internal/kit"

// DemandForRepo computes the pre-placement kit.PlacementDemand for
// repoRoot's detected kit set — the OS/arch set the whole composition can
// run on, plus any OS/arch-locked command lanes, derived WITHOUT a target
// OS. An embedding platform or the local scheduler calls this once the
// session's repo is known and BEFORE a candidate is chosen, applying the
// result as a stage-2 viability filter (see routing_state.go's
// FilterCandidatesByDemand for the local capability-filter phase).
//
// Unlike ComposeForRepo, DemandForRepo never short-circuits kit detection
// on OS — it runs DetectForRepoAnyOS so every candidate kit's own declared
// support is visible to kit.DeriveDemand's intersection, regardless of
// which OS (if any) happens to be the local host's.
//
// A repo with no matching kits returns the zero kit.DeriveDemand output —
// the full known-universe OS/Arch sets and no lanes, i.e. "unconstrained":
// additive, no behaviour change for a kit-less session.
func (r *KitRegistry) DemandForRepo(repoRoot string) (kit.PlacementDemand, error) {
	views, err := r.DetectForRepoAnyOS(repoRoot)
	if err != nil {
		return kit.PlacementDemand{}, err
	}
	return kit.DeriveDemand(views), nil
}
