package runner

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// resolvedProfileWire mirrors the JSON wire shape of the daemon's
// SessionResolvedProfile (daemon/session_detail.go) — the subset of fields
// this reconciliation needs. It is duplicated here rather than imported
// because package daemon already imports package runner (daemon/
// mutation_apply.go); importing daemon back from runner would cycle. The
// shared contract is the JSON field names, not the Go type.
type resolvedProfileWire struct {
	Harness         string                 `json:"harness,omitempty"`
	Provider        string                 `json:"provider,omitempty"`
	Runner          string                 `json:"runner,omitempty"`
	Model           string                 `json:"model,omitempty"`
	Effort          string                 `json:"effort,omitempty"`
	CredentialID    string                 `json:"credentialId,omitempty"`
	ProviderConfig  map[string]any         `json:"providerConfig,omitempty"`
	ContextWindow   int                    `json:"contextWindow,omitempty"`
	MaxOutputTokens int                    `json:"maxOutputTokens,omitempty"`
	Endpoint        *agent.EndpointBinding `json:"endpoint,omitempty"`
}

// ReconcileResolvedProfile applies the platform's per-session model/provider
// resolution to qw.ResolvedProfile: a non-empty modelProfileJSON (the richer
// ADR-2026-05-12 worktype+model-profile routing shape) supersedes
// Provider/Model/Effort; otherwise a non-empty resolvedProfileJSON is applied
// verbatim. Both are raw JSON — see resolvedProfileWire's doc comment for why
// this package cannot decode the daemon's own typed
// SessionModelProfile/SessionResolvedProfile.
//
// This is the exact reconciliation afcli.detailToQueuedWork applies for the
// spawned child (donmai agent run); it now also runs inside the daemon's
// preflight compiler (ProviderView.PreflightExecution): before this, preflight
// only ever saw QueuedWork.ResolvedProfile as embedded (or absent) in
// OperationalPayload, while the spawned child
// authoritatively overwrote qw.ResolvedProfile — Model, Effort,
// ProviderConfig, and Endpoint identity, all genuine authority-digest fields
// — from SessionDetail's sibling ResolvedProfile/ModelProfile fields, which
// preflight never received. The host-compiled plan and the child's
// materialized Spec must derive Model/Effort/ProviderConfig/Endpoint from
// the identical inputs via the identical logic, or ApplyPreparedHarness's
// authority digest can never agree even when nothing genuinely changed
// between preflight and spawn. Ordering: call this AFTER QueuedWork has been
// built from OperationalPayload (or its zero value, when absent) and BEFORE
// computing or applying a PreparedHarness plan.
func ReconcileResolvedProfile(qw QueuedWork, modelProfileJSON, resolvedProfileJSON json.RawMessage) (QueuedWork, error) {
	if len(modelProfileJSON) > 0 {
		var mp ResolvedModelProfile
		if err := json.Unmarshal(modelProfileJSON, &mp); err != nil {
			return qw, fmt.Errorf("runner: decode model profile: %w", err)
		}
		qw.ResolvedProfile = mp.ToResolvedProfile()
		if len(resolvedProfileJSON) == 0 {
			return qw, nil
		}
		rp, err := decodeResolvedProfileWire(resolvedProfileJSON)
		if err != nil {
			return qw, err
		}
		if rp.CredentialID != "" {
			qw.ResolvedProfile.CredentialID = rp.CredentialID
		}
		if rp.ProviderConfig != nil {
			qw.ResolvedProfile.ProviderConfig = rp.ProviderConfig
		}
		qw.ResolvedProfile.ProviderConfig = rp.foldLimits(qw.ResolvedProfile.ProviderConfig)
		endpoint, err := reconciledEndpointBinding(rp.Endpoint)
		if err != nil {
			return qw, err
		}
		qw.ResolvedProfile.Endpoint = endpoint
		return qw, nil
	}
	if len(resolvedProfileJSON) == 0 {
		return qw, nil
	}
	rp, err := decodeResolvedProfileWire(resolvedProfileJSON)
	if err != nil {
		return qw, err
	}
	endpoint, err := reconciledEndpointBinding(rp.Endpoint)
	if err != nil {
		return qw, err
	}
	qw.ResolvedProfile = ResolvedProfile{
		Harness:        rp.Harness,
		Provider:       agent.ProviderName(rp.Provider),
		Runner:         rp.Runner,
		Model:          rp.Model,
		Effort:         agent.EffortLevel(rp.Effort),
		CredentialID:   rp.CredentialID,
		ProviderConfig: rp.foldLimits(rp.ProviderConfig),
		Endpoint:       endpoint,
	}
	return qw, nil
}

func decodeResolvedProfileWire(raw json.RawMessage) (resolvedProfileWire, error) {
	var rp resolvedProfileWire
	if err := json.Unmarshal(raw, &rp); err != nil {
		return resolvedProfileWire{}, fmt.Errorf("runner: decode resolved profile: %w", err)
	}
	return rp, nil
}

// reconciledEndpointBinding applies the same fail-closed BaseURL shape check
// afcli.detailEndpointBinding runs at spawn time (agent.
// ValidateEndpointBindingBaseURL: absolute http(s), no userinfo, https for
// any non-loopback host) so a malformed BaseURL is rejected here — at
// preflight — rather than reaching ApplyPreparedHarness as an undiagnosable
// authority-digest mismatch.
func reconciledEndpointBinding(in *agent.EndpointBinding) (*agent.EndpointBinding, error) {
	if in == nil {
		return nil, nil
	}
	if err := agent.ValidateEndpointBindingBaseURL(in.BaseURL); err != nil {
		return nil, fmt.Errorf("resolved profile endpoint: %w", err)
	}
	out := *in
	return &out, nil
}

// foldLimits folds the resolvedProfile's top-level model limits
// (contextWindow, maxOutputTokens) into pc under the keys
// ResolvedModelProfile.ToResolvedProfile uses, so a provider reads one key
// regardless of which wire field carried the value.
func (rp resolvedProfileWire) foldLimits(pc map[string]any) map[string]any {
	pc = providerConfigWithPositiveInt(pc, "contextWindow", rp.ContextWindow)
	return providerConfigWithPositiveInt(pc, "maxOutputTokens", rp.MaxOutputTokens)
}

// providerConfigWithPositiveInt folds a positive value into pc under key. A
// no-op when value is not positive or pc already carries the key: an explicit
// providerConfig entry (or a modelProfile-derived one) wins over the
// top-level wire field. pc is never mutated; a copy is returned when the key
// is added.
func providerConfigWithPositiveInt(pc map[string]any, key string, value int) map[string]any {
	if value <= 0 {
		return pc
	}
	if _, ok := pc[key]; ok {
		return pc
	}
	// CodeQL (go/allocation-size-overflow) flags an unbounded +1 capacity hint
	// as a possible allocation-size overflow; len(pc) alone is a safe hint and
	// the map still grows correctly when the key is inserted.
	out := make(map[string]any, len(pc))
	for k, v := range pc {
		out[k] = v
	}
	out[key] = value
	return out
}
