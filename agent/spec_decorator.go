package agent

import "context"

// This file declares the embedder registration hook for the additional-
// extension delivery seam (extension_delivery.go / Spec.AdditionalExtensions,
// per ADR-2026-08-12-pi-extension-delivery-seam-and-capability-pack-boundary.md
// D1, mirrored into 002-provider-base-contract.md §E "Additional-extension
// delivery"). The seam itself lets ANY caller populate
// Spec.AdditionalExtensions before Spawn/Resume — but agent-run orchestration
// (the daemon-spawned `agent run` subcommand) constructs the Spec internally
// with unexported logic, so an embedding binary that only composes this
// package as a library via afcli.RegisterCommands has no sanctioned way to
// reach it: it cannot subclass the orchestration, and it cannot edit
// unexported code in a vendored dependency.
//
// ExtensionDecorator + DecorateProvider close that gap by moving the
// insertion point to the Provider boundary instead of the orchestration
// internals: an embedder wraps its registered providers once, and every
// Spawn/Resume call — present orchestration and any future caller — passes
// through the decorator first. This mirrors the shape every other
// capability-gated Spec field already uses (Spec.Interactive,
// Spec.ResponseSchema): a harness without a host-side extension API simply
// ignores an empty/absent AdditionalExtensions list, so wrapping a provider
// that never reads the field is a no-op, not an error.

// ExtensionDecorator computes the additional extension deliveries an
// embedding binary wants appended to a spec immediately before Spawn or
// Resume. It receives the spec exactly as orchestration constructed it and
// returns ONLY the deliveries to add — never the whole spec — so a decorator
// cannot accidentally drop, reorder, or replace anything orchestration or an
// earlier decorator already placed on spec.AdditionalExtensions per D1: "the
// policy extension is always first and cannot be displaced, reordered, or
// disabled by a delivery." Returning nil or an empty slice leaves the spec
// unchanged.
//
// A harness with no host-side extension API (every harness other than pi
// today) ignores AdditionalExtensions entirely — per D5.5 there is no
// cross-harness "supports extensions" capability flag, so a decorator's
// appended deliveries are inert there, exactly like any other
// capability-gated Spec field the exact selected harness doesn't honor.
type ExtensionDecorator func(spec Spec) []ExtensionDelivery

// ApplyExtensionDecorator returns spec with decorate's deliveries appended to
// AdditionalExtensions — the exact mutation DecorateProvider's wrapped
// Spawn/Resume applies (decorateSpec, below), exported so a preflight
// compiler can apply the identical decorator to the identical spec shape
// BEFORE persisting a ToolLifecycleReceipt from it.
//
// This exists because CompilePreparedHarness/ApplyPreparedHarness must stay
// side-effect-free (PreparedHarness is a secret-free, digest-only authority —
// see its doc comment), so a compile site can never reach a decorator by
// calling through Provider.Spawn/Resume the way DecorateProvider does — it
// has to apply the same pure function directly. See
// runner.ReconcileAdditionalExtensions for the shared constructor that
// threads this into both the daemon's preflight compiler and the runner's
// own prepared-source authority check, so an embedder's
// AgentSpecExtensionDecorator (donmai-architecture
// 002-provider-base-contract.md §E) can never be visible to only one of the
// two compile sites that must agree on Spec.AdditionalExtensions.
func ApplyExtensionDecorator(spec Spec, decorate ExtensionDecorator) Spec {
	return decorateSpec(spec, decorate)
}

// decorateSpec returns spec with decorate's deliveries appended to
// AdditionalExtensions, without mutating spec's own backing array. A nil
// decorate, or one that returns no deliveries, returns spec unchanged
// (byte-identical, not merely equal) so a no-op decorator costs nothing
// beyond the call itself.
func decorateSpec(spec Spec, decorate ExtensionDecorator) Spec {
	if decorate == nil {
		return spec
	}
	extra := decorate(spec)
	if len(extra) == 0 {
		return spec
	}
	merged := make([]ExtensionDelivery, 0, len(spec.AdditionalExtensions)+len(extra))
	merged = append(merged, spec.AdditionalExtensions...)
	merged = append(merged, extra...)
	spec.AdditionalExtensions = merged
	return spec
}

// DecorateProvider wraps p so decorate runs against the Spec argument of
// EVERY Spawn and Resume call before delegating to p — the sanctioned
// registration hook for an embedding binary that composes this package via
// afcli.RegisterCommands and wants to append its own additional-extension
// deliveries onto specs agent-run orchestration constructs internally.
//
// decorate is invoked once per Spawn/Resume call, on the spec exactly as the
// caller built it, before any provider-side materialization or digest
// verification — the harness's own trust-boundary extension (where one
// exists, e.g. pi's embedded policy extension) is loaded by the provider
// itself and is never part of Spec.AdditionalExtensions, so it keeps loading
// first regardless of what a decorator appends here (D1). Digest
// verification is likewise unchanged: this hook only appends
// ExtensionDelivery values to the spec; it never materializes, reads, or
// verifies bytes itself, so every delivery — decorator-supplied or
// caller-supplied — is still verified exactly once, after materialization,
// by the exact harness adapter (D2(b)).
//
// A nil decorate is a passthrough: DecorateProvider(p, nil) returns p
// unchanged (the identical value, not a wrapper around it), so a caller may
// wrap unconditionally — e.g. every provider a registry constructs — without
// a nil check on the hot path, and zero decorators registered means zero
// behavior change. p == nil returns nil.
//
// The returned Provider preserves agent.HarnessProvider when p implements
// it. This matters operationally, not just cosmetically: runner package
// admission (runner/harness_selection.go) type-asserts a registered
// Provider against HarnessProvider to read its manifest for
// capability-gated selection and explicit-harness routing. A wrapper that
// silently dropped that satisfaction would make a HarnessProvider invisible
// to manifest-based selection — a real behavior change no embedder asked
// for, not a cosmetic one.
func DecorateProvider(p Provider, decorate ExtensionDecorator) Provider {
	if p == nil || decorate == nil {
		return p
	}
	if hp, ok := p.(HarnessProvider); ok {
		return decoratedHarnessProvider{HarnessProvider: hp, decorate: decorate}
	}
	return decoratedProvider{Provider: p, decorate: decorate}
}

// decoratedProvider wraps a plain Provider. Every method other than Spawn
// and Resume forwards unchanged via the embedded interface.
type decoratedProvider struct {
	Provider
	decorate ExtensionDecorator
}

func (d decoratedProvider) Spawn(ctx context.Context, spec Spec) (Handle, error) {
	return d.Provider.Spawn(ctx, decorateSpec(spec, d.decorate))
}

func (d decoratedProvider) Resume(ctx context.Context, sessionID string, spec Spec) (Handle, error) {
	return d.Provider.Resume(ctx, sessionID, decorateSpec(spec, d.decorate))
}

// decoratedHarnessProvider wraps a HarnessProvider. Spawn/Resume are
// explicit overrides (defined below); every other method — including
// Manifest() — forwards unchanged via the embedded interface, so the
// wrapped value keeps satisfying HarnessProvider. Deliberately does NOT
// also embed decoratedProvider: embedding both would promote Spawn/Resume
// from two fields at equal depth (decoratedProvider's own explicit methods
// vs HarnessProvider's promoted ones), which is an ambiguous selector Go
// excludes from the method set entirely — silently breaking Provider
// satisfaction. Defining Spawn/Resume directly here, at depth zero, avoids
// that trap.
type decoratedHarnessProvider struct {
	HarnessProvider
	decorate ExtensionDecorator
}

func (d decoratedHarnessProvider) Spawn(ctx context.Context, spec Spec) (Handle, error) {
	return d.HarnessProvider.Spawn(ctx, decorateSpec(spec, d.decorate))
}

func (d decoratedHarnessProvider) Resume(ctx context.Context, sessionID string, spec Spec) (Handle, error) {
	return d.HarnessProvider.Resume(ctx, sessionID, decorateSpec(spec, d.decorate))
}
