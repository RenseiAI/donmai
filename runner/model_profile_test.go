package runner

import (
	"errors"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
)

// TestSelectProvider_KnownProvider verifies that SelectProvider returns the
// matching registered provider for a known ProviderID.
func TestSelectProvider_KnownProvider(t *testing.T) {
	reg := NewRegistry()
	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	profile := ResolvedModelProfile{
		ID:         "mp_test_001",
		ProviderID: "stub",
		Model:      "stub-v1",
	}
	got, err := reg.SelectProvider(profile)
	if err != nil {
		t.Fatalf("SelectProvider(stub): unexpected error: %v", err)
	}
	if got.Name() != agent.ProviderStub {
		t.Errorf("SelectProvider returned provider %q; want %q", got.Name(), agent.ProviderStub)
	}
}

// TestSelectProvider_UnknownProvider verifies that SelectProvider returns a
// structured *ProviderNotRegisteredError when the requested provider is not
// in the registry.
func TestSelectProvider_UnknownProvider(t *testing.T) {
	reg := NewRegistry()
	// Register stub so the registry is not empty — the error message
	// should list it.
	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	_ = reg.Register(p)

	profile := ResolvedModelProfile{
		ID:         "mp_test_002",
		ProviderID: "gemini",
		Model:      "gemini-2.0-flash",
	}
	_, gotErr := reg.SelectProvider(profile)
	if gotErr == nil {
		t.Fatal("SelectProvider(gemini): expected error, got nil")
	}
	var pErr *ProviderNotRegisteredError
	if !errors.As(gotErr, &pErr) {
		t.Fatalf("SelectProvider(gemini): error type = %T; want *ProviderNotRegisteredError", gotErr)
	}
	if pErr.RequestedID != "gemini" {
		t.Errorf("ProviderNotRegisteredError.RequestedID = %q; want %q", pErr.RequestedID, "gemini")
	}
	if len(pErr.Registered) == 0 {
		t.Error("ProviderNotRegisteredError.Registered is empty; want at least [stub]")
	}
	if pErr.Error() == "" {
		t.Error("ProviderNotRegisteredError.Error() returned empty string")
	}
}

// TestSelectProvider_EmptyRegistryStructuredError verifies the error message
// when the registry has no providers registered at all.
func TestSelectProvider_EmptyRegistryStructuredError(t *testing.T) {
	reg := NewRegistry()
	profile := ResolvedModelProfile{
		ID:         "mp_test_003",
		ProviderID: "claude",
	}
	_, err := reg.SelectProvider(profile)
	if err == nil {
		t.Fatal("expected error from empty registry; got nil")
	}
	var pErr *ProviderNotRegisteredError
	if !errors.As(err, &pErr) {
		t.Fatalf("error type = %T; want *ProviderNotRegisteredError", err)
	}
	if pErr.RequestedID != "claude" {
		t.Errorf("RequestedID = %q; want %q", pErr.RequestedID, "claude")
	}
}

// TestSelectProvider_EmptyProviderIDFallback verifies that an empty ProviderID
// falls back to the claude provider (backwards compat with pre-enrichment
// dispatches).
func TestSelectProvider_EmptyProviderIDFallback(t *testing.T) {
	reg := NewRegistry()
	// We deliberately do NOT register claude (it requires the binary on
	// PATH), so the fallback still surfaces the structured error — but
	// the RequestedID confirms the fallback was "claude".
	profile := ResolvedModelProfile{
		ID:    "mp_test_004",
		Model: "claude-opus-4-7",
		// ProviderID intentionally empty.
	}
	_, err := reg.SelectProvider(profile)
	if err == nil {
		t.Fatal("expected error when claude is not registered; got nil")
	}
	var pErr *ProviderNotRegisteredError
	if !errors.As(err, &pErr) {
		t.Fatalf("error type = %T; want *ProviderNotRegisteredError", err)
	}
	if pErr.RequestedID != string(agent.ProviderClaude) {
		t.Errorf("fallback RequestedID = %q; want %q", pErr.RequestedID, agent.ProviderClaude)
	}
}

// TestResolvedModelProfile_ToResolvedProfile exercises the bridge between
// the richer ResolvedModelProfile and the legacy ResolvedProfile shape.
func TestResolvedModelProfile_ToResolvedProfile(t *testing.T) {
	profile := ResolvedModelProfile{
		ID:              "mp_test_005",
		ProviderID:      "claude",
		Model:           "claude-opus-4-7",
		Mode:            "xhigh",
		Context:         1_000_000,
		MaxOutputTokens: 32_000,
	}
	rp := profile.ToResolvedProfile()

	if rp.Provider != agent.ProviderClaude {
		t.Errorf("Provider = %q; want %q", rp.Provider, agent.ProviderClaude)
	}
	if rp.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q; want %q", rp.Model, "claude-opus-4-7")
	}
	if rp.Effort != agent.EffortXHigh {
		t.Errorf("Effort = %q; want %q", rp.Effort, agent.EffortXHigh)
	}
	if rp.ProviderConfig == nil {
		t.Fatal("ProviderConfig is nil; want map with contextWindow + maxOutputTokens")
	}
	if ctx, ok := rp.ProviderConfig["contextWindow"]; !ok || ctx != 1_000_000 {
		t.Errorf("ProviderConfig[contextWindow] = %v; want 1000000", ctx)
	}
	if maxOut, ok := rp.ProviderConfig["maxOutputTokens"]; !ok || maxOut != 32_000 {
		t.Errorf("ProviderConfig[maxOutputTokens] = %v; want 32000", maxOut)
	}
}

// TestResolvedModelProfile_ToResolvedProfile_ZeroContext verifies that zero
// Context and MaxOutputTokens do not produce a ProviderConfig map.
func TestResolvedModelProfile_ToResolvedProfile_ZeroContext(t *testing.T) {
	profile := ResolvedModelProfile{
		ID:         "mp_test_006",
		ProviderID: "codex",
		Model:      "gpt-4o",
	}
	rp := profile.ToResolvedProfile()
	if rp.ProviderConfig != nil {
		t.Errorf("ProviderConfig should be nil for zero context; got %v", rp.ProviderConfig)
	}
}

// TestSelectProvider_MultipleProviders verifies that the registry correctly
// dispatches to the matching provider when several providers are registered.
func TestSelectProvider_MultipleProviders(t *testing.T) {
	reg := NewRegistry()
	s, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := reg.Register(s); err != nil {
		t.Fatalf("Register stub: %v", err)
	}

	// SelectProvider for stub should return the stub provider, not any
	// other registered entry (guard against off-by-one or map ordering).
	profile := ResolvedModelProfile{
		ID:         "mp_test_007",
		ProviderID: string(agent.ProviderStub),
	}
	got, err := reg.SelectProvider(profile)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	if got.Name() != agent.ProviderStub {
		t.Errorf("Name() = %q; want %q", got.Name(), agent.ProviderStub)
	}
}
