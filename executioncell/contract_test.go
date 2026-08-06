package executioncell

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

type fixtureSuite struct {
	ContractVersion string                     `json:"contractVersion"`
	Cells           map[string]json.RawMessage `json:"cells"`
	ClaimBound      struct {
		Intent           json.RawMessage `json:"intent"`
		AdmissionReceipt json.RawMessage `json:"admissionReceipt"`
		EffectiveCell    json.RawMessage `json:"effectiveCell"`
		ClaimReceipt     json.RawMessage `json:"claimReceipt"`
	} `json:"claimBound"`
	Delegation struct {
		ChildIntent           json.RawMessage   `json:"childIntent"`
		ChildCell             json.RawMessage   `json:"childCell"`
		ChildAdmissionReceipt json.RawMessage   `json:"childAdmissionReceipt"`
		ChildSession          json.RawMessage   `json:"childSession"`
		ParentSession         json.RawMessage   `json:"parentSession"`
		Edges                 []json.RawMessage `json:"edges"`
	} `json:"delegation"`
}

func loadFixtures(t *testing.T) fixtureSuite {
	t.Helper()
	var fixtures fixtureSuite
	if err := json.Unmarshal(fixtureSuiteJSON, &fixtures); err != nil {
		t.Fatalf("decode fixture suite: %v", err)
	}
	return fixtures
}

func requireContractError(t *testing.T, err error, code ContractErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	var contractErr *ContractError
	if !errors.As(err, &contractErr) {
		t.Fatalf("error type = %T, want *ContractError: %v", err, err)
	}
	if contractErr.Code != code {
		t.Fatalf("error code = %s, want %s: %v", contractErr.Code, code, err)
	}
}

func TestSemanticFixtureCellsDecode(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	wantNames := []string{
		"claudeSubscription", "codexSubscription", "antigravityGemini",
		"antigravityAnthropic", "openCodeSubscription", "ollamaSubscription",
		"lmStudioNoAuth", "lmStudioApiKey", "claimBoundPool",
	}
	if len(fixtures.Cells) != len(wantNames) {
		t.Fatalf("fixture cells = %d, want %d", len(fixtures.Cells), len(wantNames))
	}
	for _, name := range wantNames {
		raw, ok := fixtures.Cells[name]
		if !ok {
			t.Errorf("missing fixture %s", name)
			continue
		}
		if _, err := DecodeResolvedExecutionCell(raw); err != nil {
			t.Errorf("decode fixture %s: %v", name, err)
		}
	}
}

func TestOwnershipAxesAndGenericEndpointStayIndependent(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	keyed, err := DecodeResolvedExecutionCell(fixtures.Cells["lmStudioApiKey"])
	if err != nil {
		t.Fatal(err)
	}
	none, err := DecodeResolvedExecutionCell(fixtures.Cells["lmStudioNoAuth"])
	if err != nil {
		t.Fatal(err)
	}
	if keyed.Model.Author != "meta" || keyed.Endpoint.Operator != "local-operator" || keyed.AuthBinding.Authority != "local-user" || keyed.Harness.ID != "opencode" || keyed.Placement.ID != "host_mac_studio" {
		t.Fatalf("ownership axes collapsed: %+v", keyed)
	}
	if !sameValue(keyed.Model, none.Model) || !sameValue(keyed.Endpoint, none.Endpoint) || !sameValue(keyed.Harness, none.Harness) {
		t.Fatal("generic endpoint variants changed model, endpoint, or harness identity")
	}
	if none.AuthBinding.Mechanism != AuthNone || keyed.AuthBinding.Mechanism != AuthAPIKey {
		t.Fatalf("optional auth mechanisms = %q/%q, want none/api_key", none.AuthBinding.Mechanism, keyed.AuthBinding.Mechanism)
	}
	if keyed.Endpoint.Protocol != "openai-chat" {
		t.Fatalf("endpoint protocol = %q, want openai-chat", keyed.Endpoint.Protocol)
	}
}

func TestClosedDecoderTypedFailures(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	var intent map[string]any
	if err := json.Unmarshal(fixtures.ClaimBound.Intent, &intent); err != nil {
		t.Fatal(err)
	}
	mutate := func(change func(map[string]any)) []byte {
		original, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		var clone map[string]any
		if err := json.Unmarshal(original, &clone); err != nil {
			t.Fatal(err)
		}
		change(clone)
		raw, err := json.Marshal(clone)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	_, err := DecodeDispatchIntent(mutate(func(value map[string]any) {
		value["contractVersion"] = "donmai.execution-cell/v9"
	}), nil)
	requireContractError(t, err, ErrorUnsupportedContractVersion)

	_, err = DecodeDispatchIntent(mutate(func(value map[string]any) { value["surprise"] = true }), nil)
	requireContractError(t, err, ErrorUnknownField)

	_, err = DecodeDispatchIntent(mutate(func(value map[string]any) {
		auth := value["authBinding"].(map[string]any)
		auth["mechanism"] = "local"
	}), nil)
	requireContractError(t, err, ErrorUnknownDiscriminator)

	duplicate := bytes.Replace(fixtures.ClaimBound.Intent,
		[]byte(`"requestId": "request_claim_bound_pool"`),
		[]byte(`"requestId": "request_claim_bound_pool", "requestId": "second"`), 1)
	if bytes.Equal(duplicate, fixtures.ClaimBound.Intent) {
		t.Fatal("fixture format changed; duplicate-field mutation did not apply")
	}
	_, err = DecodeDispatchIntent(duplicate, nil)
	requireContractError(t, err, ErrorUnknownField)

	_, err = DecodeDispatchIntent(fixtures.ClaimBound.Intent, &ExecutionSelectorRegistry{
		HarnessVersions: map[string][]string{"codex": {"other-version"}},
	})
	requireContractError(t, err, ErrorInvalidReference)

	_, err = DecodeDispatchIntent(mutate(func(value map[string]any) {
		auth := value["authBinding"].(map[string]any)
		auth["id"] = "sk-FAKEFAKEFAKE"
	}), nil)
	requireContractError(t, err, ErrorSecretMaterialForbidden)
}

func TestImmutableReceiptsSecretRejectionAndNarrowClaim(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	admission, err := DecodeAdmissionReceipt(fixtures.ClaimBound.AdmissionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := DecodeClaimReceipt(fixtures.ClaimBound.ClaimReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertNarrowClaim(admission, claim); err != nil {
		t.Fatalf("valid narrow claim: %v", err)
	}

	first := admission.Value()
	if first.Cell == nil {
		t.Fatal("admitted fixture has no cell")
	}
	first.Cell.Model.ID = "mutated"
	second := admission.Value()
	if second.Cell == nil || second.Cell.Model.ID == "mutated" {
		t.Fatal("immutable admission wrapper exposed mutable receipt state")
	}

	badClaim := claim.Value()
	if badClaim.EffectiveCell == nil {
		t.Fatal("claimed fixture has no effective cell")
	}
	badClaim.EffectiveCell.Model.ID = "different-model"
	badRaw, err := json.Marshal(badClaim)
	if err != nil {
		t.Fatal(err)
	}
	badImmutable, err := DecodeClaimReceipt(badRaw)
	if err != nil {
		t.Fatal(err)
	}
	requireContractError(t, AssertNarrowClaim(admission, badImmutable), ErrorInvalidReference)

	var secretReceipt map[string]any
	if err := json.Unmarshal(fixtures.ClaimBound.AdmissionReceipt, &secretReceipt); err != nil {
		t.Fatal(err)
	}
	decisions := secretReceipt["resolverDecisions"].([]any)
	decisions[0].(map[string]any)["reason"] = "Resolved from Bearer abcdefghijklmnopqrstuvwxyz"
	secretRaw, err := json.Marshal(secretReceipt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeAdmissionReceipt(secretRaw)
	requireContractError(t, err, ErrorSecretMaterialForbidden)
}

func TestDelegationTransportDoesNotChangeChildIdentity(t *testing.T) {
	t.Parallel()
	fixtures := loadFixtures(t)
	childIntent, err := DecodeDispatchIntent(fixtures.Delegation.ChildIntent, nil)
	if err != nil {
		t.Fatal(err)
	}
	childCell, err := DecodeResolvedExecutionCell(fixtures.Delegation.ChildCell)
	if err != nil {
		t.Fatal(err)
	}
	childSession, err := DecodeSessionRef(fixtures.Delegation.ChildSession)
	if err != nil {
		t.Fatal(err)
	}
	if childSession.Value().AdmissionReceiptID == "" {
		t.Fatal("child session is not linked to an admission receipt")
	}
	wantTransports := []DelegationTransport{
		TransportNativeHarness, TransportPlatformDispatch, TransportA2A, TransportHostCLI,
	}
	if len(fixtures.Delegation.Edges) != len(wantTransports) {
		t.Fatalf("edge count = %d, want %d", len(fixtures.Delegation.Edges), len(wantTransports))
	}
	var parent SessionRef
	for index, raw := range fixtures.Delegation.Edges {
		edge, err := DecodeDelegationEdgeIntent(raw)
		if err != nil {
			t.Fatalf("edge %d: %v", index, err)
		}
		if edge.Transport != wantTransports[index] {
			t.Errorf("edge %d transport = %q, want %q", index, edge.Transport, wantTransports[index])
		}
		if edge.ChildRequestID != childIntent.RequestID {
			t.Errorf("edge %d child request = %q, want %q", index, edge.ChildRequestID, childIntent.RequestID)
		}
		if index == 0 {
			parent = edge.Parent
		} else if !sameValue(parent, edge.Parent) {
			t.Errorf("edge %d changed parent session identity", index)
		}
	}
	if childCell.ContractVersion != ContractVersion {
		t.Fatalf("child cell version = %q", childCell.ContractVersion)
	}
}
