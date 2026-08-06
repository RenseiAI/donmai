package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// HarnessAdmissionError is the typed pre-spawn denial returned when an
// explicit harness selector cannot be honored exactly. Code uses the canonical
// execution-cell denial vocabulary. Receipt is populated by preflight or
// runLoop after the canonical denied AdmissionReceipt is built and validated.
type HarnessAdmissionError struct {
	Code      executioncell.AdmissionDenialCode
	Harness   string
	Detail    string
	Decisions []executioncell.ResolverDecision
	Receipt   executioncell.ImmutableAdmissionReceipt
	cause     error
}

func (e *HarnessAdmissionError) Error() string {
	return fmt.Sprintf("harness admission denied (%s, harness=%q): %s", e.Code, e.Harness, e.Detail)
}

func (e *HarnessAdmissionError) Unwrap() error { return e.cause }

// harnessSelection is the single admitted runtime selection carried through a
// run. Provider is the concrete implementation; Harness is the independent
// loop-driver identity. Decisions use the canonical execution-cell resolver
// vocabulary so they can be attached to the full upstream receipt when the
// dispatch contract is threaded end-to-end.
type harnessSelection struct {
	Provider  agent.Provider
	Harness   executioncell.HarnessRef
	Decisions []executioncell.ResolverDecision
	Explicit  bool
}

type harnessSelectorIntent struct {
	Harness  string
	Provider agent.ProviderName
	Runner   string
}

func selectorIntent(profile ResolvedProfile) harnessSelectorIntent {
	return harnessSelectorIntent{
		Harness: profile.Harness, Provider: profile.Provider, Runner: profile.Runner,
	}
}

// HarnessAdmission is an opaque, side-effect-free explicit-harness preflight
// result. Its private fields bind it to the registry and selector intent that
// produced it, so RunAdmitted can consume the exact admitted provider once
// even if later pre-run setup adds unrelated profile fields such as Endpoint.
type HarnessAdmission struct {
	registry            *Registry
	requestID           string
	intent              harnessSelectorIntent
	selection           harnessSelection
	denial              error
	denialPayloadDigest string
	consumed            atomic.Bool
}

// CanonicalHarnessRef returns a value copy of the canonical harness identity
// from a successful explicit admission. Reading the projection does not
// consume the one-shot admission and never exposes its mutable Provider. Nil,
// legacy/absent, and denied admissions return false.
func (a *HarnessAdmission) CanonicalHarnessRef() (executioncell.HarnessRef, bool) {
	if a == nil || a.denial != nil || a.selection.Provider == nil || a.selection.Harness.ID == "" {
		return executioncell.HarnessRef{}, false
	}
	return a.selection.Harness, true
}

// PreflightHarness admits explicit harness intent using only the in-memory
// registry. It performs no posterior request, worktree operation, credential
// delivery, gateway binding, provider Spawn, or result/status post. A nil
// admission with nil error means the request omitted Harness and must follow
// the named legacy adapter inside Run. Explicit denials carry a canonical
// immutable denied receipt on HarnessAdmissionError.
func (r *Registry) PreflightHarness(qw QueuedWork) (*HarnessAdmission, error) {
	if qw.ResolvedProfile.Harness == "" {
		return nil, nil
	}
	selection, err := r.selectExplicitHarness(qw.ResolvedProfile)
	if err != nil {
		denial := attachDeniedHarnessReceipt(qw, err, time.Now())
		payloadDigest, _ := executioncell.DigestContractValue(qw)
		return &HarnessAdmission{
			registry: r, requestID: qw.SessionID,
			intent: selectorIntent(qw.ResolvedProfile), denial: denial,
			denialPayloadDigest: payloadDigest,
		}, denial
	}
	return &HarnessAdmission{
		registry: r, requestID: qw.SessionID,
		intent: selectorIntent(qw.ResolvedProfile), selection: selection,
	}, nil
}

const (
	legacyNativeHarness = agent.HarnessName("native")
	legacyRawHarness    = agent.HarnessName("raw")
)

// recognizedHarnessToken recognizes canonical execution-cell harness ids plus
// the documented legacy Platform wire aliases. Recognition is deliberately
// separate from raw/native provider-pair validation: both compatibility tokens
// are known even when their provider pair cannot be admitted.
func recognizedHarnessToken(token string) (agent.HarnessName, bool) {
	if token == "" || token != strings.TrimSpace(token) {
		return "", false
	}
	switch token {
	case string(agent.HarnessClaudeCode), "claude":
		return agent.HarnessClaudeCode, true
	case string(agent.HarnessCodex):
		return agent.HarnessCodex, true
	case string(agent.HarnessOpenCode):
		return agent.HarnessOpenCode, true
	case string(agent.HarnessAntigravity), "agy":
		return agent.HarnessAntigravity, true
	case string(agent.HarnessAmp):
		return agent.HarnessAmp, true
	case string(agent.HarnessGeminiDirect):
		return agent.HarnessGeminiDirect, true
	case string(agent.HarnessOllama):
		return agent.HarnessOllama, true
	case string(legacyRawHarness):
		return legacyRawHarness, true
	case "native":
		return legacyNativeHarness, true
	case string(agent.HarnessStub):
		return agent.HarnessStub, true
	case string(agent.HarnessPi):
		return agent.HarnessPi, true
	case string(agent.HarnessShell):
		return agent.HarnessShell, true
	default:
		return "", false
	}
}

// canonicalInBoxHarness validates the provider-paired raw/native compatibility
// aliases present on the live Platform wire. Claude and Codex have dedicated
// CLI harness tokens; accepting raw/native for them would conflate direct/API
// and CLI execution.
func canonicalInBoxHarness(provider agent.ProviderName) (agent.HarnessName, bool) {
	switch provider {
	case agent.ProviderGemini:
		return agent.HarnessGeminiDirect, true
	case agent.ProviderOllama:
		return agent.HarnessOllama, true
	default:
		return "", false
	}
}

func harnessRef(provider agent.HarnessProvider) executioncell.HarnessRef {
	manifest := provider.Manifest()
	return executioncell.HarnessRef{ID: string(manifest.Name), Version: manifest.ContractABI}
}

// legacyHarnessNameForProvider is compatibility projection confined to the
// named legacy adapter. It lets older Provider implementations that predate
// Manifest() retain absent-selector behavior; explicit harness admission never
// uses this map and always requires a live registered manifest match.
func legacyHarnessNameForProvider(name agent.ProviderName) agent.HarnessName {
	switch name {
	case agent.ProviderClaude:
		return agent.HarnessClaudeCode
	case agent.ProviderCodex:
		return agent.HarnessCodex
	case agent.ProviderAmp:
		return agent.HarnessAmp
	case agent.ProviderAGYCLI:
		return agent.HarnessAntigravity
	case agent.ProviderOpenCode:
		return agent.HarnessOpenCode
	case agent.ProviderGemini:
		return agent.HarnessGeminiDirect
	case agent.ProviderOllama:
		return agent.HarnessOllama
	case agent.ProviderStub:
		return agent.HarnessStub
	case agent.ProviderPi:
		return agent.HarnessPi
	case agent.ProviderShell:
		return agent.HarnessShell
	default:
		return agent.HarnessName(name)
	}
}

func legacyHarnessRef(provider agent.Provider) executioncell.HarnessRef {
	if manifestProvider, ok := provider.(agent.HarnessProvider); ok {
		return harnessRef(manifestProvider)
	}
	return executioncell.HarnessRef{
		ID: string(legacyHarnessNameForProvider(provider.Name())), Version: "harness/v2",
	}
}

func explicitHarnessSourceRef(token string) string {
	switch token {
	case "claude", "agy", "native", string(legacyRawHarness):
		return "legacy-harness:" + token
	default:
		// codex, amp, and opencode are both live Platform wire values and
		// canonical manifest ids; no alias translation is necessary.
		return "canonical-harness:" + token
	}
}

func explicitHarnessDecision(token string, ref executioncell.HarnessRef) executioncell.ResolverDecision {
	return executioncell.ResolverDecision{
		Kind:        executioncell.DecisionExplicit,
		Field:       "harness",
		SelectedRef: "harness:" + ref.ID + "@" + ref.Version,
		SourceRef:   explicitHarnessSourceRef(token),
		Reason:      "ResolvedProfile carried an explicit harness selector; the registered manifest supplied its canonical identity and version.",
	}
}

// selectExplicitHarness resolves only the requested harness. It never consults
// Provider, Runner, a default, or posterior routing as a fallback. Provider is
// consulted only by the receipted raw/native compatibility adapter; a canonical
// harness id is authoritative even when Provider contradicts it.
func (r *Registry) selectExplicitHarness(profile ResolvedProfile) (harnessSelection, error) {
	canonical, known := recognizedHarnessToken(profile.Harness)
	if !known {
		return harnessSelection{}, &HarnessAdmissionError{
			Code:    executioncell.DenialUnknownHarness,
			Harness: profile.Harness,
			Detail:  "selector is not a known canonical harness or documented legacy harness alias",
		}
	}
	if canonical == legacyNativeHarness || canonical == legacyRawHarness {
		var compatible bool
		canonical, compatible = canonicalInBoxHarness(profile.Provider)
		if !compatible {
			decision := executioncell.ResolverDecision{
				Kind:        executioncell.DecisionExplicit,
				Field:       "harness",
				SelectedRef: "harness:" + profile.Harness,
				SourceRef:   explicitHarnessSourceRef(profile.Harness),
				Reason:      "Legacy ResolvedProfile explicitly selected a known raw/native compatibility alias, but its provider was not one of the documented gemini or ollama pairings.",
			}
			return harnessSelection{}, &HarnessAdmissionError{
				Code:      executioncell.DenialHarnessUnavailable,
				Harness:   profile.Harness,
				Detail:    "known raw/native compatibility alias requires provider gemini or ollama",
				Decisions: []executioncell.ResolverDecision{decision},
			}
		}
	}

	var matches []agent.HarnessProvider
	for _, name := range r.Names() {
		provider, err := r.Resolve(name)
		if err != nil {
			continue
		}
		harness, ok := provider.(agent.HarnessProvider)
		if ok && harness.Manifest().Name == canonical {
			matches = append(matches, harness)
		}
	}

	if len(matches) != 1 {
		detail := "known harness has no registered runtime implementation"
		if len(matches) > 1 {
			detail = "known canonical harness maps to multiple registered runtime implementations"
		}
		decision := executioncell.ResolverDecision{
			Kind:        executioncell.DecisionExplicit,
			Field:       "harness",
			SelectedRef: "harness:" + string(canonical),
			SourceRef:   explicitHarnessSourceRef(profile.Harness),
			Reason:      "ResolvedProfile explicitly selected a known harness, but the live registry could not admit exactly one matching implementation.",
		}
		return harnessSelection{}, &HarnessAdmissionError{
			Code:      executioncell.DenialHarnessUnavailable,
			Harness:   string(canonical),
			Detail:    detail,
			Decisions: []executioncell.ResolverDecision{decision},
		}
	}

	selected := matches[0]
	ref := harnessRef(selected)
	decision := explicitHarnessDecision(profile.Harness, ref)
	return harnessSelection{
		Provider: selected, Harness: ref,
		Decisions: []executioncell.ResolverDecision{decision}, Explicit: true,
	}, nil
}

type legacyHarnessSource struct {
	provider agent.ProviderName
	kind     executioncell.ResolverDecisionKind
	source   string
	reason   string
}

// legacyHarnessSelectionSource is the only place where absent harness intent
// may use the historical Provider -> Runner -> Claude chain.
func legacyHarnessSelectionSource(profile ResolvedProfile) legacyHarnessSource {
	if profile.Provider != "" {
		return legacyHarnessSource{
			provider: profile.Provider,
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-provider:" + string(profile.Provider),
			reason:   "Legacy ResolvedProfile omitted harness; the documented provider mapping supplied it.",
		}
	}
	if profile.Runner != "" {
		return legacyHarnessSource{
			provider: agent.ProviderName(profile.Runner),
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-runner:" + profile.Runner,
			reason:   "Legacy ResolvedProfile omitted harness and provider; the documented runner mapping supplied it.",
		}
	}
	return legacyHarnessSource{
		provider: agent.ProviderClaude,
		kind:     executioncell.DecisionDefault,
		reason:   "Legacy ResolvedProfile omitted harness, provider, and runner; existing dispatch semantics default to Claude Code.",
	}
}

// legacyHarnessSelectionAdapter preserves absent-selector behavior while
// making every inference/default visible. posteriorProvider is optional; when
// set, it is the final legacy routing choice and resolution still happens once.
func (r *Registry) legacyHarnessSelectionAdapter(profile ResolvedProfile, posteriorProvider agent.ProviderName) (harnessSelection, error) {
	source := legacyHarnessSelectionSource(profile)
	if posteriorProvider != "" {
		source = legacyHarnessSource{
			provider: posteriorProvider,
			kind:     executioncell.DecisionLegacyInference,
			source:   "legacy-posterior:" + string(posteriorProvider),
			reason:   "Legacy request omitted an explicit harness; posterior routing selected a registered provider before harness admission.",
		}
	}
	provider, err := r.Resolve(source.provider)
	if err != nil {
		providerErr := &ProviderNotRegisteredError{
			RequestedID: string(source.provider),
			Registered:  r.registeredNames(),
		}
		return harnessSelection{}, &HarnessAdmissionError{
			Code:      executioncell.DenialHarnessUnavailable,
			Harness:   string(source.provider),
			Detail:    providerErr.Error(),
			Decisions: []executioncell.ResolverDecision{legacyUnresolvedDecision(source)},
			cause:     providerErr,
		}
	}
	ref := legacyHarnessRef(provider)
	decision := executioncell.ResolverDecision{
		Kind: source.kind, Field: "harness",
		SelectedRef: "harness:" + ref.ID + "@" + ref.Version,
		SourceRef:   source.source, Reason: source.reason,
	}
	return harnessSelection{
		Provider: provider, Harness: ref,
		Decisions: []executioncell.ResolverDecision{decision},
	}, nil
}

func legacyUnresolvedDecision(source legacyHarnessSource) executioncell.ResolverDecision {
	return executioncell.ResolverDecision{
		Kind: source.kind, Field: "harness",
		SelectedRef: "harness:" + string(source.provider),
		SourceRef:   source.source, Reason: source.reason,
	}
}

// resolveHarnessSelection performs exactly one registry resolution. Explicit
// intent is admitted first and never reaches posterior routing. Legacy intent
// may retain posterior behavior, but the final posterior/static provider is
// handed once to the explicit legacy adapter.
func (r *Runner) resolveHarnessSelection(ctx context.Context, qw QueuedWork) (harnessSelection, error) {
	if qw.ResolvedProfile.Harness != "" {
		return r.registry.selectExplicitHarness(qw.ResolvedProfile)
	}
	var posterior agent.ProviderName
	if name, ok := r.selectProviderByPosterior(ctx, qw); ok {
		posterior = agent.ProviderName(name)
	}
	return r.registry.legacyHarnessSelectionAdapter(qw.ResolvedProfile, posterior)
}

func (r *Runner) admittedHarnessSelection(ctx context.Context, qw QueuedWork, admission *HarnessAdmission) (harnessSelection, error) {
	if admission == nil {
		return r.resolveHarnessSelection(ctx, qw)
	}
	if admission.registry != r.registry {
		return harnessSelection{}, errors.New("runner: harness admission belongs to a different registry")
	}
	if admission.requestID != qw.SessionID {
		return harnessSelection{}, errors.New("runner: harness admission belongs to a different request")
	}
	if admission.intent != selectorIntent(qw.ResolvedProfile) {
		return harnessSelection{}, errors.New("runner: harness selector intent changed after preflight admission")
	}
	if admission.denial != nil {
		payloadDigest, err := executioncell.DigestContractValue(qw)
		if err != nil {
			return harnessSelection{}, fmt.Errorf("runner: digest denied harness payload: %w", err)
		}
		if payloadDigest != admission.denialPayloadDigest {
			return harnessSelection{}, errors.New("runner: denied harness payload changed after preflight admission")
		}
	}
	if !admission.consumed.CompareAndSwap(false, true) {
		return harnessSelection{}, errors.New("runner: harness admission was already consumed")
	}
	if admission.denial != nil {
		return harnessSelection{}, admission.denial
	}
	return admission.selection, nil
}

func attachDeniedHarnessReceipt(qw QueuedWork, err error, recordedAt time.Time) error {
	var admissionErr *HarnessAdmissionError
	if !errors.As(err, &admissionErr) {
		return err
	}
	if len(admissionErr.Receipt.Bytes()) != 0 {
		return err
	}
	receipt, receiptErr := deniedHarnessAdmissionReceipt(qw, admissionErr, recordedAt)
	if receiptErr != nil {
		return errors.Join(err, fmt.Errorf("persist canonical harness denial evidence: %w", receiptErr))
	}
	admissionErr.Receipt = receipt
	return err
}

func deniedHarnessAdmissionReceipt(qw QueuedWork, denial *HarnessAdmissionError, recordedAt time.Time) (executioncell.ImmutableAdmissionReceipt, error) {
	decisions := append([]executioncell.ResolverDecision{}, denial.Decisions...)
	intentProjection := struct {
		ContractVersion string          `json:"contractVersion"`
		RequestID       string          `json:"requestId"`
		ResolvedProfile ResolvedProfile `json:"resolvedProfile"`
	}{executioncell.ContractVersion, qw.SessionID, qw.ResolvedProfile}
	intentDigest, err := executioncell.DigestContractValue(intentProjection)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, err
	}
	payloadDigest, err := executioncell.DigestContractValue(qw)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, err
	}
	receiptID := "admission_harness_" + intentDigest[:24]
	receipt := executioncell.AdmissionReceipt{
		ContractVersion:          executioncell.ContractVersion,
		ReceiptID:                receiptID,
		RequestID:                qw.SessionID,
		Decision:                 executioncell.AdmissionDenied,
		IntentDigest:             intentDigest,
		OperationalPayloadDigest: payloadDigest,
		DenialCode:               denial.Code,
		DenialDetail:             denial.Detail,
		ResolverDecisions:        decisions,
		RecordedAt:               recordedAt.UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, fmt.Errorf("marshal denied harness admission receipt: %w", err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		return executioncell.ImmutableAdmissionReceipt{}, fmt.Errorf("validate denied harness admission receipt: %w", err)
	}
	return immutable, nil
}
