package localqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/result"
)

func testDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func testJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := executioncell.CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testBinding() ConfiguredHostBinding {
	return ConfiguredHostBinding{
		ScopeID: "local-scope", HostID: "local-host", WorkerID: "local-worker",
		Selectors: &executioncell.ExecutionSelectorRegistry{
			HarnessVersions: map[string][]string{"codex": {"harness/v2"}},
			Models:          []string{"openai/gpt-5-codex"},
			Endpoints:       []string{"local-endpoint"},
			AuthBindings:    []string{"host-login"},
			Placements:      []string{"local-host"},
			Capabilities:    []string{},
		},
	}
}

func testProducerEvidence(t *testing.T, cell *executioncell.ResolvedExecutionCell) (json.RawMessage, string) {
	t.Helper()
	configuration := testJSON(t, map[string]any{
		"ScopeID": "local-scope", "WorkerID": "local-worker", "PlacementID": "local-host", "RuntimeTransportMode": "local",
		"Revision": "fixture-v1", "EndpointID": "local-endpoint", "AuthBindingID": "host-login",
		"Harness": "codex", "Model": cell.Model,
		"Catalog": map[string]any{
			"Kind":  "operator",
			"Entry": map[string]any{"model": cell.Model, "host": "oauth-cli", "revision": "fixture-catalog-v1"},
		},
	})
	configurationDigest, err := executioncell.DigestContractValue(configuration)
	if err != nil {
		t.Fatal(err)
	}
	manifest := agent.HarnessManifest{
		Name: agent.HarnessCodex, Family: agent.FamilyHarness, ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsOneShot: true, Drives: []agent.WireProtocol{agent.ProtoOpenAIResponses},
			DrivesHosts: []agent.ServingHost{agent.HostOAuthCLI}, Transport: agent.TransportSubprocessRPC,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{}, ToolLifecycle: []agent.ToolLifecycleProfile{},
	}
	inventory := testJSON(t, map[string]any{
		"manifest": manifest, "binarySha256": testDigest("opened-binary"),
		"configurationDigest": configurationDigest, "authReferencePresent": true,
	})
	inventoryDigest, err := executioncell.DigestContractValue(inventory)
	if err != nil {
		t.Fatal(err)
	}
	compatibilityCell := matrix.HarnessEndpointCell{
		Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: agent.HostOAuthCLI,
		Protocol: agent.ProtoOpenAIResponses, Transport: agent.TransportSubprocessRPC,
		AuthModes: []agent.AuthMode{agent.AuthHostSession}, BringsOwnAuth: true,
		OneShot: true, CostModel: agent.CostHostSubscription,
	}
	compatibility := testJSON(t, map[string]any{"Cell": compatibilityCell, "Manifest": manifest})
	compatibilityDigest, err := executioncell.DigestContractValue(compatibility)
	if err != nil {
		t.Fatal(err)
	}
	cell.Endpoint.Revision = configurationDigest
	cell.RuntimeInventoryDigest = inventoryDigest
	cell.CompatibilityDigest = compatibilityDigest
	evidence := testJSON(t, map[string]any{
		"configuration": json.RawMessage(configuration),
		"inventory":     json.RawMessage(inventory),
		"compatibility": json.RawMessage(compatibility),
	})
	return evidence, configurationDigest
}

func testEnvelope(t *testing.T) AdmissionEnvelope {
	return testEnvelopeFor(t, "local-session", 7)
}

func testEnvelopeFor(t *testing.T, sessionID string, issueNumber int) AdmissionEnvelope {
	t.Helper()
	harness := executioncell.HarnessRef{ID: "codex", Version: "harness/v2"}
	model := executioncell.ModelRef{ID: "gpt-5-codex", Author: "openai"}
	endpoint := executioncell.ServingEndpointRef{
		ID: "local-endpoint", Protocol: "openai-responses", Operator: "openai", Revision: testDigest("endpoint-config"),
	}
	auth := executioncell.AuthBindingRef{
		ID: "host-login", Mechanism: executioncell.AuthCLISession,
		CommercialMode: executioncell.CommercialSubscription, Authority: "openai",
		BindingScope: executioncell.ScopeHost, Portability: executioncell.HostBound,
		Delivery: executioncell.DeliveryHostCLIHomeReference,
	}
	placement := executioncell.PlacementRef{ID: "local-host", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact}
	intent := executioncell.DispatchIntent{
		ContractVersion: executioncell.ContractVersion, RequestID: sessionID,
		Harness: &harness, Model: model, Endpoint: &endpoint, AuthBinding: &auth, Placement: &placement,
		SessionMode:          executioncell.SessionAutonomous,
		RequiredCapabilities: []executioncell.CapabilityRequirement{},
		OptionalCapabilities: []executioncell.CapabilityRequirement{},
		FallbackAlternatives: executioncell.FallbackPolicy{},
	}
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion, Harness: harness, Model: model,
		Endpoint: endpoint, AuthBinding: auth, Placement: placement,
		SessionMode:         executioncell.SessionAutonomous,
		GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceImplemented,
		CompatibilityDigest: testDigest("matrix-cell"), RuntimeInventoryDigest: testDigest("installed-runtime"),
	}
	evidence, configurationDigest := testProducerEvidence(t, &cell)
	endpoint.Revision = configurationDigest
	intent.Endpoint = &endpoint
	operational := testJSON(t, map[string]any{
		"sessionId": sessionID, "organizationId": "local-scope", "body": "fixture issue", "workType": "development",
	})
	operationalDigest, err := executioncell.DigestOperationalPayload(operational)
	if err != nil {
		t.Fatal(err)
	}
	intentDigest, err := executioncell.DigestContractValue(intent)
	if err != nil {
		t.Fatal(err)
	}
	decisions := make([]executioncell.ResolverDecision, 0, 5)
	for _, axis := range []struct{ field, selected string }{
		{"harness", "harness:codex@harness/v2"},
		{"model", "model:openai/gpt-5-codex"},
		{"endpoint", "endpoint:local-endpoint"},
		{"authBinding", "auth-binding:host-login"},
		{"placement", "placement:local-host"},
	} {
		decisions = append(decisions, executioncell.ResolverDecision{
			Kind: executioncell.DecisionExplicit, Field: axis.field, SelectedRef: axis.selected,
			SourceRef: "local-config:" + configurationDigest, Reason: "explicit fixture selection",
		})
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-" + sessionID, RequestID: sessionID,
		Decision: executioncell.AdmissionAdmitted, IntentDigest: intentDigest,
		OperationalPayloadDigest: operationalDigest, Cell: &cell,
		ResolverDecisions: decisions, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       sessionID, WorkerID: "local-worker", PlacementID: "local-host",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{
			ContractVersion: executioncell.PreflightRegistrationContractVersion,
			Required:        true, ChallengeID: "local-challenge-" + sessionID,
		},
	}
	plan := testJSON(t, map[string]any{"operationalPayloadDigest": operationalDigest, "decision": "ready"})
	planSum := sha256.Sum256(plan)
	host := executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID, WorkerID: "local-worker", PlacementID: "local-host", Decision: "ready",
		Plan: plan, PlanDigest: hex.EncodeToString(planSum[:]),
		PromptReceipt:        testJSON(t, map[string]any{"decision": "ready"}),
		ToolLifecycleReceipt: testJSON(t, map[string]any{"decision": "ready"}),
	}
	return AdmissionEnvelope{
		ScopeID: "local-scope",
		Source:  GitHubSource{RepositoryID: 1234, OwnerRepo: "acme/repo", IssueNumber: issueNumber, IssueURL: fmt.Sprintf("https://github.com/acme/repo/issues/%d", issueNumber)},
		Session: executioncell.SessionRef{
			ContractVersion: executioncell.ContractVersion, SessionID: sessionID,
			AdmissionReceiptID: receipt.ReceiptID, Mode: executioncell.SessionAutonomous,
		},
		Intent: intent, Receipt: testJSON(t, receipt), EffectiveCell: testJSON(t, cell),
		RuntimeBinding: testJSON(t, binding), OperationalPayload: operational,
		HostPreflight: testJSON(t, host), ProducerEvidence: evidence,
		InitialEvent: LifecycleEvent{
			Type: EventSessionAdmitted, ScopeID: "local-scope", SessionID: sessionID,
			ReceiptID: receipt.ReceiptID, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Dispatch: DispatchOutbox{State: DispatchPending}, CredentialHandle: "cred_fixture",
	}
}

func TestAdmitDurableDedupeAndBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	binding := testBinding()
	store, err := Open(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	binding.Selectors.Models[0] = "untrusted-caller-mutation"
	if got := store.CurrentRevision(); got != 1 {
		t.Fatalf("initial authority revision = %d, want 1", got)
	}
	envelope := testEnvelope(t)
	record, err := store.Admit(context.Background(), store.CurrentRevision(), envelope)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if record.DispatchState != DispatchPending || record.Envelope.Dispatch.EnvelopeSHA256 != record.EnvelopeSHA256 {
		t.Fatalf("admission and outbox disagree: %+v", record)
	}
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); !errors.Is(err, ErrAlreadyAdmitted) {
		t.Fatalf("duplicate issue Admit = %v, want ErrAlreadyAdmitted", err)
	}
	// Mutating caller-owned bytes cannot rewrite the durable record.
	envelope.Receipt[0] = 'x'
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	changedBinding := testBinding()
	changedBinding.HostID = "other-host"
	if _, err := Open(root, changedBinding); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("Open with different configured host = %v, want mismatch", err)
	}
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.CurrentRevision() != record.Revision {
		t.Fatalf("recovered revision = %d, want %d", reopened.CurrentRevision(), record.Revision)
	}
	pending, err := reopened.PendingDispatch(context.Background())
	if err != nil || len(pending) != 1 || pending[0].EnvelopeSHA256 != record.EnvelopeSHA256 {
		t.Fatalf("recovered pending dispatch = %+v, %v", pending, err)
	}
	if bytes.Equal(pending[0].Envelope.Receipt, envelope.Receipt) {
		t.Fatal("caller mutation changed durable receipt")
	}
	if !bytes.Equal(pending[0].Envelope.ProducerEvidence, record.Envelope.ProducerEvidence) {
		t.Fatal("restart changed producer evidence bytes")
	}
}

func TestOpenCreatesPrivateParentChain(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "donmai", "localqueue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("first-use nested local queue: %v", err)
	}
	_ = store.Close()
	for _, path := range []string{filepath.Dir(filepath.Dir(root)), filepath.Dir(root), root, filepath.Join(root, "transactions")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("private parent/root %q: info=%v err=%v", path, info, err)
		}
	}
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("reopen nested local queue: %v", err)
	}
	_ = reopened.Close()
}

func TestHostPreflightPlanBytesSurviveJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	var host executioncell.HostAdaptationReceipt
	if err := json.Unmarshal(envelope.HostPreflight, &host); err != nil {
		t.Fatal(err)
	}
	var receipt executioncell.AdmissionReceipt
	if err := json.Unmarshal(envelope.Receipt, &receipt); err != nil {
		t.Fatal(err)
	}
	// This plan is valid compact JSON, but its key order is deliberately not
	// RFC8785. The compiler's PlanDigest binds these exact bytes.
	host.Plan = []byte(`{"z":"fixture","operationalPayloadDigest":"` + receipt.OperationalPayloadDigest + `","a":1}`)
	planSum := sha256.Sum256(host.Plan)
	host.PlanDigest = hex.EncodeToString(planSum[:])
	envelope.HostPreflight, err = json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(envelope.HostPreflight); err != nil {
		t.Fatalf("fixture plan is invalid before persistence: %v", err)
	}
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatalf("Admit byte-bound host plan: %v", err)
	}
	_ = store.Close()
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("reopen byte-bound host plan: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	projection, err := reopened.Session(context.Background(), "local-scope", envelope.Session.SessionID)
	if err != nil || !bytes.Equal(projection.Admission.Envelope.HostPreflight, envelope.HostPreflight) {
		t.Fatalf("host preflight bytes changed on restart: %v", err)
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(projection.Admission.Envelope.HostPreflight); err != nil {
		t.Fatalf("recovered plan digest changed: %v", err)
	}
}

func TestKernelWriterLockAcrossProcesses(t *testing.T) {
	if os.Getenv("DONMAI_LOCALQUEUE_LOCK_CHILD") == "1" {
		root := os.Getenv("DONMAI_LOCALQUEUE_LOCK_ROOT")
		other, err := Open(root, testBinding())
		if other != nil {
			_ = other.Close()
		}
		if !errors.Is(err, ErrWriterLocked) {
			t.Fatalf("second process Open = %v, want kernel writer lock refusal", err)
		}
		return
	}
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(root, testBinding()); !errors.Is(err, ErrWriterLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("same-process second Open = %v, want writer lock refusal", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.StartProcess(executable, []string{executable, "-test.run=^TestKernelWriterLockAcrossProcesses$"}, &os.ProcAttr{
		Env:   []string{"DONMAI_LOCALQUEUE_LOCK_CHILD=1", "DONMAI_LOCALQUEUE_LOCK_ROOT=" + root},
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		t.Fatalf("start separate-process kernel lock control: %v", err)
	}
	state, err := child.Wait()
	if err != nil || !state.Success() {
		t.Fatalf("separate-process kernel lock control: state=%v error=%v", state, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("Open after writer release: %v", err)
	}
	_ = reopened.Close()
}

func TestAdmitRejectsMutatedAuthorityAndDigests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := testEnvelope(t)
	tests := []struct {
		name   string
		mutate func(*AdmissionEnvelope)
	}{
		{"wrong repository identity", func(e *AdmissionEnvelope) { e.Source.IssueURL = "https://github.com/acme/other/issues/7" }},
		{"wrong placement", func(e *AdmissionEnvelope) {
			var binding executioncell.RuntimeBinding
			_ = json.Unmarshal(e.RuntimeBinding, &binding)
			binding.PlacementID = "other-host"
			e.RuntimeBinding = testJSON(t, binding)
		}},
		{"pool claim smuggling", func(e *AdmissionEnvelope) {
			var binding executioncell.RuntimeBinding
			_ = json.Unmarshal(e.RuntimeBinding, &binding)
			binding.ClaimID = "claim-forbidden"
			e.RuntimeBinding = testJSON(t, binding)
		}},
		{"receipt digest mismatch", func(e *AdmissionEnvelope) {
			var receipt executioncell.AdmissionReceipt
			_ = json.Unmarshal(e.Receipt, &receipt)
			receipt.IntentDigest = testDigest("wrong-intent")
			e.Receipt = testJSON(t, receipt)
		}},
		{"noncanonical payload", func(e *AdmissionEnvelope) { e.OperationalPayload = append([]byte("  "), e.OperationalPayload...) }},
		{"bearer instead of handle", func(e *AdmissionEnvelope) { e.CredentialHandle = "rsp_live_fake" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envelope := cloneValue(base)
			tc.mutate(&envelope)
			if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("Admit malformed %s = %v, want ErrInvalidEnvelope", tc.name, err)
			}
		})
	}
	if store.CurrentRevision() != 1 {
		t.Fatalf("invalid admissions changed revision to %d", store.CurrentRevision())
	}
}

func mutateEvidenceSection(t *testing.T, envelope *AdmissionEnvelope, section string, mutate func(map[string]any)) {
	t.Helper()
	var outer map[string]any
	if err := json.Unmarshal(envelope.ProducerEvidence, &outer); err != nil {
		t.Fatal(err)
	}
	value, ok := outer[section].(map[string]any)
	if !ok {
		t.Fatalf("producer evidence section %q missing", section)
	}
	mutate(value)
	envelope.ProducerEvidence = testJSON(t, outer)
}

func updateAdmittedCell(t *testing.T, envelope *AdmissionEnvelope, mutate func(*executioncell.ResolvedExecutionCell)) {
	t.Helper()
	var receipt executioncell.AdmissionReceipt
	if err := json.Unmarshal(envelope.Receipt, &receipt); err != nil || receipt.Cell == nil {
		t.Fatal("decode admitted cell for mutation")
	}
	mutate(receipt.Cell)
	envelope.Receipt = testJSON(t, receipt)
	envelope.EffectiveCell = testJSON(t, receipt.Cell)
}

func producerEvidenceSectionDigest(t *testing.T, envelope AdmissionEnvelope, section string) string {
	t.Helper()
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(envelope.ProducerEvidence, &outer); err != nil {
		t.Fatal(err)
	}
	digest, err := executioncell.DigestContractValue(outer[section])
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestAdmitRejectsProducerEvidenceMutation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	base := testEnvelope(t)
	tests := []struct {
		name   string
		mutate func(*AdmissionEnvelope)
	}{
		{"configuration", func(e *AdmissionEnvelope) {
			mutateEvidenceSection(t, e, "configuration", func(v map[string]any) { v["EndpointID"] = "changed-endpoint" })
		}},
		{"inventory", func(e *AdmissionEnvelope) {
			mutateEvidenceSection(t, e, "inventory", func(v map[string]any) { v["binarySha256"] = testDigest("different-binary") })
		}},
		{"compatibility", func(e *AdmissionEnvelope) {
			mutateEvidenceSection(t, e, "compatibility", func(v map[string]any) {
				cell := v["Cell"].(map[string]any)
				cell["host"] = "direct"
			})
		}},
		{"rehashed manifest disagreement", func(e *AdmissionEnvelope) {
			mutateEvidenceSection(t, e, "inventory", func(v map[string]any) {
				manifest := v["manifest"].(map[string]any)
				manifest["humanLabel"] = "different harness"
			})
			digest := producerEvidenceSectionDigest(t, *e, "inventory")
			updateAdmittedCell(t, e, func(cell *executioncell.ResolvedExecutionCell) { cell.RuntimeInventoryDigest = digest })
		}},
		{"rehashed incompatible host", func(e *AdmissionEnvelope) {
			mutateEvidenceSection(t, e, "compatibility", func(v map[string]any) {
				cell := v["Cell"].(map[string]any)
				cell["host"] = "direct"
			})
			digest := producerEvidenceSectionDigest(t, *e, "compatibility")
			updateAdmittedCell(t, e, func(cell *executioncell.ResolvedExecutionCell) { cell.CompatibilityDigest = digest })
		}},
		{"unknown wrapper field", func(e *AdmissionEnvelope) {
			var outer map[string]any
			_ = json.Unmarshal(e.ProducerEvidence, &outer)
			outer["credential"] = "not-allowed"
			e.ProducerEvidence = testJSON(t, outer)
		}},
		{"noncanonical evidence", func(e *AdmissionEnvelope) {
			e.ProducerEvidence = append([]byte("  "), e.ProducerEvidence...)
		}},
		{"missing evidence", func(e *AdmissionEnvelope) { e.ProducerEvidence = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envelope := cloneValue(base)
			tc.mutate(&envelope)
			if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("mutated producer evidence Admit = %v, want invalid envelope", err)
			}
		})
	}
	if store.CurrentRevision() != 1 {
		t.Fatalf("evidence mutations advanced revision to %d", store.CurrentRevision())
	}
}

func TestJournalFaultRecoveryAndCorruption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	store.fault = func(stage string) error {
		if stage == "before_link" {
			return errors.New("injected pre-link failure")
		}
		return nil
	}
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err == nil {
		t.Fatal("pre-link failure reported a durable admission")
	}
	if store.CurrentRevision() != 1 {
		t.Fatal("pre-link failure advanced revision")
	}
	store.fault = func(stage string) error {
		if stage == "after_link" {
			return errors.New("injected post-link ambiguity")
		}
		return nil
	}
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("post-link failure = %v, want ambiguous commit", err)
	}
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("poisoned writer allowed retry: %v", err)
	}
	_ = store.Close()
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("recover visible post-link transaction: %v", err)
	}
	if _, err := reopened.Admit(context.Background(), reopened.CurrentRevision(), envelope); !errors.Is(err, ErrAlreadyAdmitted) {
		t.Fatalf("recovered transaction did not dedupe: %v", err)
	}
	_ = reopened.Close()
	path := filepath.Join(root, "transactions", transactionName(2))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	journalRoot, err := os.OpenRoot(filepath.Join(root, "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := journalRoot.OpenFile(transactionName(2), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = journalRoot.Close()
		t.Fatal(err)
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	rootCloseErr := journalRoot.Close()
	if err := errors.Join(writeErr, closeErr, rootCloseErr); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, testBinding()); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("corrupt journal Open = %v, want ErrCorruptStore", err)
	}
}

func TestJournalFileAndDirectorySyncFaults(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	store.fileSync = func(*os.File) error { return errors.New("injected file fsync failure") }
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err == nil {
		t.Fatal("file fsync failure reported durable admission")
	}
	if store.CurrentRevision() != 1 {
		t.Fatal("file fsync failure advanced revision")
	}
	_ = store.Close()
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := reopened.PendingDispatch(context.Background()); len(pending) != 0 {
		t.Fatal("file fsync failure left a visible admission")
	}
	reopened.dirSync = func(*os.Root) error { return errors.New("injected directory fsync failure") }
	if _, err := reopened.Admit(context.Background(), reopened.CurrentRevision(), envelope); !errors.Is(err, ErrAmbiguousCommit) {
		t.Fatalf("directory fsync failure = %v, want ambiguous commit", err)
	}
	_ = reopened.Close()
	recovered, err := Open(root, testBinding())
	if err != nil {
		t.Fatalf("recover linked but unsynced transaction: %v", err)
	}
	defer func() { _ = recovered.Close() }()
	if pending, _ := recovered.PendingDispatch(context.Background()); len(pending) != 1 {
		t.Fatalf("recovered ambiguous directory-sync admission count = %d, want 1", len(pending))
	}
}

func TestJournalGapAndSymlinkFailClosed(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "queue")
		store, err := Open(root, testBinding())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Admit(context.Background(), store.CurrentRevision(), testEnvelope(t)); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		dir := filepath.Join(root, "transactions")
		if err := os.Rename(filepath.Join(dir, transactionName(2)), filepath.Join(dir, transactionName(3))); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, testBinding()); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("journal with revision gap = %v, want corruption", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "queue")
		store, err := Open(root, testBinding())
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		link := filepath.Join(root, "transactions", transactionName(2))
		if err := os.Symlink(filepath.Join(root, "writer.lock"), link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, testBinding()); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("symlinked journal member = %v, want corruption", err)
		}
	})
}

func TestAttemptFenceUnknownLaunchAndExactTerminalReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Claim(context.Background(), store.CurrentRevision(), envelope.Session)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Generation != 1 || attempt.AttemptID == "" || attempt.WorkerID != "local-worker" {
		t.Fatalf("unexpected private attempt: %+v", attempt)
	}
	if err := store.BeginLaunch(context.Background(), store.CurrentRevision(), attempt); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if pending, _ := reopened.PendingDispatch(context.Background()); len(pending) != 0 {
		t.Fatal("unknown launch was automatically requeued")
	}
	projection, err := reopened.Session(context.Background(), "local-scope", envelope.Session.SessionID)
	if err != nil || projection.Admission.DispatchState != DispatchUnknown {
		t.Fatalf("recovered launch state = %+v, %v", projection, err)
	}
	wrong := attempt
	wrong.Generation++
	if err := reopened.RecordObservation(context.Background(), wrong, BoundedTypedObservation{Kind: ObservationLiveness, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}); !errors.Is(err, ErrAttemptMismatch) {
		t.Fatalf("wrong generation refreshed liveness: %v", err)
	}
	process := OwnedProcessCorrelation{ProcessID: 123, ProcessStart: "birth-token", SessionDetailSHA256: testDigest("session-detail")}
	if err := reopened.ObserveLaunch(context.Background(), reopened.CurrentRevision(), attempt, process); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecordObservation(context.Background(), attempt, BoundedTypedObservation{Kind: ObservationLiveness, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	beforeCompletion := reopened.CurrentRevision()
	if _, err := reopened.CommitTerminal(context.Background(), attempt, []byte(`{"workerId":"local-worker","status":"completed"}`)); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("completed development without PR/head evidence = %v, want invalid envelope", err)
	}
	if reopened.CurrentRevision() != beforeCompletion {
		t.Fatal("missing PR/head evidence advanced terminal revision")
	}
	body := []byte(`{ "workerId" : "local-worker", "status" : "completed", "summary" : "done", "pullRequestUrl" : "https://github.com/acme/repo/pull/9", "commitSha" : "` + strings.Repeat("a", 40) + `" }`)
	terminal, err := reopened.CommitTerminal(context.Background(), attempt, body)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := reopened.CommitTerminal(context.Background(), attempt, bytes.Clone(body))
	if err != nil || !bytes.Equal(replayed.Body, terminal.Body) || replayed.Revision != terminal.Revision {
		t.Fatalf("exact terminal replay = %+v, %v", replayed, err)
	}
	changed := []byte(`{"workerId":"local-worker","status":"completed","summary":"changed"}`)
	if _, err := reopened.CommitTerminal(context.Background(), attempt, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed terminal replay = %v, want conflict", err)
	}
	publication, err := reopened.PendingPublication(context.Background())
	if err != nil || len(publication) != 1 || publication[0].ResultSHA256 != terminal.BodySHA256 ||
		publication[0].Operation != PublicationGitHubPR ||
		publication[0].URL != "https://github.com/acme/repo/pull/9" || publication[0].HeadSHA != strings.Repeat("a", 40) {
		t.Fatalf("terminal publication outbox = %+v, %v", publication, err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL:    "https://github.com/acme/repo/issues/7#issuecomment-42",
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("issue comment satisfied completed PR/head publication: %v", err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/acme/repo/pull/9", ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("headless PR URL satisfied completed PR/head publication: %v", err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/acme/repo/pull/10", HeadSHA: strings.Repeat("a", 40),
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrelated same-repo PR satisfied completed publication: %v", err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/acme/repo/pull/9", HeadSHA: strings.Repeat("b", 40),
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong pushed head satisfied completed publication: %v", err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/other/repo/pull/9", HeadSHA: strings.Repeat("a", 40),
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("publication for a different repository = %v, want conflict", err)
	}
	if err := reopened.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/acme/repo/pull/9", HeadSHA: strings.Repeat("a", 40),
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := reopened.PendingPublication(context.Background()); len(pending) != 0 {
		t.Fatal("completed publication remained pending")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	finalStore, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = finalStore.Close() }()
	final, err := finalStore.Session(context.Background(), "local-scope", envelope.Session.SessionID)
	if err != nil || final.Terminal == nil || !bytes.Equal(final.Terminal.Body, body) ||
		len(final.Publications) != 1 || final.Publications[0].URL != "https://github.com/acme/repo/pull/9" ||
		final.Publications[0].State != "delivered" {
		t.Fatalf("recovered terminal/publication = %+v, %v", final, err)
	}
	if replayed, err := finalStore.CommitTerminal(context.Background(), attempt, bytes.Clone(body)); err != nil ||
		replayed.Revision != terminal.Revision || !bytes.Equal(replayed.Body, body) {
		t.Fatalf("recovered byte-identical terminal replay = %+v, %v", replayed, err)
	}
}

func TestPrestartDenialCanPublishIssueCommentWithoutHead(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	envelope := testEnvelope(t)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Claim(context.Background(), store.CurrentRevision(), envelope.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObservation(context.Background(), attempt, BoundedTypedObservation{
		Kind: ObservationPrestartDeny, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	beforeTerminal := store.CurrentRevision()
	if err := store.BeginLaunch(context.Background(), beforeTerminal, attempt); !errors.Is(err, ErrAttemptMismatch) {
		t.Fatalf("prestart denial allowed launch intent: %v", err)
	}
	if _, err := store.CommitTerminal(context.Background(), attempt, []byte(`{"workerId":"local-worker","status":"completed"}`)); err == nil {
		t.Fatal("prestart denial became successful completion without PR evidence")
	}
	completedWithPR := []byte(`{"workerId":"local-worker","status":"completed","pullRequestUrl":"https://github.com/acme/repo/pull/9","commitSha":"` + strings.Repeat("a", 40) + `"}`)
	if _, err := store.CommitTerminal(context.Background(), attempt, completedWithPR); !errors.Is(err, ErrAttemptMismatch) {
		t.Fatalf("prestart denial became successful completion with PR/head evidence: %v", err)
	}
	if store.CurrentRevision() != beforeTerminal {
		t.Fatal("refused prestart success advanced durable revision")
	}
	if pending, _ := store.PendingPublication(context.Background()); len(pending) != 0 {
		t.Fatal("refused prestart success produced a publication outbox")
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: "http://127.0.0.1:1", WorkerID: attempt.WorkerID, AuthToken: "fixture-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
		Status: "failed", FailureMode: "prestart-denied", Summary: "prestart denied",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTerminal(context.Background(), attempt, body); err != nil {
		t.Fatal(err)
	}
	publication, err := store.PendingPublication(context.Background())
	if err != nil || len(publication) != 1 || publication[0].Operation != PublicationGitHubComment {
		t.Fatalf("failed result publication = %+v, %v", publication, err)
	}
	if err := store.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: "https://github.com/acme/repo/pull/9", HeadSHA: strings.Repeat("a", 40),
		ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("PR/head satisfied failed comment publication: %v", err)
	}
	commentURL := "https://github.com/acme/repo/issues/7#issuecomment-42"
	if err := store.CompletePublication(context.Background(), publication[0].Key, PublicationEvidence{
		URL: commentURL, ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("comment-only publication: %v", err)
	}
	projection, err := store.Session(context.Background(), "local-scope", envelope.Session.SessionID)
	if err != nil || len(projection.Publications) != 1 || projection.Publications[0].URL != commentURL || projection.Publications[0].HeadSHA != "" {
		t.Fatalf("comment-only projection = %+v, %v", projection.Publications, err)
	}
}

func TestRealPosterTerminalBodyWithPublicPRCorrelation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	envelope := testEnvelope(t)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Claim(context.Background(), store.CurrentRevision(), envelope.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLaunch(context.Background(), store.CurrentRevision(), attempt); err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: "http://127.0.0.1:1", WorkerID: attempt.WorkerID, AuthToken: "fixture-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/repo/pull/9"
	head := strings.Repeat("a", 40)
	body, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
		Status: "completed", Summary: "Reviewed public result at " + prURL,
		PullRequestURL: prURL, CommitSHA: head,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var unsafeBody map[string]any
	if err := json.Unmarshal(body, &unsafeBody); err != nil {
		t.Fatal(err)
	}
	unsafeBody["authorization"] = "Bearer fixture-only"
	before := store.CurrentRevision()
	if _, err := store.CommitTerminal(context.Background(), attempt, testJSON(t, unsafeBody)); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("explicit credential-bearing terminal field = %v, want refusal", err)
	}
	if store.CurrentRevision() != before {
		t.Fatal("credential-bearing terminal body advanced revision")
	}
	for _, malformedURL := range []string{
		"https://evil.example/acme/repo/pull/9",
		"http://github.com/acme/repo/pull/9",
		"https://user@github.com/acme/repo/pull/9",
		"https://github.com/acme/repo/pull/9?view=1",
	} {
		malformedBody, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
			Status: "completed", Summary: "Reviewed public result at " + malformedURL,
			PullRequestURL: malformedURL, CommitSHA: head,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitTerminal(context.Background(), attempt, malformedBody); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("noncanonical terminal PR URL %q = %v, want refusal", malformedURL, err)
		}
		if store.CurrentRevision() != before {
			t.Fatalf("noncanonical terminal PR URL %q advanced revision", malformedURL)
		}
		if publications, err := store.PendingPublication(context.Background()); err != nil || len(publications) != 0 {
			t.Fatalf("noncanonical terminal PR URL %q created publication: %+v, %v", malformedURL, publications, err)
		}
	}
	if _, err := store.CommitTerminal(context.Background(), attempt, body); err != nil {
		t.Fatalf("real Poster terminal body: %v", err)
	}
	pub, err := store.PendingPublication(context.Background())
	if err != nil || len(pub) != 1 || pub[0].URL != prURL || pub[0].HeadSHA != head {
		t.Fatalf("real Poster PR/head expectation = %+v, %v", pub, err)
	}
	if err := store.CompletePublication(context.Background(), pub[0].Key, PublicationEvidence{
		URL: prURL, HeadSHA: head, ReadAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("real Poster publication readback: %v", err)
	}
}

func TestFailedPosterMayRetainCanonicalPRWithoutHead(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "queue"), testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	envelope := testEnvelope(t)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), envelope); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Claim(context.Background(), store.CurrentRevision(), envelope.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLaunch(context.Background(), store.CurrentRevision(), attempt); err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: "http://127.0.0.1:1", WorkerID: attempt.WorkerID, AuthToken: "fixture-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalidURL := range []string{
		"https://evil.example/acme/repo/pull/9",
		"https://github.com/acme/other/pull/9",
	} {
		body, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
			Status: "failed", Summary: "Partial work failed", PullRequestURL: invalidURL,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := store.CurrentRevision()
		if _, err := store.CommitTerminal(context.Background(), attempt, body); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("invalid failed-result PR %q = %v, want refusal", invalidURL, err)
		}
		if store.CurrentRevision() != before {
			t.Fatalf("invalid failed-result PR %q advanced revision", invalidURL)
		}
	}
	prURL := "https://github.com/acme/repo/pull/9"
	body, err := poster.PrepareTerminalStatusBody(context.Background(), attempt.SessionID, agent.Result{
		Status: "failed", Summary: "Partial work failed", PullRequestURL: prURL,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTerminal(context.Background(), attempt, body); err != nil {
		t.Fatalf("real failed Poster body with PR but no head: %v", err)
	}
	publication, err := store.PendingPublication(context.Background())
	if err != nil || len(publication) != 1 || publication[0].Operation != PublicationGitHubComment || publication[0].URL != "" || publication[0].HeadSHA != "" {
		t.Fatalf("failed result must request source comment, not PR publication: %+v, %v", publication, err)
	}
}

func TestSessionsNeedingRecoveryIsOrderedAndDefensive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "queue")
	store, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	denied := testEnvelopeFor(t, "session-denied", 7)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), denied); err != nil {
		t.Fatal(err)
	}
	deniedAttempt, err := store.Claim(context.Background(), store.CurrentRevision(), denied.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObservation(context.Background(), deniedAttempt, BoundedTypedObservation{
		Kind: ObservationPrestartDeny, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	unknown := testEnvelopeFor(t, "session-unknown", 8)
	if _, err := store.Admit(context.Background(), store.CurrentRevision(), unknown); err != nil {
		t.Fatal(err)
	}
	unknownAttempt, err := store.Claim(context.Background(), store.CurrentRevision(), unknown.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLaunch(context.Background(), store.CurrentRevision(), unknownAttempt); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := Open(root, testBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if pending, _ := reopened.PendingDispatch(context.Background()); len(pending) != 0 {
		t.Fatal("claimed/unknown sessions were requeued after restart")
	}
	recovery, err := reopened.SessionsNeedingRecovery(context.Background())
	if err != nil || len(recovery) != 2 || recovery[0].Admission.Envelope.Session.SessionID != "session-denied" ||
		recovery[1].Admission.Envelope.Session.SessionID != "session-unknown" ||
		recovery[0].Admission.DispatchState != DispatchClaimed ||
		recovery[1].Admission.DispatchState != DispatchUnknown ||
		len(recovery[0].Observations) != 1 || recovery[0].Observations[0].Kind != ObservationPrestartDeny {
		t.Fatalf("ordered recovery = %+v, %v", recovery, err)
	}
	recovery[0].Admission.Envelope.HostPreflight[0] = 'x'
	recovery[0].Observations[0].Kind = ObservationLiveness
	again, err := reopened.SessionsNeedingRecovery(context.Background())
	if err != nil || !bytes.Equal(again[0].Admission.Envelope.HostPreflight, denied.HostPreflight) ||
		again[0].Observations[0].Kind != ObservationPrestartDeny {
		t.Fatalf("caller mutation changed recovered authority: %v", err)
	}
	if err := reopened.ObserveLaunch(context.Background(), reopened.CurrentRevision(), unknownAttempt, OwnedProcessCorrelation{
		ProcessID: 123, ProcessStart: "birth-token", SessionDetailSHA256: testDigest("detail"),
	}); err != nil {
		t.Fatal(err)
	}
	launched, err := reopened.SessionsNeedingRecovery(context.Background())
	if err != nil || len(launched) != 2 || launched[1].Admission.DispatchState != DispatchLaunched {
		t.Fatalf("launched recovery = %+v, %v", launched, err)
	}
	if _, err := reopened.CommitTerminal(context.Background(), deniedAttempt, []byte(`{"workerId":"local-worker","status":"failed"}`)); err != nil {
		t.Fatal(err)
	}
	remaining, err := reopened.SessionsNeedingRecovery(context.Background())
	if err != nil || len(remaining) != 1 || remaining[0].Admission.Envelope.Session.SessionID != "session-unknown" {
		t.Fatalf("terminal recovery removal = %+v, %v", remaining, err)
	}
}

func TestSessionForSourceRetainsExactScopeAndProjectionAfterReopen(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "queue")
	binding := testBinding()
	store, err := Open(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	envelope := testEnvelope(t)
	record, err := store.Admit(context.Background(), store.CurrentRevision(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	revision := store.CurrentRevision()
	projection, err := store.SessionForSource(context.Background(), binding.ScopeID, envelope.Source)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Admission.Envelope.Session != record.Envelope.Session || projection.Admission.EnvelopeSHA256 != record.EnvelopeSHA256 {
		t.Fatal("source lookup changed canonical session")
	}
	projection.Admission.Envelope.Receipt[0] = 'x'
	projection.Admission.Envelope.ProducerEvidence[0] = 'x'
	again, err := store.SessionForSource(context.Background(), binding.ScopeID, envelope.Source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Admission.Envelope.Receipt, record.Envelope.Receipt) || !bytes.Equal(again.Admission.Envelope.ProducerEvidence, record.Envelope.ProducerEvidence) {
		t.Fatal("source lookup returned mutable store evidence")
	}
	if _, err = store.SessionForSource(context.Background(), "foreign-scope", envelope.Source); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("foreign scope source lookup = %v", err)
	}
	changed := envelope.Source
	changed.RepositoryID++
	if _, err = store.SessionForSource(context.Background(), binding.ScopeID, changed); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("foreign repository source lookup = %v", err)
	}
	changed = envelope.Source
	changed.OwnerRepo = "other/repo"
	changed.IssueURL = fmt.Sprintf("https://github.com/other/repo/issues/%d", changed.IssueNumber)
	if _, err = store.SessionForSource(context.Background(), binding.ScopeID, changed); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("changed source correlation lookup = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.SessionForSource(canceled, binding.ScopeID, envelope.Source); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup=%v", err)
	}
	if store.CurrentRevision() != revision {
		t.Fatal("read-only source lookup advanced journal")
	}
}

func TestUpdateSelectorsPreservesAuthorityAndFreezesTrustedInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	binding := testBinding()
	store, err := Open(filepath.Join(t.TempDir(), "queue"), binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	revision := store.CurrentRevision()
	denied := testBinding().Selectors
	denied.Models = []string{}
	if err = store.UpdateSelectors(ctx, denied); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Admit(ctx, revision, testEnvelope(t)); err == nil {
		t.Fatal("removed model remained admissible")
	}
	if store.CurrentRevision() != revision {
		t.Fatal("selector update or refused admission rewrote journal")
	}
	good := testBinding().Selectors
	if err = store.UpdateSelectors(ctx, good); err != nil {
		t.Fatal(err)
	}
	good.Models[0] = "other/model"
	good.HarnessVersions["codex"][0] = "changed"
	if err = store.UpdateSelectors(ctx, nil); err == nil {
		t.Fatal("nil registry replaced trusted input")
	}
	record, err := store.Admit(ctx, revision, testEnvelope(t))
	if err != nil {
		t.Fatalf("caller mutation or invalid update altered last good selectors: %v", err)
	}
	before := bytes.Clone(record.Envelope.OperationalPayload)
	if err = store.UpdateSelectors(ctx, denied); err != nil {
		t.Fatal(err)
	}
	projection, err := store.Session(ctx, binding.ScopeID, record.Envelope.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, projection.Admission.Envelope.OperationalPayload) || store.CurrentRevision() != record.Revision {
		t.Fatal("trusted selector change rewrote admitted authority")
	}
}
