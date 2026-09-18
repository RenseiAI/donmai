package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

const producerIntegrationCapability = "example.local-runtime-selection/v1"

type producerIntegrationProvider struct {
	inner      *dualSelectionRuntimeProvider
	spawnCalls atomic.Int32
}

func (p *producerIntegrationProvider) Name() agent.ProviderName { return p.inner.Name() }

func (p *producerIntegrationProvider) Capabilities() agent.Capabilities {
	return p.inner.Capabilities()
}

func (p *producerIntegrationProvider) Manifest() agent.HarnessManifest {
	return p.inner.Manifest()
}

func (p *producerIntegrationProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spawnCalls.Add(1)
	return p.inner.Spawn(ctx, spec)
}

func (p *producerIntegrationProvider) Resume(ctx context.Context, id string, spec agent.Spec) (agent.Handle, error) {
	return p.inner.Resume(ctx, id, spec)
}

func (p *producerIntegrationProvider) Shutdown(ctx context.Context) error {
	return p.inner.Shutdown(ctx)
}

type producerIntegrationComposition struct {
	registry     *Registry
	realizations *agent.CapabilityRealizationRegistry
	policy       ProtectedRuntimeMCPDualSelectionPolicy
	view         *ProviderView
	runner       *Runner
	provider     *producerIntegrationProvider
	v1           agent.CompiledCapabilityRealization
	v2           agent.CompiledCapabilityRealization
	tokenPath    string
	composeCalls atomic.Int32
	configCalls  atomic.Int32
}

func newProducerIntegrationComposition(t *testing.T) *producerIntegrationComposition {
	t.Helper()
	manifest := codexManifestForTest()
	baseProfile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("missing autonomous fixture profile")
	}
	v1Profile := baseProfile
	v1Profile.ID = "local/runtime-selection-v1"
	v2Profile := baseProfile
	v2Profile.ID = "local/runtime-selection-v2"
	v2Profile.EvidenceTier = "native_verified"
	v2Profile.ProductionEligible = true
	manifest.ToolLifecycle = append(manifest.ToolLifecycle, v1Profile, v2Profile)

	inner := &dualSelectionRuntimeProvider{manifest: manifest, specs: make(map[string]agent.Spec)}
	provider := &producerIntegrationProvider{inner: inner}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	seed := exactReceiptQueuedWork("local-runtime-selection-seed")
	seed.PlatformURL, seed.McpAuthToken = "https://local.invalid/", "local-fixture-bearer"
	v1 := protectedRuntimeMCPCompiledForProfile(t, producerIntegrationCapability, provider, seed, v1Profile.ID, agent.PromptModeAutonomous)
	v2 := protectedRuntimeMCPCompiledForProfile(t, producerIntegrationCapability, provider, seed, v2Profile.ID, agent.PromptModeAutonomous)
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	policy := ProtectedRuntimeMCPDualSelectionPolicy{
		V1: ProtectedRuntimeMCPSelector{
			CapabilityID: producerIntegrationCapability, HarnessID: agent.HarnessCodex,
			AdapterProfileID: v1Profile.ID, Mode: agent.PromptModeAutonomous,
		},
		V2: ProtectedRuntimeMCPV2Selector{
			CapabilityID: producerIntegrationCapability, HarnessID: agent.HarnessCodex,
			AdapterProfileID: v2Profile.ID, Mode: agent.PromptModeAutonomous,
			ConfigRequirementID: "example.local-session-config/v1",
		},
	}
	composition := &producerIntegrationComposition{
		registry: registry, realizations: realizations, policy: policy,
		provider: provider, v1: v1, v2: v2,
	}
	view, err := NewProviderViewWithOptions(registry, ProviderViewOptions{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPDualSelectionPolicy: policy,
		ConfigRequirements: func(ExecutionPreflightConfigRequirementContext) ([]executioncell.PreflightConfigRequirementV1, error) {
			composition.configCalls.Add(1)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	composition.view = view
	composition.tokenPath = filepath.Join(t.TempDir(), "local-runtime-token")
	if err := os.WriteFile(composition.tokenPath, []byte(seed.McpAuthToken), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(mcpGatewayTokenFileEnv, composition.tokenPath)

	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: server.URL, WorkerID: "worker_test", AuthToken: "worker-token",
		HTTPClient: server.Client(), BaseDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	composition.runner, err = New(Options{
		Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(),
		CapabilityRealizations: realizations, ProtectedRuntimeMCPDualSelectionPolicy: policy,
		SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return composition
}

func producerIntegrationTarget(compiled agent.CompiledCapabilityRealization) CapabilityRealizationSelectionTarget {
	return CapabilityRealizationSelectionTarget{
		CapabilityID: compiled.Declaration.CapabilityID, HarnessID: compiled.Declaration.HarnessID,
		AdapterVersion: compiled.Declaration.AdapterVersion, Mode: compiled.Declaration.Mode,
	}
}

func (c *producerIntegrationComposition) compose(t *testing.T, qw QueuedWork, compiled agent.CompiledCapabilityRealization) []byte {
	t.Helper()
	input, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	c.composeCalls.Add(1)
	produced, err := ComposeCapabilityRealizationSelectionOperationalPayload(input, c.realizations, producerIntegrationTarget(compiled))
	if err != nil {
		t.Fatal(err)
	}
	inputHash := sha256.Sum256(input)
	outputHash := sha256.Sum256(produced)
	t.Logf("session=%s input_sha256=%s output_sha256=%s profile=%s", qw.SessionID, hex.EncodeToString(inputHash[:]), hex.EncodeToString(outputHash[:]), compiled.Declaration.AdapterVersion)
	return produced
}

func admitProducerIntegrationWork(t *testing.T, qw QueuedWork, raw []byte) QueuedWork {
	t.Helper()
	profile := executioncell.LegacyResolvedProfile{
		Harness: "codex", Provider: "openai", Model: "gpt-test",
		ServingHost: "openai-direct", AuthMode: "test-key",
	}
	cellTemplate := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: producerIntegrationCapability}})
	adapted, err := executioncell.AdaptQueuedWorkJSON(raw, profile, executioncell.LegacyAdapterContext{
		RequestID: qw.SessionID,
		HarnessRefsByLegacyID: map[string]executioncell.HarnessRef{
			"codex": cellTemplate.Harness,
		},
		ModelAuthorsByProvider: map[string]string{"openai": cellTemplate.Model.Author},
		EndpointsByServingHost: map[string]executioncell.ServingEndpointRef{
			"openai-direct": cellTemplate.Endpoint,
		},
		AuthBindingsByMode: map[string]executioncell.AuthBindingRef{
			"test-key": cellTemplate.AuthBinding,
		},
		Placement:            cellTemplate.Placement,
		RequiredCapabilities: []executioncell.CapabilityRequest{{Name: producerIntegrationCapability}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(adapted.OperationalPayload, raw) {
		t.Fatal("legacy adaptation did not retain the produced bytes")
	}
	intentDigest, err := executioncell.DigestContractValue(adapted.Intent)
	if err != nil {
		t.Fatal(err)
	}
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         *adapted.Intent.Harness,
		Model:           adapted.Intent.Model,
		Endpoint:        *adapted.Intent.Endpoint,
		AuthBinding:     *adapted.Intent.AuthBinding,
		Placement:       *adapted.Intent.Placement,
		SessionMode:     adapted.Intent.SessionMode,
		GrantedCapabilities: []executioncell.CapabilityRequirement{{
			Name: producerIntegrationCapability,
		}},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
	receiptBytes, err := json.Marshal(executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion,
		ReceiptID:       "admission_" + qw.SessionID, RequestID: qw.SessionID,
		Decision: executioncell.AdmissionAdmitted, IntentDigest: intentDigest,
		OperationalPayloadDigest: adapted.OperationalPayloadDigest,
		Cell:                     &cell, ResolverDecisions: adapted.ResolverDecisions,
		RecordedAt: "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(receiptBytes)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executioncell.DecodeResolvedExecutionCell(effective); err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = bytes.Clone(raw)
	qw.AdmissionReceipt = immutable.Bytes()
	qw.EffectiveCell = effective
	qw.WorkerID = "worker_test"
	bindingBytes, err := json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       qw.SessionID, WorkerID: qw.WorkerID, PlacementID: cell.Placement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executioncell.DecodeRuntimeBinding(bindingBytes); err != nil {
		t.Fatal(err)
	}
	qw.ExecutionRuntimeBinding = bindingBytes
	qw.HostAdaptationReceipt = nil
	return qw
}

func producerIntegrationDetail(t *testing.T, qw QueuedWork) json.RawMessage {
	t.Helper()
	_, detail := protectedRuntimeMCPPreflightInput(t, qw)
	return detail
}

func (c *producerIntegrationComposition) prepare(
	t *testing.T,
	compiled agent.CompiledCapabilityRealization,
	sessionID string,
) dualSelectionRuntimeWork {
	t.Helper()
	qw := exactReceiptQueuedWork(sessionID)
	qw.IssueIdentifier = "LOCAL-RUNTIME-SELECTION"
	qw.Body = "exercise retained local realization selection"
	qw.PlatformURL = "https://local.invalid/"
	qw.McpAuthToken = "local-fixture-bearer"
	qw.Skills = []prompt.SkillSpec{{ID: "local-runtime-skill", Body: "per-run skill: " + sessionID}}
	produced := c.compose(t, qw, compiled)
	qw = admitProducerIntegrationWork(t, qw, produced)
	detail := producerIntegrationDetail(t, qw)
	receipt, err := c.view.PreflightExecution(detail)
	if err != nil {
		t.Fatalf("PreflightExecution(%s): %v", sessionID, err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var toolReceipt agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &toolReceipt); err != nil {
		t.Fatal(err)
	}
	if toolReceipt.ProfileID != compiled.Declaration.AdapterVersion {
		t.Fatalf("host profile for %s = %q, want %q", sessionID, toolReceipt.ProfileID, compiled.Declaration.AdapterVersion)
	}
	beforeConfig := c.configCalls.Load()
	if requirements, err := c.view.ResolveExecutionPreflightConfigRequirements(detail, receipt); err != nil || requirements != nil {
		t.Fatalf("common config requirements for %s = %+v err=%v", sessionID, requirements, err)
	}
	if c.configCalls.Load() != beforeConfig+1 {
		t.Fatalf("common config resolver calls for %s did not advance exactly once", sessionID)
	}

	runtimeWork := dualSelectionRuntimeWork{
		profileID: compiled.Declaration.AdapterVersion,
		skillBody: qw.Skills[0].Body,
	}
	if compiled.Declaration.AdapterVersion == c.policy.V1.AdapterProfileID {
		requirements, err := c.view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detail, receipt)
		if err != nil || len(requirements) != 1 {
			t.Fatalf("V1 requirements for %s = %+v err=%v", sessionID, requirements, err)
		}
		qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterialization(t, receipt, requirements[0])
		runtimeWork.serverName = requirements[0].ServerName
	} else {
		requirements, err := c.view.ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detail, receipt)
		if err != nil || len(requirements) != 1 {
			t.Fatalf("V2 requirements for %s = %+v err=%v", sessionID, requirements, err)
		}
		qw.HostAdaptationReceipt = attachProtectedRuntimeMCPMaterializationV2(t, receipt, requirements[0], c.tokenPath, qw.McpAuthToken)
		runtimeWork.serverName = requirements[0].ServerName
		runtimeWork.v2 = true
	}
	bound, admission, err := c.registry.PreflightHarnessWithProtectedRuntimeMCPSelection(
		qw, c.realizations, ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, c.policy,
	)
	if err != nil {
		t.Fatalf("child preflight for %s: %v", sessionID, err)
	}
	if bound.toolLifecycleProfileID != runtimeWork.profileID {
		t.Fatalf("child profile for %s = %q, want %q", sessionID, bound.toolLifecycleProfileID, runtimeWork.profileID)
	}
	runtimeWork.work, runtimeWork.admission = bound, admission
	return runtimeWork
}

func (c *producerIntegrationComposition) run(t *testing.T, runtimeWork dualSelectionRuntimeWork) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := c.runner.RunAdmitted(ctx, runtimeWork.work, runtimeWork.admission)
	if !errors.Is(err, errDualSelectionRuntimeSpawn) || got == nil || got.Status != "failed" {
		t.Fatalf("RunAdmitted(%s) result=%+v err=%v", runtimeWork.work.SessionID, got, err)
	}
	assertDualSelectionRuntimeSpawn(t, c.provider.inner, runtimeWork, c.tokenPath)
}

func mutateProducerIntegrationSelection(t *testing.T, raw []byte, field string, value any) []byte {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	selection, ok := payload["capabilityRealizationSelection"].(map[string]any)
	if !ok {
		t.Fatal("produced payload has no selection object")
	}
	selection[field] = value
	changed, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestStandaloneSelectionProducerToConsumerRuntimeAcceptance(t *testing.T) {
	composition := newProducerIntegrationComposition(t)

	sequential := []dualSelectionRuntimeWork{
		composition.prepare(t, composition.v1, "local-selection-sequential-v1"),
		composition.prepare(t, composition.v2, "local-selection-sequential-v2"),
	}
	for _, runtimeWork := range sequential {
		composition.run(t, runtimeWork)
	}

	replay := sequential[0]
	replayDigest, err := DigestOperationalPayload(replay.work)
	if err != nil {
		t.Fatal(err)
	}
	replaySelection, err := executioncell.ExtractCapabilityRealizationSelectionV1(replay.work.OperationalPayload)
	if err != nil || replaySelection == nil {
		t.Fatalf("retained replay selection=%+v err=%v", replaySelection, err)
	}
	composeCallsBeforeReplay := composition.composeCalls.Load()
	replayDetail := producerIntegrationDetail(t, replay.work)
	if err := composition.view.ValidateRetainedExecution(replayDetail, replay.work.HostAdaptationReceipt); err != nil {
		t.Fatalf("retained replay validation: %v", err)
	}
	boundReplay, replayAdmission, err := composition.registry.PreflightHarnessWithProtectedRuntimeMCPSelection(
		replay.work, composition.realizations, ProtectedRuntimeMCPSelector{}, ProtectedRuntimeMCPV2Selector{}, composition.policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay.work, replay.admission = boundReplay, replayAdmission
	composition.run(t, replay)
	if composition.composeCalls.Load() != composeCallsBeforeReplay {
		t.Fatal("retained replay called the producer again")
	}
	replayedDigest, err := DigestOperationalPayload(replay.work)
	if err != nil {
		t.Fatal(err)
	}
	if replayedDigest != replayDigest || replaySelection.AdapterVersion != replay.profileID {
		t.Fatalf("retained replay digest/profile = %s/%s, want %s/%s", replayedDigest, replaySelection.AdapterVersion, replayDigest, replay.profileID)
	}
	t.Logf("retained_replay digest=%s profile=%s compose_calls=%d", replayedDigest, replay.profileID, composition.composeCalls.Load())

	concurrent := []dualSelectionRuntimeWork{
		composition.prepare(t, composition.v1, "local-selection-concurrent-v1"),
		composition.prepare(t, composition.v2, "local-selection-concurrent-v2"),
	}
	errs := make(chan error, len(concurrent))
	var wg sync.WaitGroup
	for _, runtimeWork := range concurrent {
		wg.Add(1)
		go func(runtimeWork dualSelectionRuntimeWork) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, runErr := composition.runner.RunAdmitted(ctx, runtimeWork.work, runtimeWork.admission)
			if !errors.Is(runErr, errDualSelectionRuntimeSpawn) || got == nil || got.Status != "failed" {
				errs <- errors.Join(errors.New("concurrent RunAdmitted did not reach controlled spawn"), runErr)
				return
			}
			errs <- nil
		}(runtimeWork)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, runtimeWork := range concurrent {
		assertDualSelectionRuntimeSpawn(t, composition.provider.inner, runtimeWork, composition.tokenPath)
	}

	t.Run("post-receipt raw digest tamper refuses before config and cached admission", func(t *testing.T) {
		runtimeWork := composition.prepare(t, composition.v1, "local-selection-tampered-digest")
		configBefore := composition.configCalls.Load()
		spawnBefore := composition.provider.spawnCalls.Load()
		runtimeWork.work.OperationalPayload = mutateProducerIntegrationSelection(
			t, runtimeWork.work.OperationalPayload, "adapterVersion", composition.v2.Declaration.AdapterVersion,
		)
		detail := producerIntegrationDetail(t, runtimeWork.work)
		if _, err := composition.view.ResolveExecutionPreflightConfigRequirements(detail, runtimeWork.work.HostAdaptationReceipt); err == nil || !strings.Contains(err.Error(), "operational payload digest mismatch") {
			t.Fatalf("post-receipt digest tamper error=%v", err)
		}
		got, err := composition.runner.RunAdmitted(t.Context(), runtimeWork.work, runtimeWork.admission)
		if err == nil || got != nil || !strings.Contains(err.Error(), "operational payload digest mismatch") {
			t.Fatalf("cached admission digest tamper result=%+v err=%v", got, err)
		}
		if runtimeWork.admission.consumed.Load() || composition.configCalls.Load() != configBefore || composition.provider.spawnCalls.Load() != spawnBefore {
			t.Fatalf("tamper effects consumed=%t config=%d/%d spawn=%d/%d", runtimeWork.admission.consumed.Load(), composition.configCalls.Load(), configBefore, composition.provider.spawnCalls.Load(), spawnBefore)
		}
	})

	t.Run("post-receipt re-admission cannot reuse host authority", func(t *testing.T) {
		runtimeWork := composition.prepare(t, composition.v1, "local-selection-tampered-evidence")
		configBefore := composition.configCalls.Load()
		spawnBefore := composition.provider.spawnCalls.Load()
		retainedHostReceipt := bytes.Clone(runtimeWork.work.HostAdaptationReceipt)
		changed := mutateProducerIntegrationSelection(t, runtimeWork.work.OperationalPayload, "recipeDigest", strings.Repeat("0", 64))
		runtimeWork.work = admitProducerIntegrationWork(t, runtimeWork.work, changed)
		runtimeWork.work.HostAdaptationReceipt = retainedHostReceipt
		detail := producerIntegrationDetail(t, runtimeWork.work)
		if _, err := composition.view.ResolveExecutionPreflightConfigRequirements(detail, retainedHostReceipt); err == nil || !strings.Contains(err.Error(), "incomplete prepared harness authority") {
			t.Fatalf("post-receipt host authority tamper error=%v", err)
		}
		got, err := composition.runner.RunAdmitted(t.Context(), runtimeWork.work, runtimeWork.admission)
		if err == nil || got != nil || !strings.Contains(err.Error(), "incomplete prepared harness authority") {
			t.Fatalf("cached admission host authority tamper result=%+v err=%v", got, err)
		}
		if runtimeWork.admission.consumed.Load() || composition.configCalls.Load() != configBefore || composition.provider.spawnCalls.Load() != spawnBefore {
			t.Fatalf("tamper effects consumed=%t config=%d/%d spawn=%d/%d", runtimeWork.admission.consumed.Load(), composition.configCalls.Load(), configBefore, composition.provider.spawnCalls.Load(), spawnBefore)
		}
	})

	t.Logf("profiles=%s,%s compose_calls=%d config_calls=%d spawn_calls=%d", composition.v1.Declaration.AdapterVersion, composition.v2.Declaration.AdapterVersion, composition.composeCalls.Load(), composition.configCalls.Load(), composition.provider.spawnCalls.Load())
}
