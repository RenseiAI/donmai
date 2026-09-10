package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func readyHostReceipt(t *testing.T, binding RuntimeBinding) []byte {
	t.Helper()
	operationalDigest := strings.Repeat("a", 64)
	prompt := agent.PromptDeliveryReceipt{ContractVersion: agent.PromptContractVersion, ProfileID: "test-prompt", Decision: "ready", Entries: []agent.PromptDeliveryEntry{}}
	claimReceiptID := ""
	if binding.ClaimID != "" {
		claimReceiptID = "claim-receipt"
	}
	tool := agent.ToolLifecycleReceipt{ContractVersion: agent.ToolLifecycleContractVersion, AdmissionReceiptID: "admission", ClaimReceiptID: claimReceiptID, OperationalPayloadDigest: operationalDigest, ProfileID: "test-tools", Decision: "ready", EvidenceTier: string(agent.EvidenceStructured), ProductionEligible: true, Entries: []agent.ToolLifecycleEntry{}}
	channels := []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"}
	materializations := make([]agent.HarnessMaterialization, 0, len(channels))
	for _, channel := range channels {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	plan := mustJSON(t, agent.PreparedHarness{ContractVersion: agent.HarnessAdaptationContractVersion, Harness: "stub", Mode: agent.PromptModeAutonomous, OperationalPayloadDigest: operationalDigest, AuthorityDigest: strings.Repeat("f", 64), RuntimeMCPNames: []string{}, Materializations: materializations, PromptReceipt: prompt, ToolLifecycleReceipt: tool})
	digest := sha256.Sum256(plan)
	raw, err := json.Marshal(HostAdaptationReceipt{
		ContractVersion: HostAdaptationContractVersion,
		RequestID:       binding.RequestID, WorkerID: binding.WorkerID,
		PlacementID: binding.PlacementID, ClaimID: binding.ClaimID,
		Decision: "ready", Plan: plan, PlanDigest: hex.EncodeToString(digest[:]),
		PromptReceipt:        mustJSON(t, prompt),
		ToolLifecycleReceipt: mustJSON(t, tool),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func runtimeBindingV2() RuntimeBinding {
	return RuntimeBinding{
		ContractVersion: RuntimeBindingV2ContractVersion,
		RequestID:       "request-1", WorkerID: "worker-1", PlacementID: "host-1", ClaimID: "claim-1",
		PreflightRegistration: &PreflightRegistrationRef{
			ContractVersion: PreflightRegistrationContractVersion,
			Required:        true, ChallengeID: "challenge-1",
		},
	}
}

func TestDecodeRuntimeBindingVersions(t *testing.T) {
	v1 := RuntimeBinding{ContractVersion: RuntimeBindingContractVersion, RequestID: "r", WorkerID: "w", PlacementID: "p"}
	if _, err := DecodeRuntimeBinding(mustJSON(t, v1)); err != nil {
		t.Fatalf("v1 changed: %v", err)
	}
	for name, raw := range map[string][]byte{
		"v1 with registration":      mustJSON(t, RuntimeBinding{ContractVersion: RuntimeBindingContractVersion, RequestID: "r", WorkerID: "w", PlacementID: "p", PreflightRegistration: runtimeBindingV2().PreflightRegistration}),
		"v1 with null registration": []byte(`{"contractVersion":"execution-runtime-binding/v1","requestId":"r","workerId":"w","placementId":"p","preflightRegistration":null}`),
		"v1 with null claim":        []byte(`{"contractVersion":"execution-runtime-binding/v1","requestId":"r","workerId":"w","placementId":"p","claimId":null}`),
		"v2 missing registration":   []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":"c"}`),
		"v2 null claim":             []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":null,"preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":"x"}}`),
		"v2 required false":         []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":"c","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":false,"challengeId":"x"}}`),
		"v2 nested unknown":         []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":"c","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":"x","extra":1}}`),
		"v2 duplicate challenge":    []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":"c","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":"x","challengeId":"y"}}`),
		"v2 unstable request":       []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"../r","workerId":"w","placementId":"p","claimId":"c","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":"x"}}`),
		"v2 whitespace challenge":   []byte(`{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","claimId":"c","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":" "}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRuntimeBinding(raw); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
	if got, err := DecodeRuntimeBinding(mustJSON(t, runtimeBindingV2())); err != nil || got.ContractVersion != RuntimeBindingV2ContractVersion {
		t.Fatalf("v2 decode = %+v, %v", got, err)
	}
}

func TestPreflightRegistrationRequestClosedAndBound(t *testing.T) {
	binding := runtimeBindingV2()
	receipt := readyHostReceipt(t, binding)
	request, err := NewPreflightRegistrationRequest(binding, receipt, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	raw := mustJSON(t, request)
	decoded, err := DecodePreflightRegistrationRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := decoded.ReceiptBytes()
	if err != nil || string(bytes) != string(receipt) {
		t.Fatalf("receipt bytes changed: %v", err)
	}

	mutations := []func(*PreflightRegistrationRequest){
		func(v *PreflightRegistrationRequest) { v.ReceiptSHA256 = strings.Repeat("b", 64) },
		func(v *PreflightRegistrationRequest) { v.OperationalPayloadDigest = "not-a-digest" },
		func(v *PreflightRegistrationRequest) { v.ReceiptBytesBase64 = "***" },
		func(v *PreflightRegistrationRequest) { v.RuntimeBinding.WorkerID = "other" },
		func(v *PreflightRegistrationRequest) {
			v.RuntimeBinding.ContractVersion = RuntimeBindingContractVersion
			v.RuntimeBinding.PreflightRegistration = nil
		},
	}
	for i, mutate := range mutations {
		candidate := request
		candidate.RuntimeBinding = request.RuntimeBinding
		ref := *request.RuntimeBinding.PreflightRegistration
		candidate.RuntimeBinding.PreflightRegistration = &ref
		mutate(&candidate)
		if err := ValidatePreflightRegistrationRequest(candidate); err == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
	withUnknown := []byte(string(raw[:len(raw)-1]) + `,"extra":true}`)
	if _, err := DecodePreflightRegistrationRequest(withUnknown); err == nil {
		t.Fatal("unknown request field accepted")
	}
}

func TestPreflightRegistrationRequestRejectsMalformedNestedHostAuthority(t *testing.T) {
	binding := runtimeBindingV2()
	valid := readyHostReceipt(t, binding)
	var base HostAdaptationReceipt
	if err := json.Unmarshal(valid, &base); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*HostAdaptationReceipt){
		"denied unknown plan": func(receipt *HostAdaptationReceipt) {
			receipt.Plan = json.RawMessage(`{"decision":"denied","unexpected":true}`)
			digest := sha256.Sum256(receipt.Plan)
			receipt.PlanDigest = hex.EncodeToString(digest[:])
		},
		"unknown prompt receipt member": func(receipt *HostAdaptationReceipt) {
			receipt.PromptReceipt = json.RawMessage(`{"contractVersion":"donmai.prompt-delivery/v1alpha1","profileId":"test-prompt","decision":"ready","entries":[],"unexpected":true}`)
		},
		"denied tool receipt": func(receipt *HostAdaptationReceipt) {
			var tool agent.ToolLifecycleReceipt
			if err := json.Unmarshal(receipt.ToolLifecycleReceipt, &tool); err != nil {
				t.Fatal(err)
			}
			tool.Decision = "denied"
			receipt.ToolLifecycleReceipt = mustJSON(t, tool)
		},
		"nested receipt differs from plan": func(receipt *HostAdaptationReceipt) {
			var prompt agent.PromptDeliveryReceipt
			if err := json.Unmarshal(receipt.PromptReceipt, &prompt); err != nil {
				t.Fatal(err)
			}
			prompt.ProfileID = "different-profile"
			receipt.PromptReceipt = mustJSON(t, prompt)
		},
		"invalid authority digest": func(receipt *HostAdaptationReceipt) {
			var plan agent.PreparedHarness
			if err := json.Unmarshal(receipt.Plan, &plan); err != nil {
				t.Fatal(err)
			}
			plan.AuthorityDigest = "not-a-digest"
			receipt.Plan = mustJSON(t, plan)
			digest := sha256.Sum256(receipt.Plan)
			receipt.PlanDigest = hex.EncodeToString(digest[:])
		},
		"ready prompt hides required denial": func(receipt *HostAdaptationReceipt) {
			var plan agent.PreparedHarness
			if err := json.Unmarshal(receipt.Plan, &plan); err != nil {
				t.Fatal(err)
			}
			plan.PromptReceipt.Entries = []agent.PromptDeliveryEntry{{
				ID: "required-role", Channel: agent.PromptChannelRoleIntent, Required: true,
				Outcome: agent.PromptOutcomeDenied, DenialCode: agent.PromptDenialDeliveryUnsupported,
			}}
			receipt.Plan = mustJSON(t, plan)
			digest := sha256.Sum256(receipt.Plan)
			receipt.PlanDigest = hex.EncodeToString(digest[:])
			receipt.PromptReceipt = mustJSON(t, plan.PromptReceipt)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			if _, err := NewPreflightRegistrationRequest(binding, mustJSON(t, candidate), strings.Repeat("a", 64)); err == nil {
				t.Fatal("malformed nested host authority was accepted")
			}
		})
	}
}

func TestPreflightRegistrationResponseClosedAndExact(t *testing.T) {
	binding := runtimeBindingV2()
	request, err := NewPreflightRegistrationRequest(binding, readyHostReceipt(t, binding), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	response := PreflightRegistrationResponse{
		ContractVersion: PreflightRegistrationContractVersion,
		Decision:        "authorized", RegistrationID: "registration-1",
		ReceiptSHA256: request.ReceiptSHA256, RuntimeBinding: &binding,
		AuthorizationRevision: 1,
	}
	if err := ValidateAuthorizedPreflightRegistration(request, response); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePreflightRegistrationResponse(mustJSON(t, response))
	if err != nil || decoded.RegistrationID != response.RegistrationID {
		t.Fatalf("response decode = %+v, %v", decoded, err)
	}
	for name, raw := range map[string][]byte{
		"unknown":                           append(mustJSON(t, response)[:len(mustJSON(t, response))-1], []byte(`,"extra":true}`)...),
		"duplicate":                         []byte(`{"contractVersion":"execution-preflight-registration/v1","decision":"refused","decision":"authorized","code":"claim_retired"}`),
		"missing":                           []byte(`{"contractVersion":"execution-preflight-registration/v1","decision":"authorized"}`),
		"authorized null code":              []byte(`{"contractVersion":"execution-preflight-registration/v1","decision":"authorized","registrationId":"reg","receiptSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","runtimeBinding":{"contractVersion":"execution-runtime-binding/v2","requestId":"r","workerId":"w","placementId":"p","preflightRegistration":{"contractVersion":"execution-preflight-registration/v1","required":true,"challengeId":"c"}},"authorizationRevision":1,"code":null}`),
		"refused null authorization fields": []byte(`{"contractVersion":"execution-preflight-registration/v1","decision":"refused","code":"claim_retired","registrationId":null,"receiptSha256":null,"runtimeBinding":null,"authorizationRevision":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePreflightRegistrationResponse(raw); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}

	wrong := response
	wrong.ReceiptSHA256 = strings.Repeat("b", 64)
	if err := ValidateAuthorizedPreflightRegistration(request, wrong); err == nil {
		t.Fatal("mismatched acknowledgement accepted")
	}
	refused := PreflightRegistrationResponse{
		ContractVersion: PreflightRegistrationContractVersion,
		Decision:        "refused", Code: PreflightClaimRetired,
	}
	if err := ValidatePreflightRegistrationResponse(refused); err != nil {
		t.Fatal(err)
	}
	refused.RegistrationID = "forged"
	if err := ValidatePreflightRegistrationResponse(refused); err == nil {
		t.Fatal("refused response carried authorization fields")
	}
	deniedReceipt, err := json.Marshal(HostAdaptationReceipt{
		ContractVersion: HostAdaptationContractVersion,
		RequestID:       binding.RequestID, WorkerID: binding.WorkerID,
		PlacementID: binding.PlacementID, ClaimID: binding.ClaimID,
		Decision: "denied", Denial: "unsupported",
	})
	if err != nil {
		t.Fatal(err)
	}
	deniedRequest, err := NewPreflightRegistrationRequest(binding, deniedReceipt, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	deniedResponse := response
	deniedResponse.ReceiptSHA256 = deniedRequest.ReceiptSHA256
	if err := ValidateAuthorizedPreflightRegistration(deniedRequest, deniedResponse); err == nil {
		t.Fatal("denied host receipt was authorized")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
