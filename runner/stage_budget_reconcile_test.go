package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
)

func TestReconcileStageBudgetPreservesOperationalAuthority(t *testing.T) {
	budget := prompt.StageBudget{MaxDurationSeconds: 1800, MaxSubAgents: 3, MaxTokens: 24_000}
	different := budget
	different.MaxTokens++
	tests := []struct {
		name               string
		hasOperational     bool
		admitted           *prompt.StageBudget
		sibling            *prompt.StageBudget
		siblingPresent     bool
		wantErr            bool
		wantSiblingAdopted bool
	}{
		{name: "legacy adopts sibling", sibling: &budget, siblingPresent: true, wantSiblingAdopted: true},
		{name: "matching receipt mirror preserves admitted value", hasOperational: true, admitted: &budget, sibling: &budget, siblingPresent: true},
		{name: "receipt budget missing from sibling", hasOperational: true, admitted: &budget, wantErr: true},
		{name: "sibling budget absent from receipt", hasOperational: true, sibling: &budget, siblingPresent: true, wantErr: true},
		{name: "sibling budget differs from receipt", hasOperational: true, admitted: &budget, sibling: &different, siblingPresent: true, wantErr: true},
		{name: "both receipt and sibling omit budget", hasOperational: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qw := QueuedWork{}
			if tt.hasOperational {
				qw.OperationalPayload = json.RawMessage(`{}`)
			}
			if tt.admitted != nil {
				admittedBudget := *tt.admitted
				qw.StageBudget = &admittedBudget
			}
			admittedPointer := qw.StageBudget
			var siblingJSON json.RawMessage
			if tt.siblingPresent {
				var err error
				siblingJSON, err = json.Marshal(tt.sibling)
				if err != nil {
					t.Fatal(err)
				}
			}

			got, err := ReconcileStageBudget(qw, siblingJSON)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "stage budget compatibility mirror differs from operational payload") {
					t.Fatalf("ReconcileStageBudget error = %v, want compatibility-mirror refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReconcileStageBudget: %v", err)
			}
			if tt.wantSiblingAdopted {
				if got.StageBudget == nil || *got.StageBudget != budget {
					t.Fatalf("legacy StageBudget = %+v, want %+v", got.StageBudget, budget)
				}
				return
			}
			if got.StageBudget != admittedPointer {
				t.Fatalf("receipt-bearing reconciliation replaced admitted pointer: got=%p want=%p", got.StageBudget, admittedPointer)
			}
		})
	}
}

func TestPreflightAndSpawnAgreeForAutonomousStageDispatchWithMatchingSiblingBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget prompt.StageBudget
	}{
		{name: "duration limit only", budget: prompt.StageBudget{MaxDurationSeconds: 1800}},
		{name: "subagent limit only", budget: prompt.StageBudget{MaxSubAgents: 3}},
		{name: "token limit only", budget: prompt.StageBudget{MaxTokens: 8_000}},
		{
			name: "all limits",
			budget: prompt.StageBudget{
				MaxDurationSeconds: 1800,
				MaxSubAgents:       3,
				MaxTokens:          24_000,
			},
		},
		{name: "explicit zero budget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

			const model = "gpt-stage-budget-model"
			baseQW := QueuedWork{}
			baseQW.SessionID = "host-preflight-autonomous-stage-budget"
			baseQW.StageID = "development"
			baseQW.StagePrompt = "Implement the accepted stage and report the result."
			budgetCopy := tt.budget
			baseQW.StageBudget = &budgetCopy
			var err error
			baseQW, err = ReconcileResolvedProfile(baseQW, nil, resolvedProfileFixtureJSON(t, model))
			if err != nil {
				t.Fatalf("ReconcileResolvedProfile: %v", err)
			}
			baseQW = attachAdmittedExecutionCell(t, baseQW, piReceiptCell("harness/v2", model, executioncell.SessionAutonomous, nil))
			operational, err := CanonicalOperationalPayload(baseQW)
			if err != nil {
				t.Fatal(err)
			}
			baseQW.OperationalPayload = operational

			detail := func(sibling *prompt.StageBudget, include bool) map[string]any {
				value := map[string]any{
					"sessionId": baseQW.SessionID, "workerId": baseQW.WorkerID,
					"admissionReceipt": baseQW.AdmissionReceipt, "effectiveCell": baseQW.EffectiveCell,
					"executionRuntimeBinding": baseQW.ExecutionRuntimeBinding,
					"operationalPayload":      baseQW.OperationalPayload,
				}
				if include {
					value["stageBudget"] = sibling
				}
				return value
			}

			compileHostPlan := func(t *testing.T, input map[string]any) agent.PreparedHarness {
				t.Helper()
				receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, input))
				if err != nil {
					t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
				}
				host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
				if err != nil {
					t.Fatal(err)
				}
				var plan agent.PreparedHarness
				if err := json.Unmarshal(host.Plan, &plan); err != nil {
					t.Fatal(err)
				}
				return plan
			}

			t.Run("matching receipt mirror", func(t *testing.T) {
				plan := compileHostPlan(t, detail(&tt.budget, true))
				var spawnQW QueuedWork
				if err := json.Unmarshal(baseQW.OperationalPayload, &spawnQW); err != nil {
					t.Fatalf("decode admitted operational payload: %v", err)
				}
				spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
				spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
				spawnQW.WorkerID = baseQW.WorkerID
				siblingJSON, err := json.Marshal(tt.budget)
				if err != nil {
					t.Fatal(err)
				}
				spawnQW, err = ReconcileStageBudget(spawnQW, siblingJSON)
				if err != nil {
					t.Fatalf("ReconcileStageBudget: %v", err)
				}
				source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
					Provider: providerWithManifest,
					receipt:  mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
					effectiveCell: piReceiptCell(
						"harness/v2", model, executioncell.SessionAutonomous, nil,
					),
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(source.Prompt, "<stageBudget") {
					t.Fatalf("spawn prompt did not render the admitted stage budget: %q", source.Prompt)
				}
				source.PreparedHarness = &plan
				if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
					t.Fatalf("ApplyPreparedHarness rejected matching admitted budget: %v", err)
				}
			})

			t.Run("missing receipt mirror denies preflight", func(t *testing.T) {
				receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail(nil, false)))
				if err == nil || !strings.Contains(err.Error(), "stage budget compatibility mirror differs from operational payload") {
					t.Fatalf("PreflightExecution receipt=%s error=%v, want missing-mirror refusal", receipt, err)
				}
			})

			t.Run("different receipt mirror denies preflight", func(t *testing.T) {
				different := tt.budget
				different.MaxTokens++
				receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail(&different, true)))
				if err == nil || !strings.Contains(err.Error(), "stage budget compatibility mirror differs from operational payload") {
					t.Fatalf("PreflightExecution receipt=%s error=%v, want value-mismatch refusal", receipt, err)
				}
			})

			if provider.spawnCalls.Load() != 0 {
				t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
			}
		})
	}
}
