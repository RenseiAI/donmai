package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func localAdmissionFixture(t *testing.T) (*Registry, LocalAdmissionConfig, LocalAdmissionRequest) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink("/bin/sh", binary); err != nil {
		t.Fatal(err)
	}
	provider, err := codex.New(codex.Options{CodexBin: binary})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: RuntimeTransportLocal})
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	model := executioncell.ModelRef{ID: "gpt-5-codex", Author: "openai"}
	config := LocalAdmissionConfig{ScopeID: "local-org", WorkerID: "local-worker", PlacementID: "local-host", ConfigurationRevision: "config-1", Harness: agent.HarnessCodex, Model: model, ModelCatalogEntry: &LocalModelCatalogEntry{Model: model, Host: agent.HostOAuthCLI, Revision: "operator-catalog-1"}, EndpointID: "local-openai", AuthBindingID: "local-login", BinaryPath: binary, CheckHostAuth: func(context.Context) error { return nil }}
	request := LocalAdmissionRequest{SessionID: "local-session", ReceiptID: "local-admission", PreflightChallengeID: "local-challenge"}
	request.Work.Body = "Implement the authored issue."
	request.Work.WorkType = "development"
	return registry, config, request
}

func admitLocalFixture(t *testing.T, registry *Registry, config LocalAdmissionConfig, request LocalAdmissionRequest) *LocalAdmission {
	t.Helper()
	producer, err := NewLocalAdmissionProducer(registry, config)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := producer.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

func TestLocalAdmissionCompilesExactHostEvidence(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	got := admitLocalFixture(t, registry, config, request)
	receipt, err := executioncell.DecodeAdmissionReceipt(got.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = executioncell.AssertAdmissionProvenance(got.Intent, receipt); err != nil {
		t.Fatal(err)
	}
	value := receipt.Value()
	if value.Decision != executioncell.AdmissionAdmitted || value.Cell.EvidenceTier != executioncell.EvidenceImplemented {
		t.Fatalf("unexpected admission/evidence: %+v", value)
	}
	binding, err := executioncell.DecodeRuntimeBinding(got.RuntimeBinding)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ContractVersion != executioncell.RuntimeBindingV2ContractVersion || binding.ClaimID != "" || got.Session.ClaimReceiptID != "" || binding.PlacementID != config.PlacementID {
		t.Fatalf("not exact-host binding: %+v", binding)
	}
	if binding.PreflightRegistration == nil || !binding.PreflightRegistration.Required {
		t.Fatal("missing real start-registration requirement")
	}
	if got.Session.Capabilities != (executioncell.SessionCapabilities{}) {
		t.Fatal("unmeasured lifecycle capability advertised")
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(got.HostPreflight)
	if err != nil {
		t.Fatal(err)
	}
	if host.Decision != "ready" || host.PlanDigest == "" {
		t.Fatal("host compiler did not produce ready plan")
	}
	if bytes.Contains(got.Evidence, []byte(config.BinaryPath)) {
		t.Fatal("host path leaked into evidence")
	}
	var evidence struct {
		Configuration json.RawMessage
		Inventory     json.RawMessage
		Compatibility json.RawMessage
	}
	if err = json.Unmarshal(got.Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]struct {
		raw  json.RawMessage
		want string
	}{"inventory": {evidence.Inventory, value.Cell.RuntimeInventoryDigest}, "compatibility": {evidence.Compatibility, value.Cell.CompatibilityDigest}} {
		digest, err := executioncell.DigestContractValue(pair.raw)
		if err != nil {
			t.Fatal(err)
		}
		if digest != pair.want {
			t.Fatalf("%s digest is not actual evidence", name)
		}
	}
	payloadDigest, err := executioncell.DigestOperationalPayload(got.OperationalPayload)
	if err != nil {
		t.Fatal(err)
	}
	if payloadDigest != value.OperationalPayloadDigest {
		t.Fatal("payload digest mismatch")
	}
	if request.Work.SessionID != "" || request.Work.ResolvedProfile.Harness != "" {
		t.Fatal("producer mutated caller work")
	}
	// The evidence must be checked, not merely parseable.
	changed := value
	changed.IntentDigest = strings.Repeat("0", 64)
	raw, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	if executioncell.AssertAdmissionProvenance(got.Intent, immutable) == nil {
		t.Fatal("changed intent passed provenance")
	}
}

func TestLocalAdmissionRequiresHostAuthObservation(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	unavailable := errors.New("host login unavailable")
	config.CheckHostAuth = func(context.Context) error { return unavailable }
	producer, err := NewLocalAdmissionProducer(registry, config)
	if err != nil {
		t.Fatal(err)
	}
	got, err := producer.Admit(context.Background(), request)
	if got != nil || !errors.Is(err, unavailable) {
		t.Fatalf("missing auth observation admitted: got=%v err=%v", got != nil, err)
	}
}

func TestLocalAdmissionRequiresExplicitOperatorCatalog(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	config.ModelCatalogEntry = nil
	producer, err := NewLocalAdmissionProducer(registry, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := producer.Admit(context.Background(), request); err == nil || got != nil {
		t.Fatal("missing built-in model-host pair silently admitted")
	}
}

func TestLocalAdmissionUsesBuiltInNativeModelHostPairs(t *testing.T) {
	tests := []struct {
		name, harness, model, author string
	}{
		{name: "Codex GPT-6 Sol", harness: string(agent.HarnessCodex), model: "gpt-6-sol", author: "openai"},
		{name: "Claude Code Sonnet 5", harness: string(agent.HarnessClaudeCode), model: "claude-sonnet-5", author: "anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "native-cli")
			if err := os.Symlink("/bin/sh", binary); err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: RuntimeTransportLocal})
			if err != nil {
				t.Fatal(err)
			}
			switch tt.harness {
			case string(agent.HarnessCodex):
				provider, err := codex.New(codex.Options{CodexBin: binary})
				if err != nil {
					t.Fatal(err)
				}
				if err := registry.Register(provider); err != nil {
					t.Fatal(err)
				}
			case string(agent.HarnessClaudeCode):
				provider, err := claude.New(claude.Options{Binary: binary})
				if err != nil {
					t.Fatal(err)
				}
				if err := registry.Register(provider); err != nil {
					t.Fatal(err)
				}
			}
			config := LocalAdmissionConfig{
				ScopeID: "local-org", WorkerID: "local-worker", PlacementID: "local-host",
				ConfigurationRevision: "config-1", Harness: agent.HarnessName(tt.harness),
				Model:      executioncell.ModelRef{ID: tt.model, Author: tt.author},
				EndpointID: "local-" + tt.author, AuthBindingID: "local-login", BinaryPath: binary,
				CheckHostAuth: func(context.Context) error { return nil },
			}
			if config.ModelCatalogEntry != nil {
				t.Fatal("fixture unexpectedly supplied an operator catalog")
			}
			request := LocalAdmissionRequest{SessionID: "local-session", ReceiptID: "local-admission", PreflightChallengeID: "local-challenge"}
			request.Work.Body, request.Work.WorkType = "Implement the authored issue.", "development"
			admitted := admitLocalFixture(t, registry, config, request)
			receipt, err := executioncell.DecodeAdmissionReceipt(admitted.Receipt)
			if err != nil {
				t.Fatal(err)
			}
			if got := receipt.Value().Cell.Model; got != config.Model {
				t.Fatalf("admitted model = %+v, want %+v", got, config.Model)
			}
			var evidence struct {
				Configuration struct {
					Catalog struct{ Kind string }
				}
			}
			if err := json.Unmarshal(admitted.Evidence, &evidence); err != nil {
				t.Fatal(err)
			}
			if evidence.Configuration.Catalog.Kind != "builtin" {
				t.Fatalf("catalog evidence kind = %q, want builtin", evidence.Configuration.Catalog.Kind)
			}
		})
	}
}

func TestLocalAdmissionUnknownBuiltInHostPairRefused(t *testing.T) {
	registry, config, request := localAdmissionFixture(t)
	config.Model = executioncell.ModelRef{ID: "unknown-native-model", Author: "openai"}
	config.ModelCatalogEntry = nil
	producer, err := NewLocalAdmissionProducer(registry, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := producer.Admit(context.Background(), request); err == nil || got != nil {
		t.Fatal("unknown built-in model-host pair admitted")
	}
}

func TestLocalAdmissionInventoryAndConfigurationAreBound(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	first := admitLocalFixture(t, registry, config, request)
	firstReceipt, err := executioncell.DecodeAdmissionReceipt(first.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(config.BinaryPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/bin/cat", config.BinaryPath); err != nil {
		t.Fatal(err)
	}
	second := admitLocalFixture(t, registry, config, request)
	secondReceipt, err := executioncell.DecodeAdmissionReceipt(second.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if firstReceipt.Value().Cell.RuntimeInventoryDigest == secondReceipt.Value().Cell.RuntimeInventoryDigest {
		t.Fatal("changed executable bytes did not change runtime inventory")
	}
	config.ConfigurationRevision = "config-2"
	third := admitLocalFixture(t, registry, config, request)
	thirdReceipt, err := executioncell.DecodeAdmissionReceipt(third.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.Value().Cell.Endpoint.Revision == thirdReceipt.Value().Cell.Endpoint.Revision || secondReceipt.Value().IntentDigest == thirdReceipt.Value().IntentDigest {
		t.Fatal("configuration revision not bound")
	}
}

func TestLocalAdmissionRejectsUnownedExecutionInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*LocalAdmissionRequest)
	}{
		{"empty-context", func(r *LocalAdmissionRequest) { r.Work.Body = "" }},
		{"interactive", func(r *LocalAdmissionRequest) { r.Work.Mode = "interactive" }},
		{"unknown-mode", func(r *LocalAdmissionRequest) { r.Work.Mode = "unknown" }},
		{"lease", func(r *LocalAdmissionRequest) { r.Work.TerminalWorkareaLease = &workarea.TerminalLeaseRequest{} }},
		{"other-work", func(r *LocalAdmissionRequest) { r.Work.WorkType = "acceptance" }},
		{"capability", func(r *LocalAdmissionRequest) { r.Work.Capabilities = map[string]bool{"remote": true} }},
		{"profile", func(r *LocalAdmissionRequest) { r.Work.ResolvedProfile.Harness = "codex" }},
		{"receipt", func(r *LocalAdmissionRequest) { r.Work.AdmissionReceipt = json.RawMessage(`{}`) }},
		{"pool-claim", func(r *LocalAdmissionRequest) { r.Work.ClaimReceipt = json.RawMessage(`{}`) }},
		{"preencoded-payload", func(r *LocalAdmissionRequest) { r.Work.OperationalPayload = json.RawMessage(`{}`) }},
		{"authority", func(r *LocalAdmissionRequest) { r.Work.OrganizationID = "other" }},
		{"endpoint", func(r *LocalAdmissionRequest) { r.Work.PlatformURL = "https://example.com" }},
		{"credential", func(r *LocalAdmissionRequest) { r.Work.AuthToken = "not-a-real-token" }},
		{"env", func(r *LocalAdmissionRequest) { r.Work.Env = map[string]string{"EXAMPLE": "value"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			registry, config, request := localAdmissionFixture(t)
			tc.change(&request)
			producer, err := NewLocalAdmissionProducer(registry, config)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := producer.Admit(context.Background(), request); err == nil || got != nil {
				t.Fatal("unowned execution input admitted")
			}
		})
	}
}

func TestLocalAdmissionRejectsMissingRuntimeAndUnconfiguredProducer(t *testing.T) {
	t.Parallel()
	registry, config, request := localAdmissionFixture(t)
	if err := os.Remove(config.BinaryPath); err != nil {
		t.Fatal(err)
	}
	observed := false
	config.CheckHostAuth = func(context.Context) error { observed = true; return nil }
	producer, err := NewLocalAdmissionProducer(registry, config)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := producer.Admit(context.Background(), request); err == nil || got != nil {
		t.Fatal("missing executable admitted")
	}
	if observed {
		t.Fatal("credential observation preceded binary availability")
	}
	var empty LocalAdmissionProducer
	if got, err := empty.Admit(context.Background(), request); err == nil || got != nil {
		t.Fatal("unconfigured producer admitted")
	}
}
