package executioncell

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/prompt"
)

func legacyContext() LegacyAdapterContext {
	return LegacyAdapterContext{
		HarnessRefsByLegacyID: map[string]HarnessRef{
			"claude": {ID: "claude-code", Version: "pinned-1"},
		},
		ModelAuthorsByProvider: map[string]string{"claude": "anthropic"},
		EndpointsByServingHost: map[string]ServingEndpointRef{
			"oauth-cli": {
				ID: "anthropic-cli-subscription", Protocol: "anthropic-messages",
				Operator: "anthropic", Revision: "2026-08-05",
			},
		},
		DefaultServingHostByAuthMode: map[string]string{"host-session": "oauth-cli"},
		AuthBindingsByMode: map[string]AuthBindingRef{
			"host-session": {
				ID: "auth_claude_host_subscription", Mechanism: AuthCLISession,
				CommercialMode: CommercialSubscription, Authority: "anthropic",
				BindingScope: ScopeHost, Portability: HostBound,
			},
		},
		Placement: PlacementRef{ID: "pool_host_subscriptions", Kind: PlacementPool, Resolution: PlacementClaimBound},
	}
}

func legacyProfile() LegacyResolvedProfile {
	return LegacyResolvedProfile{
		Harness: "claude", Provider: "claude", Model: "claude-opus-4-7",
		ServingHost: "oauth-cli", AuthMode: "host-session",
	}
}

func TestQueuedWorkJSONAdapterRoundTripsEveryOperationalByte(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"sessionId":"session_legacy_roundtrip","issueIdentifier":"TASK-1","repository":"example/repo","ref":"main","workType":"development","mode":"","allowedTools":[],"mcpServers":[],"futureOperationalField":{"enabled":true}}`)
	adapted, err := AdaptQueuedWorkJSON(raw, legacyProfile(), legacyContext())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ProjectQueuedWork(adapted), raw) {
		t.Fatalf("operational projection changed bytes\n got: %s\nwant: %s", ProjectQueuedWork(adapted), raw)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal(ProjectQueuedWork(adapted), &projected); err != nil {
		t.Fatal(err)
	}
	if _, ok := projected["allowedTools"]; !ok || string(projected["allowedTools"]) != "[]" {
		t.Fatal("present-empty allowedTools was not preserved")
	}
	if _, ok := projected["disallowedTools"]; ok {
		t.Fatal("absent disallowedTools became present")
	}
	if _, ok := projected["futureOperationalField"]; !ok {
		t.Fatal("unknown operational field was dropped")
	}
	if adapted.Intent.Harness == nil || adapted.Intent.Harness.ID != "claude-code" {
		t.Fatalf("harness = %+v", adapted.Intent.Harness)
	}
	if adapted.Intent.Model.Author != "anthropic" || adapted.Intent.Endpoint == nil || adapted.Intent.Endpoint.Operator != "anthropic" {
		t.Fatalf("independent model/endpoint projection failed: %+v", adapted.Intent)
	}
	if adapted.Intent.AuthBinding == nil || adapted.Intent.AuthBinding.Mechanism != AuthCLISession || adapted.Intent.AuthBinding.CommercialMode != CommercialSubscription {
		t.Fatalf("auth binding = %+v", adapted.Intent.AuthBinding)
	}
	if adapted.Intent.Placement == nil || adapted.Intent.Placement.Resolution != PlacementClaimBound {
		t.Fatalf("placement = %+v", adapted.Intent.Placement)
	}
	decisionFields := make([]string, 0, len(adapted.ResolverDecisions))
	for _, decision := range adapted.ResolverDecisions {
		if decision.Kind != DecisionExplicit {
			decisionFields = append(decisionFields, decision.Field)
		}
	}
	for _, want := range []string{"model", "authBinding", "placement", "sessionMode", "requiredCapabilities.allowed-tools", "requiredCapabilities.mcp"} {
		if !slices.Contains(decisionFields, want) {
			t.Errorf("resolver decisions %v do not contain %q", decisionFields, want)
		}
	}
}

func TestTypedQueuedWorkAdapterCompilesAgainstRealType(t *testing.T) {
	t.Parallel()
	work := prompt.QueuedWork{
		SessionID: "session_typed", IssueIdentifier: "TASK-2", Repository: "example/repo",
		AllowedTools: []string{"Read"},
	}
	adapted, err := AdaptQueuedWork(work, legacyProfile(), legacyContext())
	if err != nil {
		t.Fatal(err)
	}
	var projected prompt.QueuedWork
	if err := json.Unmarshal(ProjectQueuedWork(adapted), &projected); err != nil {
		t.Fatal(err)
	}
	if projected.SessionID != work.SessionID || !slices.Equal(projected.AllowedTools, work.AllowedTools) {
		t.Fatalf("typed round trip = %+v, want %+v", projected, work)
	}
}

func TestQueuedWorkAdapterFailsUnknownExplicitSelector(t *testing.T) {
	t.Parallel()
	profile := legacyProfile()
	profile.Harness = "unknown-harness"
	_, err := AdaptQueuedWork(prompt.QueuedWork{SessionID: "session_invalid"}, profile, legacyContext())
	var contractErr *ContractError
	if !errors.As(err, &contractErr) || contractErr.Code != ErrorInvalidReference {
		t.Fatalf("error = %v, want typed invalid_reference", err)
	}
}
