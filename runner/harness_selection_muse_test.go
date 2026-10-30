package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/provider/harness/muse"
)

func TestRecognizedHarnessToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		token string
		want  agent.HarnessName
		known bool
	}{
		{"muse", agent.HarnessMuse, true},
		{"claude-code", agent.HarnessClaudeCode, true},
		{"claude", agent.HarnessClaudeCode, true},
		{"codex", agent.HarnessCodex, true},
		{"opencode", agent.HarnessOpenCode, true},
		{"gemini-direct", agent.HarnessGeminiDirect, true},
		{"ollama", agent.HarnessOllama, true},
		{"raw", legacyRawHarness, true},
		{"native", legacyNativeHarness, true},
		{"stub", agent.HarnessStub, true},
		{"pi", agent.HarnessPi, true},
		{"shell", agent.HarnessShell, true},
		{"future-harness", "", false},
		{"Muse", "", false},
		{" muse", "", false},
		{"muse ", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			t.Parallel()
			if got, known := recognizedHarnessToken(tt.token); got != tt.want || known != tt.known {
				t.Fatalf("recognizedHarnessToken(%q) = %q, %t; want %q, %t", tt.token, got, known, tt.want, tt.known)
			}
		})
	}
}

func TestRunMuseHarnessAdmission(t *testing.T) {
	for _, receipted := range []bool{false, true} {
		name := "explicit"
		if receipted {
			name = "receipt-pinned"
		}
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t)
			fake := &selectorFakeProvider{name: agent.ProviderMuse, harness: agent.HarnessMuse}
			// Keep the shipped manifest and capabilities; stop at Spawn without
			// requiring a local binary or subscriber credentials.
			provider := &manifestSelectorProvider{selectorFakeProvider: fake, manifest: (&muse.Provider{}).Manifest(), capabilities: (&muse.Provider{}).Capabilities()}
			if err := h.runner.registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			qw := h.queuedWork("muse-admission")
			qw.Repository = ""
			qw.ResolvedProfile = exactReceiptQueuedWork("unused").ResolvedProfile
			qw.ResolvedProfile.Harness = "muse"
			qw.ResolvedProfile.Provider = agent.ProviderCodex // explicit selector remains authoritative
			qw.ResolvedProfile.Endpoint.Protocol = agent.ProtoMuseMSP
			qw.ResolvedProfile.Endpoint.Host = agent.HostOAuthCLI
			if receipted {
				cell := exactReceiptCell(provider.Manifest().ContractABI, qw.ResolvedProfile.Model, executioncell.SessionAutonomous, nil)
				cell.Harness.ID = "muse"
				cell.Endpoint.Protocol = string(agent.ProtoMuseMSP)
				qw = attachAdmittedExecutionCell(t, qw, cell)
				admission, err := h.runner.registry.PreflightHarness(qw)
				if err != nil {
					t.Fatalf("PreflightHarness: %v", err)
				}
				if ref, ok := admission.CanonicalHarnessRef(); !ok || ref != cell.Harness {
					t.Fatalf("canonical harness = %+v, %t; want %+v", ref, ok, cell.Harness)
				}
				operational, err := CanonicalOperationalPayload(qw)
				if err != nil {
					t.Fatal(err)
				}
				qw.OperationalPayload = operational
				qw.HostAdaptationReceipt, err = NewProviderView(h.runner.registry).PreflightExecution(rawJSONForRunner(t, map[string]any{
					"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
					"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload,
				}))
				if err != nil {
					t.Fatalf("PreflightExecution: %v", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if fake.spawnCalls.Load() != 1 || err == nil || !strings.Contains(err.Error(), "selector fake must not spawn") {
				t.Fatalf("Run did not reach selected Muse Spawn: calls=%d error=%v", fake.spawnCalls.Load(), err)
			}
			if res.ProviderName != agent.ProviderMuse {
				t.Fatalf("provider = %q, want muse", res.ProviderName)
			}
		})
	}
}

func TestMuseHarnessReceiptVersionStillPinned(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if err := registry.Register(&muse.Provider{}); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("muse-version")
	qw.ResolvedProfile.Harness = "muse"
	cell := exactReceiptCell("harness/future", qw.ResolvedProfile.Model, executioncell.SessionAutonomous, nil)
	cell.Harness.ID = "muse"
	qw = attachAdmittedExecutionCell(t, qw, cell)
	_, err := registry.PreflightHarness(qw)
	var denial *HarnessAdmissionError
	if !errors.As(err, &denial) || denial.Code != executioncell.DenialUnsupportedHarnessVersion {
		t.Fatalf("PreflightHarness = %v; want unsupported harness version", err)
	}
}
