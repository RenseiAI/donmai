package runner

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// ResolvedModelProfile is the fully-rendered provider/model specification
// the platform passes with each dispatch (ADR-2026-05-12-worktype-and-model-
// profile-routing). The Go daemon receives this from the platform as part of
// the poll response and uses it to select the correct provider implementation
// instead of falling back to the local-config default.
//
// JSON tags follow the platform-side camelCase wire shape so the struct can
// be embedded directly in HTTP request/response bodies.
//
// Relationship to ResolvedProfile: ResolvedModelProfile is the richer,
// platform-resolved shape carrying ID + context window + output-token caps.
// ResolvedProfile (runner/types.go) is the legacy shape used by QueuedWork.
// When a dispatch includes a ResolvedModelProfile it supersedes the
// ResolvedProfile fields; detailToQueuedWork (afcli/agent_run.go) bridges
// the two shapes.
type ResolvedModelProfile struct {
	// ID is the model_profile row UUID from the platform's model_profiles
	// table (e.g. "mp_01jt5..."). Used for audit logging and for
	// cross-referencing the platform's profile management API.
	ID string `json:"id"`

	// ProviderID is the canonical provider family identifier
	// (e.g. "claude", "codex", "gemini", "ollama"). Matches the
	// agent.ProviderName enum; the daemon calls SelectProvider(profile)
	// which converts this to agent.ProviderName internally.
	ProviderID string `json:"providerId"`

	// Model is the model variant within the provider family
	// (e.g. "claude-opus-4-7", "gpt-4o-2025-04"). Empty falls back to
	// the provider's built-in default model.
	Model string `json:"model"`

	// Mode is the reasoning-effort/speed tier string the platform
	// resolved (e.g. "xhigh", "high", "medium", "low"). Maps onto
	// agent.EffortLevel; empty falls back to the provider default.
	Mode string `json:"mode,omitempty"`

	// Context is the context-window size in tokens the platform requires
	// for this dispatch (e.g. 1_000_000 for claude-3-7-sonnet-1m).
	// Zero means "use the model default".
	Context int `json:"context,omitempty"`

	// MaxOutputTokens is the per-response output-token budget the
	// platform resolver picked from the model catalog row. Zero means
	// "use the model default".
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// ToResolvedProfile converts a ResolvedModelProfile into the legacy
// runner.ResolvedProfile shape so callers can merge it into a QueuedWork
// without knowing both types. Fields not present in the legacy shape
// (Context, MaxOutputTokens) are injected via ProviderConfig so
// providers that honor extended knobs can consume them.
func (p ResolvedModelProfile) ToResolvedProfile() ResolvedProfile {
	rp := ResolvedProfile{
		Provider: agent.ProviderName(p.ProviderID),
		Model:    p.Model,
		Effort:   agent.EffortLevel(p.Mode),
	}
	// Carry Context + MaxOutputTokens into ProviderConfig so providers
	// that support extended context windows (e.g. claude 1M) can pass
	// them to the upstream API without requiring changes to the Spec
	// type. Values are omitted when zero (provider default).
	if p.Context > 0 || p.MaxOutputTokens > 0 {
		pc := make(map[string]any)
		if p.Context > 0 {
			pc["contextWindow"] = p.Context
		}
		if p.MaxOutputTokens > 0 {
			pc["maxOutputTokens"] = p.MaxOutputTokens
		}
		rp.ProviderConfig = pc
	}
	return rp
}

// SelectProvider resolves a provider implementation from the registry
// using the fully-rendered ResolvedModelProfile. It is the preferred
// dispatch entry point when the platform has already resolved the profile
// end-to-end; callers that only have a raw ProviderName should use
// Registry.Resolve directly.
//
// Lookup order:
//  1. profile.ProviderID (exact match against registered provider names)
//  2. If unregistered, return a descriptive *ProviderNotRegisteredError
//
// An empty ProviderID falls back to agent.ProviderClaude for backwards
// compatibility with dispatches that arrive before the platform ships
// the enriched profile.
//
// Returns (nil, *ProviderNotRegisteredError) when the requested provider is
// not registered on this host. The error carries the ProviderID and the
// names currently registered so the caller can log a useful diagnostic.
func (r *Registry) SelectProvider(profile ResolvedModelProfile) (agent.Provider, error) {
	providerID := strings.TrimSpace(profile.ProviderID)
	if providerID == "" {
		// Backwards-compat: empty profile means no platform enrichment
		// was present; fall back to the same default as resolvedProvider().
		providerID = string(agent.ProviderClaude)
	}
	name := agent.ProviderName(providerID)
	p, err := r.Resolve(name)
	if err != nil {
		// Wrap in a structured error so callers can inspect fields.
		return nil, &ProviderNotRegisteredError{
			RequestedID: providerID,
			Registered:  r.registeredNames(),
		}
	}
	return p, nil
}

// ProviderNotRegisteredError is returned by SelectProvider when the
// requested provider family is not registered on this host. It is a
// structured error (not a plain string) so daemon dispatch code can log
// the exact requested + available set in one log line.
type ProviderNotRegisteredError struct {
	// RequestedID is the ProviderID from the ResolvedModelProfile.
	RequestedID string
	// Registered is the snapshot of provider names at call time.
	Registered []string
}

func (e *ProviderNotRegisteredError) Error() string {
	if len(e.Registered) == 0 {
		return fmt.Sprintf("provider %q is not registered on this host (registry is empty)", e.RequestedID)
	}
	return fmt.Sprintf("provider %q is not registered on this host (registered: %s)",
		e.RequestedID, strings.Join(e.Registered, ", "))
}

// registeredNames returns a snapshot of registered provider names as
// plain strings (no locking beyond what Resolve already holds).
func (r *Registry) registeredNames() []string {
	names := r.Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out
}
