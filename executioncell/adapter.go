package executioncell

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/RenseiAI/donmai/prompt"
	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// LegacyResolvedProfile is the execution-axis subset of the current queued
// profile. It intentionally keeps the legacy fused strings at the adapter
// boundary; the resulting DispatchIntent separates every axis.
type LegacyResolvedProfile struct {
	Harness      string
	Provider     string
	Runner       string
	Model        string
	ServingHost  string
	AuthMode     string
	CredentialID string
}

// LegacyAdapterContext makes every catalog/default mapping explicit. The
// adapter never derives model author from endpoint operator, auth authority
// from model author, or endpoint identity from harness branding.
type LegacyAdapterContext struct {
	RequestID                    string
	HarnessRefsByLegacyID        map[string]HarnessRef
	ModelAuthorsByProvider       map[string]string
	DefaultModelsByProvider      map[string]ModelRef
	EndpointsByServingHost       map[string]ServingEndpointRef
	DefaultServingHostByAuthMode map[string]string
	AuthBindingsByMode           map[string]AuthBindingRef
	Placement                    PlacementRef
	RequiredCapabilities         []CapabilityRequest
	OptionalCapabilities         []CapabilityRequest
	FallbackAlternatives         FallbackPolicy
}

// LegacyAdaptation is a sidecar. OperationalPayload contains the original JSON
// bytes, and ProjectQueuedWork returns a defensive byte copy, so no prompt,
// stage, budget, kit, policy, MCP, skill, memory, or future field is dropped.
type LegacyAdaptation struct {
	Intent                   DispatchIntent
	OperationalPayload       json.RawMessage
	OperationalPayloadDigest string
	ResolverDecisions        []ResolverDecision
}

func selectedRef(kind, value string) string { return kind + ":" + value }

func legacyMapped[T any](values map[string]T, key, kind string, path []string) (T, error) {
	value, ok := values[key]
	if !ok {
		var zero T
		return zero, contractError(ErrorInvalidReference, path, "unknown legacy %s selector %q", kind, key)
	}
	return value, nil
}

func inferHarness(profile LegacyResolvedProfile, context LegacyAdapterContext) (HarnessRef, ResolverDecision, error) {
	legacyID := profile.Harness
	explicit := legacyID != ""
	if legacyID == "" {
		legacyID = profile.Provider
	}
	if legacyID == "" {
		legacyID = profile.Runner
	}
	if legacyID == "" {
		return HarnessRef{}, ResolverDecision{}, contractError(ErrorMissingRequiredField, []string{"resolvedProfile", "harness"}, "legacy profile has no harness, provider, or runner selector")
	}
	ref, err := legacyMapped(context.HarnessRefsByLegacyID, legacyID, "harness", []string{"resolvedProfile", "harness"})
	if err != nil {
		return HarnessRef{}, ResolverDecision{}, err
	}
	kind := DecisionLegacyInference
	reason := "Legacy profile omitted harness; the documented provider/runner mapping supplied it."
	if explicit {
		kind = DecisionExplicit
		reason = "Legacy profile carried an explicit harness selector."
	}
	return ref, ResolverDecision{
		Kind: kind, Field: "harness",
		SelectedRef: selectedRef("harness", ref.ID+"@"+ref.Version),
		SourceRef:   selectedRef("legacy-harness", legacyID), Reason: reason,
	}, nil
}

func profileProvider(profile LegacyResolvedProfile) string {
	if profile.Provider != "" {
		return profile.Provider
	}
	if profile.Runner != "" {
		return profile.Runner
	}
	return profile.Harness
}

func inferModel(profile LegacyResolvedProfile, context LegacyAdapterContext) (ModelRef, ResolverDecision, error) {
	provider := profileProvider(profile)
	if profile.Model == "" {
		ref, ok := context.DefaultModelsByProvider[provider]
		if !ok {
			return ModelRef{}, ResolverDecision{}, contractError(ErrorMissingRequiredField, []string{"resolvedProfile", "model"}, "legacy profile has no model and no documented default for %q", provider)
		}
		return ref, ResolverDecision{
			Kind: DecisionLegacyInference, Field: "model",
			SelectedRef: selectedRef("model", ref.Author+"/"+ref.ID),
			SourceRef:   selectedRef("legacy-provider", provider),
			Reason:      "Legacy profile omitted model; the documented provider default supplied it.",
		}, nil
	}
	author, ok := context.ModelAuthorsByProvider[provider]
	if !ok {
		return ModelRef{}, ResolverDecision{}, contractError(ErrorInvalidReference, []string{"resolvedProfile", "provider"}, "no model-author mapping for legacy provider %q", provider)
	}
	ref := ModelRef{ID: profile.Model, Author: author}
	return ref, ResolverDecision{
		Kind: DecisionLegacyInference, Field: "model",
		SelectedRef: selectedRef("model", author+"/"+profile.Model),
		SourceRef:   selectedRef("legacy-provider", provider),
		Reason:      "Legacy provider/model fields supplied model identity; author came from catalog metadata.",
	}, nil
}

func inferEndpoint(profile LegacyResolvedProfile, context LegacyAdapterContext) (ServingEndpointRef, ResolverDecision, error) {
	servingHost := profile.ServingHost
	explicit := servingHost != ""
	if servingHost == "" {
		servingHost = context.DefaultServingHostByAuthMode[profile.AuthMode]
	}
	if servingHost == "" {
		return ServingEndpointRef{}, ResolverDecision{}, contractError(ErrorMissingRequiredField, []string{"resolvedProfile", "servingHost"}, "legacy profile has no serving host or documented auth-mode default")
	}
	ref, err := legacyMapped(context.EndpointsByServingHost, servingHost, "serving endpoint", []string{"resolvedProfile", "servingHost"})
	if err != nil {
		return ServingEndpointRef{}, ResolverDecision{}, err
	}
	kind := DecisionLegacyInference
	reason := "Legacy profile omitted serving host; the documented auth-mode default supplied it."
	if explicit {
		kind = DecisionExplicit
		reason = "Legacy profile carried an explicit serving-host selector."
	}
	return ref, ResolverDecision{
		Kind: kind, Field: "endpoint", SelectedRef: selectedRef("endpoint", ref.ID),
		SourceRef: selectedRef("legacy-serving-host", servingHost), Reason: reason,
	}, nil
}

func inferAuthBinding(profile LegacyResolvedProfile, context LegacyAdapterContext) (AuthBindingRef, ResolverDecision, error) {
	ref, ok := context.AuthBindingsByMode[profile.AuthMode]
	if !ok {
		return AuthBindingRef{}, ResolverDecision{}, contractError(ErrorInvalidReference, []string{"resolvedProfile", "authMode"}, "unknown legacy auth mode selector %q", profile.AuthMode)
	}
	if profile.CredentialID != "" {
		ref.ID = profile.CredentialID
	}
	return ref, ResolverDecision{
		Kind: DecisionLegacyInference, Field: "authBinding",
		SelectedRef: selectedRef("auth-binding", ref.ID),
		SourceRef:   selectedRef("legacy-auth-mode", profile.AuthMode),
		Reason:      "Legacy auth mode conflated mechanism and commercial mode; non-secret metadata supplied authority, scope, and portability.",
	}, nil
}

func rawString(fields map[string]json.RawMessage, names ...string) string {
	for _, name := range names {
		var value string
		if raw, ok := fields[name]; ok && json.Unmarshal(raw, &value) == nil && value != "" {
			return value
		}
	}
	return ""
}

func inferPlacement(fields map[string]json.RawMessage, context LegacyAdapterContext) (PlacementRef, ResolverDecision) {
	if hostID := rawString(fields, "workerHostId", "hostId"); hostID != "" {
		ref := PlacementRef{ID: hostID, Kind: PlacementHost, Resolution: PlacementExact}
		return ref, ResolverDecision{
			Kind: DecisionExplicit, Field: "placement", SelectedRef: selectedRef("placement", hostID),
			SourceRef: selectedRef("legacy-host", hostID), Reason: "Legacy queued work carried an exact host selector.",
		}
	}
	if poolID := rawString(fields, "executionPoolId", "poolId"); poolID != "" {
		ref := PlacementRef{ID: poolID, Kind: PlacementPool, Resolution: PlacementClaimBound}
		return ref, ResolverDecision{
			Kind: DecisionExplicit, Field: "placement", SelectedRef: selectedRef("placement", poolID),
			SourceRef: selectedRef("legacy-pool", poolID), Reason: "Legacy queued work carried a claim-bound pool selector.",
		}
	}
	return context.Placement, ResolverDecision{
		Kind: DecisionLegacyInference, Field: "placement",
		SelectedRef: selectedRef("placement", context.Placement.ID),
		Reason:      "Legacy queued work omitted placement; the existing scheduler route supplied it.",
	}
}

func inferSessionMode(mode string) (SessionMode, ResolverDecision, error) {
	switch mode {
	case "interactive", "interview", "human_controlled":
		return SessionHumanControlled, ResolverDecision{
			Kind: DecisionLegacyInference, Field: "sessionMode",
			SelectedRef: selectedRef("session-mode", string(SessionHumanControlled)),
			SourceRef:   selectedRef("legacy-mode", mode),
			Reason:      "Legacy interactive/interview mode maps to the common human-controlled session mode.",
		}, nil
	case "", "headless", "autonomous":
		kind := DecisionDefault
		source := ""
		reason := "Legacy queued work omitted mode; existing dispatch semantics default to autonomous."
		if mode != "" {
			kind = DecisionLegacyInference
			source = selectedRef("legacy-mode", mode)
			reason = "Legacy headless/autonomous mode maps to the common autonomous session mode."
		}
		return SessionAutonomous, ResolverDecision{
			Kind: kind, Field: "sessionMode", SelectedRef: selectedRef("session-mode", string(SessionAutonomous)),
			SourceRef: source, Reason: reason,
		}, nil
	default:
		return "", ResolverDecision{}, contractError(ErrorUnknownDiscriminator, []string{"mode"}, "unknown legacy session mode %q", mode)
	}
}

func digestRawValue(raw json.RawMessage) (string, error) {
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", contractError(ErrorInvalidReference, nil, "canonicalize operational payload value: %v", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func deriveCapabilities(work prompt.QueuedWork, fields map[string]json.RawMessage, context LegacyAdapterContext) ([]CapabilityRequest, []CapabilityRequest, []ResolverDecision, error) {
	required := slices.Clone(context.RequiredCapabilities)
	optional := slices.Clone(context.OptionalCapabilities)
	if required == nil {
		required = []CapabilityRequest{}
	}
	if optional == nil {
		optional = []CapabilityRequest{}
	}
	decisions := []ResolverDecision{}
	add := func(name, field string, value any) error {
		raw, ok := fields[field]
		if !ok {
			return nil
		}
		digest, err := digestRawValue(raw)
		if err != nil {
			return err
		}
		request := CapabilityRequest{Name: name, ParametersDigest: digest}
		found := false
		for _, existing := range required {
			if existing == request {
				found = true
				break
			}
		}
		if !found {
			required = append(required, request)
		}
		decisions = append(decisions, ResolverDecision{
			Kind: DecisionLegacyInference, Field: "requiredCapabilities." + name,
			SelectedRef: selectedRef("capability", name),
			Reason:      fmt.Sprintf("Legacy operational field %s maps to required capability %s; only its digest enters the contract.", field, name),
		})
		_ = value // the typed field proves this is a real QueuedWork surface.
		return nil
	}
	for _, item := range []struct {
		name, field string
		value       any
	}{
		{"allowed-tools", "allowedTools", work.AllowedTools},
		{"disallowed-tools", "disallowedTools", work.DisallowedTools},
		{"mcp", "mcpServers", work.McpServers},
		{"skills", "skills", work.Skills},
		{"kits", "kits", work.Kits},
		{"code_intelligence", "codeIntel", work.CodeIntel},
		{"services", "serviceBlocks", nil},
	} {
		if err := add(item.name, item.field, item.value); err != nil {
			return nil, nil, nil, err
		}
	}
	mode, _, err := inferSessionMode(work.Mode)
	if err != nil {
		return nil, nil, nil, err
	}
	if mode == SessionHumanControlled {
		for _, name := range []string{"watch", "input", "take_control"} {
			found := false
			for _, existing := range required {
				if existing.Name == name {
					found = true
					break
				}
			}
			if !found {
				required = append(required, CapabilityRequest{Name: name})
			}
		}
	}
	return required, optional, decisions, nil
}

// AdaptQueuedWork adapts the real Go prompt.QueuedWork type. Callers that still
// possess the original wire JSON should use AdaptQueuedWorkJSON so present-empty
// fields and unknown operational extensions remain byte-for-byte available.
func AdaptQueuedWork(work prompt.QueuedWork, profile LegacyResolvedProfile, context LegacyAdapterContext) (LegacyAdaptation, error) {
	raw, err := json.Marshal(work)
	if err != nil {
		return LegacyAdaptation{}, fmt.Errorf("marshal queued work: %w", err)
	}
	return adaptQueuedWork(work, raw, profile, context)
}

// AdaptQueuedWorkJSON decodes the current prompt.QueuedWork projection for axis
// adaptation while retaining the full input bytes as the operational sidecar.
// Unknown operational fields are intentionally tolerated and preserved.
func AdaptQueuedWorkJSON(raw []byte, profile LegacyResolvedProfile, context LegacyAdapterContext) (LegacyAdaptation, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return LegacyAdaptation{}, fmt.Errorf("decode queued work: %w", err)
	}
	var work prompt.QueuedWork
	if err := json.Unmarshal(raw, &work); err != nil {
		return LegacyAdaptation{}, fmt.Errorf("decode queued work: %w", err)
	}
	return adaptQueuedWork(work, raw, profile, context)
}

func adaptQueuedWork(work prompt.QueuedWork, raw []byte, profile LegacyResolvedProfile, context LegacyAdapterContext) (LegacyAdaptation, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return LegacyAdaptation{}, contractError(ErrorInvalidReference, nil, "operational payload must be a JSON object")
	}
	requestID := context.RequestID
	if requestID == "" {
		requestID = work.SessionID
	}
	if requestID == "" {
		return LegacyAdaptation{}, contractError(ErrorMissingRequiredField, []string{"requestId"}, "requestId or legacy sessionId is required")
	}
	harness, harnessDecision, err := inferHarness(profile, context)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	model, modelDecision, err := inferModel(profile, context)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	endpoint, endpointDecision, err := inferEndpoint(profile, context)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	authBinding, authDecision, err := inferAuthBinding(profile, context)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	placement, placementDecision := inferPlacement(fields, context)
	sessionMode, sessionDecision, err := inferSessionMode(work.Mode)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	required, optional, capabilityDecisions, err := deriveCapabilities(work, fields, context)
	if err != nil {
		return LegacyAdaptation{}, err
	}
	fallbacks := slices.Clone(context.FallbackAlternatives)
	if fallbacks == nil {
		fallbacks = FallbackPolicy{}
	}
	intent := DispatchIntent{
		ContractVersion: ContractVersion, RequestID: requestID,
		Harness: &harness, Model: model, Endpoint: &endpoint,
		AuthBinding: &authBinding, Placement: &placement, SessionMode: sessionMode,
		RequiredCapabilities: required, OptionalCapabilities: optional,
		FallbackAlternatives: fallbacks,
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return LegacyAdaptation{}, fmt.Errorf("marshal adapted intent: %w", err)
	}
	intent, err = DecodeDispatchIntent(intentJSON, nil)
	if err != nil {
		return LegacyAdaptation{}, fmt.Errorf("validate adapted intent: %w", err)
	}
	canonicalPayload, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return LegacyAdaptation{}, contractError(ErrorInvalidReference, nil, "canonicalize operational payload: %v", err)
	}
	payloadDigest := sha256.Sum256(canonicalPayload)
	decisions := []ResolverDecision{
		harnessDecision, modelDecision, endpointDecision, authDecision,
		placementDecision, sessionDecision,
	}
	decisions = append(decisions, capabilityDecisions...)
	return LegacyAdaptation{
		Intent: intent, OperationalPayload: bytes.Clone(raw),
		OperationalPayloadDigest: hex.EncodeToString(payloadDigest[:]),
		ResolverDecisions:        decisions,
	}, nil
}

// ProjectQueuedWork returns the untouched legacy operational JSON.
func ProjectQueuedWork(adaptation LegacyAdaptation) []byte {
	return bytes.Clone(adaptation.OperationalPayload)
}
