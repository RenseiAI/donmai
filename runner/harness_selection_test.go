package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

type selectorFakeProvider struct {
	name       agent.ProviderName
	harness    agent.HarnessName
	spawnCalls atomic.Int32
}

func (p *selectorFakeProvider) Name() agent.ProviderName { return p.name }
func (p *selectorFakeProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (p *selectorFakeProvider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name: p.harness, HumanLabel: string(p.harness), Family: agent.FamilyHarness,
		ContractABI: "harness/v2",
	}
}

func (p *selectorFakeProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	p.spawnCalls.Add(1)
	return nil, errors.New("selector fake must not spawn")
}

func (p *selectorFakeProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, errors.New("selector fake must not resume")
}
func (p *selectorFakeProvider) Shutdown(context.Context) error { return nil }

func selectorRegistry(t *testing.T, providers ...*selectorFakeProvider) *Registry {
	t.Helper()
	registry := NewRegistry()
	for _, provider := range providers {
		if err := registry.Register(provider); err != nil {
			t.Fatalf("register %q: %v", provider.name, err)
		}
	}
	return registry
}

func TestExplicitHarnessSelectionWireAndCanonicalMatrix(t *testing.T) {
	t.Parallel()
	providers := []*selectorFakeProvider{
		{name: agent.ProviderClaude, harness: agent.HarnessClaudeCode},
		{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		{name: agent.ProviderAmp, harness: agent.HarnessAmp},
		{name: agent.ProviderAGYCLI, harness: agent.HarnessAntigravity},
		{name: agent.ProviderOpenCode, harness: agent.HarnessOpenCode},
		{name: agent.ProviderGemini, harness: agent.HarnessRaw},
		{name: agent.ProviderOllama, harness: agent.HarnessRaw},
	}
	registry := selectorRegistry(t, providers...)
	tests := []struct {
		name, harness string
		provider      agent.ProviderName
		wantProvider  agent.ProviderName
		wantHarness   string
	}{
		{name: "legacy claude wire", harness: "claude", provider: agent.ProviderClaude, wantProvider: agent.ProviderClaude, wantHarness: "claude-code"},
		{name: "legacy codex wire", harness: "codex", provider: agent.ProviderCodex, wantProvider: agent.ProviderCodex, wantHarness: "codex"},
		{name: "legacy amp wire", harness: "amp", provider: agent.ProviderAmp, wantProvider: agent.ProviderAmp, wantHarness: "amp"},
		{name: "legacy agy wire", harness: "agy", provider: agent.ProviderGemini, wantProvider: agent.ProviderAGYCLI, wantHarness: "antigravity"},
		{name: "legacy opencode wire", harness: "opencode", provider: agent.ProviderOpenCode, wantProvider: agent.ProviderOpenCode, wantHarness: "opencode"},
		{name: "legacy native gemini wire", harness: "native", provider: agent.ProviderGemini, wantProvider: agent.ProviderGemini, wantHarness: "raw"},
		{name: "legacy native ollama wire", harness: "native", provider: agent.ProviderOllama, wantProvider: agent.ProviderOllama, wantHarness: "raw"},
		{name: "canonical raw disambiguated to gemini", harness: "raw", provider: agent.ProviderGemini, wantProvider: agent.ProviderGemini, wantHarness: "raw"},
		{name: "canonical raw disambiguated to ollama", harness: "raw", provider: agent.ProviderOllama, wantProvider: agent.ProviderOllama, wantHarness: "raw"},
		{name: "canonical claude manifest id", harness: "claude-code", provider: agent.ProviderGemini, wantProvider: agent.ProviderClaude, wantHarness: "claude-code"},
		{name: "canonical antigravity manifest id", harness: "antigravity", provider: agent.ProviderGemini, wantProvider: agent.ProviderAGYCLI, wantHarness: "antigravity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			selection, err := registry.selectExplicitHarness(ResolvedProfile{Harness: tt.harness, Provider: tt.provider})
			if err != nil {
				t.Fatalf("selectExplicitHarness: %v", err)
			}
			if selection.Provider.Name() != tt.wantProvider || selection.Harness.ID != tt.wantHarness {
				t.Fatalf("selection = provider %q harness %q, want provider %q harness %q", selection.Provider.Name(), selection.Harness.ID, tt.wantProvider, tt.wantHarness)
			}
			if !selection.Explicit || len(selection.Decisions) != 1 || selection.Decisions[0].Kind != executioncell.DecisionExplicit {
				t.Fatalf("explicit resolver evidence = %+v", selection)
			}
		})
	}
}

func TestExplicitHarnessSelectionTypedDenials(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t,
		&selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		&selectorFakeProvider{name: agent.ProviderGemini, harness: agent.HarnessRaw},
		&selectorFakeProvider{name: agent.ProviderOllama, harness: agent.HarnessRaw},
	)
	tests := []struct {
		name, harness string
		provider      agent.ProviderName
		wantCode      executioncell.AdmissionDenialCode
	}{
		{name: "syntactically unknown", harness: "future-harness", provider: agent.ProviderCodex, wantCode: executioncell.DenialUnknownHarness},
		{name: "invalid whitespace", harness: " codex", provider: agent.ProviderCodex, wantCode: executioncell.DenialUnknownHarness},
		{name: "legacy ollama token is not a harness", harness: "ollama", provider: agent.ProviderOllama, wantCode: executioncell.DenialUnknownHarness},
		{name: "known canonical but unavailable", harness: "claude-code", provider: agent.ProviderClaude, wantCode: executioncell.DenialHarnessUnavailable},
		{name: "known raw remains ambiguous without provider", harness: "raw", wantCode: executioncell.DenialHarnessUnavailable},
		{name: "known native missing provider pairing", harness: "native", wantCode: executioncell.DenialHarnessUnavailable},
		{name: "known native has unsupported provider pairing", harness: "native", provider: agent.ProviderAmp, wantCode: executioncell.DenialHarnessUnavailable},
		{name: "native must not conflate claude API and CLI", harness: "native", provider: agent.ProviderClaude, wantCode: executioncell.DenialHarnessUnavailable},
		{name: "native must not conflate codex API and CLI", harness: "native", provider: agent.ProviderCodex, wantCode: executioncell.DenialHarnessUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.selectExplicitHarness(ResolvedProfile{Harness: tt.harness, Provider: tt.provider})
			var denial *HarnessAdmissionError
			if !errors.As(err, &denial) || denial.Code != tt.wantCode {
				t.Fatalf("error = %v, want HarnessAdmissionError code %q", err, tt.wantCode)
			}
		})
	}
}

func TestLegacyHarnessSelectionAdapterReceiptsEveryFallback(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t,
		&selectorFakeProvider{name: agent.ProviderClaude, harness: agent.HarnessClaudeCode},
		&selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
	)
	tests := []struct {
		name      string
		profile   ResolvedProfile
		posterior agent.ProviderName
		want      agent.ProviderName
		kind      executioncell.ResolverDecisionKind
		source    string
	}{
		{name: "provider inference", profile: ResolvedProfile{Provider: agent.ProviderCodex}, want: agent.ProviderCodex, kind: executioncell.DecisionLegacyInference, source: "legacy-provider:codex"},
		{name: "runner inference", profile: ResolvedProfile{Runner: "codex"}, want: agent.ProviderCodex, kind: executioncell.DecisionLegacyInference, source: "legacy-runner:codex"},
		{name: "claude default", profile: ResolvedProfile{}, want: agent.ProviderClaude, kind: executioncell.DecisionDefault},
		{name: "legacy posterior", profile: ResolvedProfile{Provider: agent.ProviderClaude}, posterior: agent.ProviderCodex, want: agent.ProviderCodex, kind: executioncell.DecisionLegacyInference, source: "legacy-posterior:codex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			selection, err := registry.legacyHarnessSelectionAdapter(tt.profile, tt.posterior)
			if err != nil {
				t.Fatal(err)
			}
			if selection.Provider.Name() != tt.want || len(selection.Decisions) != 1 {
				t.Fatalf("selection = %+v, want provider %q and one decision", selection, tt.want)
			}
			decision := selection.Decisions[0]
			if decision.Kind != tt.kind || decision.SourceRef != tt.source {
				t.Fatalf("decision = %+v, want kind %q source %q", decision, tt.kind, tt.source)
			}
		})
	}
}

func TestPreflightHarnessAdmissionIsConsumedWithoutReresolution(t *testing.T) {
	t.Parallel()
	first := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	registry := selectorRegistry(t, first)
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{
		Harness: "codex", Provider: agent.ProviderCodex, Model: "before-gateway",
	}}
	admission, err := registry.PreflightHarness(qw)
	if err != nil {
		t.Fatalf("PreflightHarness: %v", err)
	}
	ref, ok := admission.CanonicalHarnessRef()
	if !ok || ref.ID != string(agent.HarnessCodex) {
		t.Fatalf("CanonicalHarnessRef = %+v, %v; want codex", ref, ok)
	}
	ref.ID = "mutated-copy"
	if again, ok := admission.CanonicalHarnessRef(); !ok || again.ID != string(agent.HarnessCodex) {
		t.Fatalf("CanonicalHarnessRef after copy mutation = %+v, %v; want codex", again, ok)
	}

	// Replacing the registry entry after preflight proves the admitted concrete
	// implementation is carried, not looked up again. Endpoint/model mutations
	// are intentionally outside the selector fingerprint and remain permitted.
	replacement := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	if err := registry.Register(replacement); err != nil {
		t.Fatal(err)
	}
	qw.ResolvedProfile.Model = "after-gateway"
	r := &Runner{registry: registry}
	selection, err := r.admittedHarnessSelection(context.Background(), qw, admission)
	if err != nil {
		t.Fatalf("admittedHarnessSelection: %v", err)
	}
	if selection.Provider != first {
		t.Fatalf("admitted provider = %p, want original %p (replacement %p)", selection.Provider, first, replacement)
	}
	if _, err := r.admittedHarnessSelection(context.Background(), qw, admission); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("second admission consumption error = %v, want already consumed", err)
	}

	qw.ResolvedProfile.Provider = agent.ProviderClaude
	if _, err := r.admittedHarnessSelection(context.Background(), qw, admission); err == nil {
		t.Fatal("selector mutation after preflight must fail closed")
	}
}

func TestPreflightHarnessDenialRejectsCrossRequestAndPayloadReuse(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t, &selectorFakeProvider{
		name: agent.ProviderCodex, harness: agent.HarnessCodex,
	})
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{
		Harness: "future-harness", Provider: agent.ProviderCodex,
	}}
	qw.SessionID = "session_denial_origin"
	qw.Body = "original payload"
	admission, err := registry.PreflightHarness(qw)
	if admission == nil || err == nil {
		t.Fatalf("PreflightHarness = admission %+v, error %v; want denied token", admission, err)
	}
	r := &Runner{registry: registry}

	otherRequest := qw
	otherRequest.SessionID = "session_denial_other"
	if _, err := r.admittedHarnessSelection(context.Background(), otherRequest, admission); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("cross-request reuse error = %v, want different request", err)
	}
	changedPayload := qw
	changedPayload.Body = "changed payload"
	if _, err := r.admittedHarnessSelection(context.Background(), changedPayload, admission); err == nil || !strings.Contains(err.Error(), "payload changed") {
		t.Fatalf("changed-payload reuse error = %v, want payload changed", err)
	}
	var denial *HarnessAdmissionError
	if _, err := r.admittedHarnessSelection(context.Background(), qw, admission); !errors.As(err, &denial) {
		t.Fatalf("originating request error = %v, want cached typed denial", err)
	}
	if _, err := r.admittedHarnessSelection(context.Background(), qw, admission); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("second denial consumption error = %v, want already consumed", err)
	}
}

func TestPreflightHarnessDenialCarriesCanonicalReceipt(t *testing.T) {
	t.Parallel()
	registry := selectorRegistry(t, &selectorFakeProvider{
		name: agent.ProviderCodex, harness: agent.HarnessCodex,
	})
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{
		Harness: "future-harness", Provider: agent.ProviderCodex,
	}}
	qw.SessionID = "session_preflight_denial"
	admission, err := registry.PreflightHarness(qw)
	var denial *HarnessAdmissionError
	if admission == nil || !errors.As(err, &denial) {
		t.Fatalf("PreflightHarness = admission %+v, error %v; want reusable typed denial", admission, err)
	}
	if got := denial.Receipt.Value(); got.Decision != executioncell.AdmissionDenied || got.DenialCode != executioncell.DenialUnknownHarness {
		t.Fatalf("canonical denial receipt = %+v", got)
	}
}

func TestExplicitHarnessDenialPrecedesAllSideEffects(t *testing.T) {
	t.Setenv("ROUTING_SELECTOR_ENABLED", "true")
	var posteriorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		posteriorCalls.Add(1)
	}))
	defer server.Close()

	tests := []struct {
		name      string
		profile   ResolvedProfile
		providers []*selectorFakeProvider
		wantCode  executioncell.AdmissionDenialCode
	}{
		{
			name: "unknown selector", profile: ResolvedProfile{Harness: "future-harness", Provider: agent.ProviderCodex},
			providers: []*selectorFakeProvider{{name: agent.ProviderCodex, harness: agent.HarnessCodex}}, wantCode: executioncell.DenialUnknownHarness,
		},
		{
			name: "invalid selector", profile: ResolvedProfile{Harness: " codex", Provider: agent.ProviderCodex},
			providers: []*selectorFakeProvider{{name: agent.ProviderCodex, harness: agent.HarnessCodex}}, wantCode: executioncell.DenialUnknownHarness,
		},
		{
			name: "known unregistered selector", profile: ResolvedProfile{Harness: "claude-code", Provider: agent.ProviderClaude},
			providers: []*selectorFakeProvider{{name: agent.ProviderCodex, harness: agent.HarnessCodex}}, wantCode: executioncell.DenialHarnessUnavailable,
		},
		{
			name: "native missing provider", profile: ResolvedProfile{Harness: "native"},
			providers: []*selectorFakeProvider{{name: agent.ProviderGemini, harness: agent.HarnessRaw}}, wantCode: executioncell.DenialHarnessUnavailable,
		},
		{
			name: "native claude is not claude code", profile: ResolvedProfile{Harness: "native", Provider: agent.ProviderClaude},
			providers: []*selectorFakeProvider{{name: agent.ProviderClaude, harness: agent.HarnessClaudeCode}}, wantCode: executioncell.DenialHarnessUnavailable,
		},
		{
			name: "ollama literal is unknown", profile: ResolvedProfile{Harness: "ollama", Provider: agent.ProviderOllama},
			providers: []*selectorFakeProvider{{name: agent.ProviderOllama, harness: agent.HarnessRaw}}, wantCode: executioncell.DenialUnknownHarness,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforePosterior := posteriorCalls.Load()
			runner := &Runner{
				registry:   selectorRegistry(t, tt.providers...),
				httpClient: server.Client(),
				logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				now:        func() time.Time { return time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC) },
				// wt, promptBuilder, and every other collaborator intentionally nil:
				// reaching any post-admission side effect would panic this test.
			}
			qw := QueuedWork{}
			qw.SessionID = "session_harness_denial"
			qw.WorkType = "development"
			qw.PlatformURL = server.URL
			qw.AuthToken = "test-token"
			qw.ResolvedProfile = tt.profile
			result, err := runner.runLoop(context.Background(), qw, runner.now().UnixMilli(), nil)
			var denial *HarnessAdmissionError
			if !errors.As(err, &denial) || denial.Code != tt.wantCode {
				t.Fatalf("error = %v, want HarnessAdmissionError code %q", err, tt.wantCode)
			}
			if got := posteriorCalls.Load(); got != beforePosterior {
				t.Fatalf("posterior calls changed from %d to %d, want zero side effects", beforePosterior, got)
			}
			for _, provider := range tt.providers {
				if provider.spawnCalls.Load() != 0 {
					t.Fatalf("provider %q Spawn calls = %d, want 0", provider.name, provider.spawnCalls.Load())
				}
			}
			if result.AdmissionReceipt == nil || result.AdmissionReceipt.Decision != executioncell.AdmissionDenied || result.AdmissionReceipt.DenialCode != tt.wantCode {
				t.Fatalf("canonical denial receipt = %+v (runLoop error: %v)", result.AdmissionReceipt, err)
			}
			if denial.Receipt.Value().ReceiptID != result.AdmissionReceipt.ReceiptID {
				t.Fatalf("typed error receipt %q != result receipt %q", denial.Receipt.Value().ReceiptID, result.AdmissionReceipt.ReceiptID)
			}
		})
	}
}
