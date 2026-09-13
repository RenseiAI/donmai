package runner

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func TestProviderViewPreparedSourcePassesAdmittedEnvironmentToDecorator(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	providerWithManifest := &manifestSelectorProvider{
		selectorFakeProvider: provider,
		manifest:             piManifestForTest(),
		capabilities:         piCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}

	const sessionID = "prepared-source-environment"
	const runtimeBearer = "runtime-bearer-must-not-enter-spec-env"
	qw := QueuedWork{}
	qw.SessionID = sessionID
	qw.Mode = interactiveRunMode
	qw.InitialPrompt = "compile with exact session environment"
	qw.Env = map[string]string{
		"PUBLIC_SERVICE_ORIGIN": "https://service.example.invalid",
		"SESSION_FEATURES":      `["draft"]`,
	}
	qw.AuthToken = runtimeBearer
	qw.McpAuthToken = runtimeBearer
	qw.ResolvedProfile = ResolvedProfile{
		Harness: string(agent.HarnessPi), Model: "test-model",
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyOpenAI, Model: "test-model", Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect,
			EndpointID: "endpoint:openai/direct", EndpointOperator: "vercel", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
			AuthBindingID: "auth-binding:byok", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
			AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
			AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
		},
	}
	cell := piReceiptCell("harness/v2", "test-model", executioncell.SessionHumanControlled, nil)
	qw = attachAdmittedExecutionCell(t, qw, cell)
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational

	detail := rawJSONForRunner(t, map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID,
		"admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell,
		"executionRuntimeBinding": qw.ExecutionRuntimeBinding,
		"operationalPayload":      qw.OperationalPayload,
	})
	decoded, _, err := decodeProviderViewPreflightWork(detail)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(decoded.Env, qw.Env) {
		t.Fatalf("decoded env = %#v, want %#v", decoded.Env, qw.Env)
	}
	if decoded.AuthToken != "" || decoded.McpAuthToken != "" {
		t.Fatal("runtime bearer crossed the operational decoder boundary")
	}

	var observed map[string]string
	decorate := func(spec agent.Spec) []agent.ExtensionDelivery {
		observed = maps.Clone(spec.Env)
		if spec.Env != nil {
			spec.Env["DECORATOR_LOCAL_MUTATION"] = "must-not-alias-queued-work"
		}
		return nil
	}
	receipt, err := NewProviderViewWithDecorator(registry, decorate).PreflightExecution(detail)
	if err != nil {
		t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
	}
	if !maps.Equal(observed, qw.Env) {
		t.Fatalf("decorator observed env = %#v, want %#v", observed, qw.Env)
	}
	if _, _, err := buildPreparedSourceSpec(qw, harnessSelection{
		Provider:      providerWithManifest,
		receipt:       mustAdmissionReceipt(t, qw.AdmissionReceipt),
		effectiveCell: cell,
	}, decorate); err != nil {
		t.Fatal(err)
	}
	if _, present := qw.Env["DECORATOR_LOCAL_MUTATION"]; present {
		t.Fatal("decorator mutation aliased queued environment authority")
	}
	for _, value := range observed {
		if value == runtimeBearer {
			t.Fatal("runtime bearer leaked into prepared environment")
		}
	}
	var host executioncell.HostAdaptationReceipt
	if err := json.Unmarshal(receipt, &host); err != nil {
		t.Fatal(err)
	}
	if host.Decision != "ready" {
		t.Fatalf("host decision = %q, want ready", host.Decision)
	}
}
