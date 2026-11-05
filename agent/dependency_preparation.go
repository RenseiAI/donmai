package agent

import "strings"

// This file owns the bounded dependency-preparation report contract and
// its recovery handoff. The installer (runner) produces the report, the
// terminal poster serializes it, and ApplyPreparedHarness projects the
// recovery text — all three read these helpers so the vocabulary, the
// ordering, and the prose have a single source instead of three copies
// that can drift.
//
// Authority doctrine: NeedsRecovery and AppendDependencyPreparationRecovery
// read ONLY the bounded report (per-ecosystem outcomes from the closed
// vocabulary in types.go). Raw installer output and error text never reach
// this file — the installer logs those and returns outcomes — so neither
// the prompt handoff nor the serialized report can leak them.

// DependencyEcosystemNames lists the installable ecosystems in install
// order (pnpm before go). Reports and recovery text follow this order so
// both are deterministic.
func DependencyEcosystemNames() []string { return []string{"pnpm", "go"} }

// EcosystemOutcome reports the recorded outcome for one ecosystem, or ""
// when the ecosystem was not detected (no key present).
func (p *DependencyPreparation) EcosystemOutcome(name string) string {
	if p == nil {
		return ""
	}
	switch name {
	case "pnpm":
		return p.Pnpm
	case "go":
		return p.Go
	default:
		return ""
	}
}

// SetEcosystemOutcome records one ecosystem's outcome. Unknown names are
// ignored: the wire object is closed to the ecosystems above.
func (p *DependencyPreparation) SetEcosystemOutcome(name, outcome string) {
	if p == nil {
		return
	}
	switch name {
	case "pnpm":
		p.Pnpm = outcome
	case "go":
		p.Go = outcome
	}
}

// DetectedEcosystems names the ecosystems this report carries outcomes
// for, in install order.
func (p *DependencyPreparation) DetectedEcosystems() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, name := range DependencyEcosystemNames() {
		if p.EcosystemOutcome(name) != "" {
			out = append(out, name)
		}
	}
	return out
}

// NeedsRecovery reports whether a preparation outcome must carry the
// runner-authored recovery handoff: only failed and timed_out outcomes do.
// Ready and both skipped outcomes add no instruction; nil (no detected
// ecosystem) adds none either.
func (p *DependencyPreparation) NeedsRecovery() bool {
	if p == nil {
		return false
	}
	for _, name := range DependencyEcosystemNames() {
		switch p.EcosystemOutcome(name) {
		case DependencyPreparationFailed, DependencyPreparationTimedOut:
			return true
		}
	}
	return false
}

// OutcomeSummary names each detected ecosystem's outcome in install order
// ("pnpm: failed, go: ready"). Bounded by construction: ecosystem names
// plus closed-vocabulary outcomes only.
func (p *DependencyPreparation) OutcomeSummary() string {
	if p == nil {
		return ""
	}
	var parts []string
	for _, name := range DependencyEcosystemNames() {
		if outcome := p.EcosystemOutcome(name); outcome != "" {
			parts = append(parts, name+": "+outcome)
		}
	}
	return strings.Join(parts, ", ")
}

// withDependencyPreparationRecovery projects the bounded recovery handoff
// onto the adapted Spec AFTER the prepared-harness equality check passes.
// It copies the protocol content before appending so the shared prompt
// plan the runner passed in is never mutated through the shallow copy
// AdaptPrompt returns. Not a bypass: the digest and receipt gates above
// run first and unchanged; this only renders digest-exempt runtime state
// the admission compiler could not have known.
func withDependencyPreparationRecovery(adapted Spec, manifest HarnessManifest, prep *DependencyPreparation) Spec {
	if prep == nil || !prep.NeedsRecovery() {
		return adapted
	}
	if manifest.Name == HarnessShell {
		return adapted
	}
	if adapted.PromptPlan != nil && adapted.PromptPlan.HarnessProtocol != nil {
		updated := *adapted.PromptPlan.HarnessProtocol
		updated.Text = AppendDependencyPreparationRecovery(updated.Text, prep)
		plan := *adapted.PromptPlan
		plan.HarnessProtocol = &updated
		adapted.PromptPlan = &plan
	}
	adapted.SystemPromptAppend = AppendDependencyPreparationRecovery(adapted.SystemPromptAppend, prep)
	// A harness with no system surface received the operating protocol
	// as a downgraded user-prompt prefix at adaptation time — and sends
	// only Spec.Prompt — so the handoff must ride the prompt too, or the
	// prepared lane delivers no recovery to it at all. Gated on the same
	// caller-authorized protocol-to-user downgrade AdaptPrompt required,
	// so harnesses with a native system surface never duplicate it.
	if profile, ok := manifest.PromptProfile(PromptModeForSpec(adapted)); ok &&
		profile.SystemDelivery == PromptDeliveryUnsupported &&
		adapted.PromptPlan != nil {
		if _, found := findDowngrade(adapted.PromptPlan.AuthorizedDowngrades, PromptChannelHarnessProtocol); found {
			adapted.Prompt = AppendDependencyPreparationRecovery(adapted.Prompt, prep)
		}
	}
	return adapted
}

// dependencyPreparationPartial is the bounded recovery handoff. It reports
// attempted preparation, never a guarantee of repair: the model-driving
// harness is asked to inspect and repair with its own permitted tools and
// to report blocked when repair cannot finish.
func dependencyPreparationPartial(prep *DependencyPreparation) string {
	return "# Dependency preparation\nDependency preparation did not complete (" + prep.OutcomeSummary() + "). Dependencies are not ready. Inspect and repair the checkout with your permitted tools before relying on dependency-based checks; do not assume dependencies are installed. If repair cannot finish, report blocked with the reason."
}

// AppendDependencyPreparationRecovery appends the recovery handoff to the
// harness protocol when prep needs it, preserving the existing protocol
// text. Ready, skipped, and nil outcomes return the protocol unchanged so
// they add no instruction and no bypass. Shell delivery never reaches this
// helper: the bare-shell user surface executes commands, so no non-user
// authority is projected there — the shell lane keeps the terminal report
// as evidence only.
func AppendDependencyPreparationRecovery(existing string, prep *DependencyPreparation) string {
	if prep == nil || !prep.NeedsRecovery() {
		return existing
	}
	partial := dependencyPreparationPartial(prep)
	if existing == "" {
		return partial
	}
	return existing + "\n\n" + partial
}
