package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

type orderedPreflightProvider struct {
	mu         *sync.Mutex
	order      *[]string
	receipt    json.RawMessage
	calls      atomic.Int32
	failReplay bool
}

func (p *orderedPreflightProvider) Names() []string { return []string{"stub"} }
func (p *orderedPreflightProvider) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

func (p *orderedPreflightProvider) PreflightExecution(json.RawMessage) (json.RawMessage, error) {
	call := p.calls.Add(1)
	p.mu.Lock()
	*p.order = append(*p.order, "compile")
	p.mu.Unlock()
	if p.failReplay && call > 1 {
		return nil, errors.New("compiler changed after retained receipt")
	}
	return slices.Clone(p.receipt), nil
}

type orderedReplayStore struct {
	mu    *sync.Mutex
	order *[]string
	store *FileExecutionPreflightStore
}

func (s *orderedReplayStore) Persist(sessionID string, receipt json.RawMessage) error {
	err := s.store.Persist(sessionID, receipt)
	if err == nil {
		s.mu.Lock()
		*s.order = append(*s.order, "fsync")
		s.mu.Unlock()
	}
	return err
}

func (s *orderedReplayStore) Load(sessionID string) (json.RawMessage, error) {
	return s.store.Load(sessionID)
}

func (s *orderedReplayStore) BeginExecutionPreflightStart(ctx context.Context, request executioncell.PreflightRegistrationRequest, response executioncell.PreflightRegistrationResponse) error {
	return s.store.BeginExecutionPreflightStart(ctx, request, response)
}

func (s *orderedReplayStore) RetireExecutionPreflight(ctx context.Context, requestID string) error {
	return s.store.RetireExecutionPreflight(ctx, requestID)
}

type orderedRegistrar struct {
	mu       *sync.Mutex
	order    *[]string
	calls    atomic.Int32
	response func(executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error)
}

type losingFileRegistrar struct {
	registrar *FileExecutionPreflightRegistrar
	lost      atomic.Bool
}

func (r *losingFileRegistrar) RegisterExecutionPreflight(ctx context.Context, request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	response, err := r.registrar.RegisterExecutionPreflight(ctx, request)
	if err == nil && response.Decision == "authorized" && !r.lost.Swap(true) {
		return executioncell.PreflightRegistrationResponse{}, errors.New("simulated lost local acknowledgement")
	}
	return response, err
}

func (r *losingFileRegistrar) admitExecutionPreflight(ctx context.Context, admission localExecutionPreflightAdmission) error {
	return r.registrar.admitExecutionPreflight(ctx, admission)
}

func (r *losingFileRegistrar) beginExecutionPreflightStart(ctx context.Context, request executioncell.PreflightRegistrationRequest, response executioncell.PreflightRegistrationResponse) error {
	return r.registrar.beginExecutionPreflightStart(ctx, request, response)
}

func (r *losingFileRegistrar) retireExecutionPreflight(ctx context.Context, requestID string) error {
	return r.registrar.retireExecutionPreflight(ctx, requestID)
}

func (r *orderedRegistrar) RegisterExecutionPreflight(_ context.Context, request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	r.calls.Add(1)
	r.mu.Lock()
	*r.order = append(*r.order, "register")
	r.mu.Unlock()
	return r.response(request)
}

func authorizedRegistration(request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	binding := request.RuntimeBinding
	return executioncell.PreflightRegistrationResponse{
		ContractVersion: executioncell.PreflightRegistrationContractVersion,
		Decision:        "authorized", RegistrationID: "registration-1",
		ReceiptSHA256: request.ReceiptSHA256, RuntimeBinding: &binding,
		AuthorizationRevision: 1,
	}, nil
}

func localAdmissionFor(request executioncell.PreflightRegistrationRequest) localExecutionPreflightAdmission {
	return localExecutionPreflightAdmission{
		ContractVersion: localExecutionPreflightAuthorityVersion, RuntimeBinding: request.RuntimeBinding,
		OperationalPayloadDigest: request.OperationalPayloadDigest,
		AdmissionReceiptSHA256:   strings.Repeat("b", 64), EffectiveCellSHA256: strings.Repeat("c", 64),
		AuthorizationRevision: 1,
	}
}

func readyPreflightReceipt(t *testing.T, binding executioncell.RuntimeBinding, operationalDigest string) json.RawMessage {
	t.Helper()
	claimReceiptID := ""
	if binding.ClaimID != "" {
		claimReceiptID = "claim-receipt"
	}
	promptReceipt := agent.PromptDeliveryReceipt{ContractVersion: agent.PromptContractVersion, ProfileID: "test-prompt", Decision: "ready", Entries: []agent.PromptDeliveryEntry{}}
	toolReceipt := agent.ToolLifecycleReceipt{ContractVersion: agent.ToolLifecycleContractVersion, AdmissionReceiptID: "admission-v2", ClaimReceiptID: claimReceiptID, OperationalPayloadDigest: operationalDigest, ProfileID: "test-tools", Decision: "ready", EvidenceTier: string(agent.EvidenceStructured), ProductionEligible: true, Entries: []agent.ToolLifecycleEntry{}}
	channels := []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"}
	materializations := make([]agent.HarnessMaterialization, 0, len(channels))
	for _, channel := range channels {
		materializations = append(materializations, agent.HarnessMaterialization{Channel: channel, SourceDigest: operationalDigest, Required: true})
	}
	prepared := agent.PreparedHarness{ContractVersion: agent.HarnessAdaptationContractVersion, Harness: "codex", Mode: agent.PromptModeAutonomous, OperationalPayloadDigest: operationalDigest, AuthorityDigest: strings.Repeat("f", 64), RuntimeMCPNames: []string{}, Materializations: materializations, PromptReceipt: promptReceipt, ToolLifecycleReceipt: toolReceipt}
	plan := rawJSON(t, prepared)
	digest := sha256.Sum256(plan)
	return rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       binding.RequestID, WorkerID: binding.WorkerID,
		PlacementID: binding.PlacementID, ClaimID: binding.ClaimID,
		Decision: "ready", Plan: plan, PlanDigest: hex.EncodeToString(digest[:]),
		PromptReceipt:        rawJSON(t, promptReceipt),
		ToolLifecycleReceipt: rawJSON(t, toolReceipt),
	})
}

func operationalDigestFor(t *testing.T, detail *SessionDetail) string {
	t.Helper()
	digest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func v2Detail(t *testing.T) (*SessionDetail, executioncell.RuntimeBinding) {
	t.Helper()
	cell := daemonExecutionCell()
	operationalPayload := json.RawMessage(`{"empty":false,"items":[]}`)
	operationalDigest, err := executioncell.DigestOperationalPayload(operationalPayload)
	if err != nil {
		t.Fatal(err)
	}
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-v2", WorkerID: "worker-local", PlacementID: cell.Placement.ID,
		PreflightRegistration: &executioncell.PreflightRegistrationRef{
			ContractVersion: executioncell.PreflightRegistrationContractVersion,
			Required:        true, ChallengeID: "challenge-v2",
		},
	}
	admission := executioncell.AdmissionReceipt{
		ContractVersion: executioncell.ContractVersion, ReceiptID: "admission-v2", RequestID: binding.RequestID,
		Decision: executioncell.AdmissionAdmitted, Cell: &cell, IntentDigest: strings.Repeat("e", 64),
		OperationalPayloadDigest: operationalDigest, ResolverDecisions: []executioncell.ResolverDecision{}, RecordedAt: "2026-09-09T12:00:00Z",
	}
	return &SessionDetail{
		SessionID: binding.RequestID, WorkerID: binding.WorkerID,
		AdmissionReceipt: rawJSON(t, admission),
		EffectiveCell:    rawJSON(t, cell), ExecutionRuntimeBinding: rawJSON(t, binding),
		OperationalPayload: operationalPayload,
	}, binding
}

func startV2Daemon(t *testing.T, provider ProviderRegistry, store ExecutionPreflightStore, registrar ExecutionPreflightRegistrar, order *[]string, mu *sync.Mutex, credentials *atomic.Int32, marker string) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "daemon.yaml")
	if err := WriteConfig(configPath, &Config{
		APIVersion: "donmai.dev/v1", Kind: "LocalDaemon",
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeAllRouted,
		Machine:                 MachineConfig{ID: "preflight-registration-machine"},
		Orchestrator:            OrchestratorConfig{URL: "https://example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	d := New(Options{
		ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider,
		ExecutionPreflightStore: store, ExecutionPreflightRegistrar: registrar,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker},
			OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
				credentials.Add(1)
				mu.Lock()
				*order = append(*order, "credential")
				mu.Unlock()
				return env, nil
			},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

func TestRuntimeBindingV2RegistersAfterFsyncBeforeCredentialAndSpawn(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	detail, binding := v2Detail(t)
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail))}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	registrar := &orderedRegistrar{mu: &mu, order: &order, response: authorizedRegistration}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err != nil {
		t.Fatal(err)
	}
	if credentials.Load() != 1 {
		t.Fatalf("credential hook calls = %d", credentials.Load())
	}
	mu.Lock()
	gotOrder := slices.Clone(order)
	mu.Unlock()
	if strings.Join(gotOrder, ",") != "compile,fsync,register,credential" {
		t.Fatalf("effect order = %v", gotOrder)
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("spawn marker absent: %v", err)
		}
	}
}

func TestRuntimeBindingV2TwoContendersStartAtMostOnce(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	firstDetail, binding := v2Detail(t)
	secondDetail, _ := v2Detail(t)
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, firstDetail))}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	registrar := &orderedRegistrar{mu: &mu, order: &order, response: authorizedRegistration}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	results := make(chan error, 2)
	for _, detail := range []*SessionDetail{firstDetail, secondDetail} {
		detail := detail
		go func() {
			_, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail)
			results <- err
		}()
	}
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 || credentials.Load() != 1 {
		t.Fatalf("successful accepts=%d credential calls=%d", successes, credentials.Load())
	}
}

func TestRuntimeBindingV2FileRegistrarConsumesCurrentLocalAuthority(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	detail, binding := v2Detail(t)
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail))}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	dir := t.TempDir()
	registrar := NewFileExecutionPreflightRegistrar(dir)
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err != nil {
		t.Fatal(err)
	}
	retry, _ := v2Detail(t)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: retry.SessionID, ProjectID: "project-v2"}, retry); err == nil {
		t.Fatal("consumed local start authority was reused")
	}
	if provider.calls.Load() != 1 || credentials.Load() != 1 {
		t.Fatalf("compiler calls=%d credential calls=%d, want one admitted start", provider.calls.Load(), credentials.Load())
	}
}

func TestRuntimeBindingV2RefusalStopsCredentialAndSpawn(t *testing.T) {
	cases := map[string]func(executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error){
		"transport error": func(executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
			return executioncell.PreflightRegistrationResponse{}, errors.New("ack lost")
		},
		"refused": func(executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
			return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightClaimRetired}, nil
		},
		"forged acknowledgement": func(request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
			response, _ := authorizedRegistration(request)
			response.ReceiptSHA256 = strings.Repeat("f", 64)
			return response, nil
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			order := []string{}
			detail, binding := v2Detail(t)
			provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail))}
			store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
			registrar := &orderedRegistrar{mu: &mu, order: &order, response: response}
			var credentials atomic.Int32
			marker := filepath.Join(t.TempDir(), "spawned")
			d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
			if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err == nil {
				t.Fatal("v2 refusal was accepted")
			}
			if credentials.Load() != 0 {
				t.Fatalf("credential hook calls = %d", credentials.Load())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("spawn marker exists: %v", err)
			}
		})
	}
}

func TestRuntimeBindingV2MalformedNestedReceiptStopsRegistrationCredentialAndSpawn(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	detail, binding := v2Detail(t)
	var malformed executioncell.HostAdaptationReceipt
	if err := json.Unmarshal(readyPreflightReceipt(t, binding, operationalDigestFor(t, detail)), &malformed); err != nil {
		t.Fatal(err)
	}
	malformed.Plan = json.RawMessage(`{"decision":"denied","unexpected":true}`)
	planDigest := sha256.Sum256(malformed.Plan)
	malformed.PlanDigest = hex.EncodeToString(planDigest[:])
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: rawJSON(t, malformed)}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	registrar := &orderedRegistrar{mu: &mu, order: &order, response: authorizedRegistration}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err == nil {
		t.Fatal("malformed nested receipt was accepted")
	}
	if registrar.calls.Load() != 0 || credentials.Load() != 0 {
		t.Fatalf("registrar calls=%d credential calls=%d", registrar.calls.Load(), credentials.Load())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spawn marker exists: %v", err)
	}
}

func TestRuntimeBindingV2LostAcknowledgementReplaysRetainedReceiptWithoutCompilation(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	detail, binding := v2Detail(t)
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail)), failReplay: true}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	var registrations atomic.Int32
	registrar := &orderedRegistrar{mu: &mu, order: &order, response: func(request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
		if registrations.Add(1) == 1 {
			return executioncell.PreflightRegistrationResponse{}, errors.New("ack lost")
		}
		return authorizedRegistration(request)
	}}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err == nil {
		t.Fatal("lost acknowledgement was accepted")
	}
	retry, _ := v2Detail(t)
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: retry.SessionID, ProjectID: "project-v2"}, retry); err != nil {
		t.Fatalf("retained-receipt retry: %v", err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("compiler calls = %d, want retained receipt replay without recompilation", provider.calls.Load())
	}
}

func TestRuntimeBindingV2LostLocalAcknowledgementReplaysAfterDaemonRestart(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	detail, binding := v2Detail(t)
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail)), failReplay: true}
	receiptDir, registrationDir := t.TempDir(), t.TempDir()
	firstStore := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(receiptDir)}
	firstRegistrar := &losingFileRegistrar{registrar: NewFileExecutionPreflightRegistrar(registrationDir)}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	firstDaemon := startV2Daemon(t, provider, firstStore, firstRegistrar, &order, &mu, &credentials, marker)
	if _, err := firstDaemon.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err == nil {
		t.Fatal("lost local acknowledgement was accepted")
	}
	if credentials.Load() != 0 {
		t.Fatalf("credential hook calls before restart = %d", credentials.Load())
	}
	if err := firstDaemon.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	secondStore := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(receiptDir)}
	secondRegistrar := NewFileExecutionPreflightRegistrar(registrationDir)
	secondDaemon := startV2Daemon(t, provider, secondStore, secondRegistrar, &order, &mu, &credentials, marker)
	retry, _ := v2Detail(t)
	if _, err := secondDaemon.AcceptWorkWithDetail(SessionSpec{SessionID: retry.SessionID, ProjectID: "project-v2"}, retry); err != nil {
		t.Fatalf("restart retained-receipt retry: %v", err)
	}
	if provider.calls.Load() != 1 || credentials.Load() != 1 {
		t.Fatalf("compiler calls=%d credential calls=%d after restart", provider.calls.Load(), credentials.Load())
	}
}

func TestRuntimeBindingV2RequiresRegistrarAndReplayStore(t *testing.T) {
	for name, configure := range map[string]func(*Options){
		"missing registrar": func(options *Options) {
			options.ExecutionPreflightRegistrar = nil
		},
		"non-replayable store": func(options *Options) {
			options.ExecutionPreflightStore = &countingExecutionStore{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			order := []string{}
			detail, binding := v2Detail(t)
			provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, operationalDigestFor(t, detail))}
			store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
			registrar := &orderedRegistrar{mu: &mu, order: &order, response: authorizedRegistration}
			var credentials atomic.Int32
			marker := filepath.Join(t.TempDir(), "spawned")
			tmp := t.TempDir()
			configPath := filepath.Join(tmp, "daemon.yaml")
			if err := WriteConfig(configPath, &Config{APIVersion: "donmai.dev/v1", Kind: "LocalDaemon", ProjectAdmissionVersion: ProjectAdmissionVersionV2, ProjectAdmissionMode: ProjectAdmissionModeAllRouted, Machine: MachineConfig{ID: "machine"}, Orchestrator: OrchestratorConfig{URL: "https://example.test"}}); err != nil {
				t.Fatal(err)
			}
			options := Options{
				ConfigPath: configPath, JWTPath: filepath.Join(tmp, "daemon.jwt"),
				SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider,
				ExecutionPreflightStore: store, ExecutionPreflightRegistrar: registrar,
				SpawnerOptions: SpawnerOptions{WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker}, OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) { credentials.Add(1); return env, nil }},
			}
			configure(&options)
			d := New(options)
			if err := d.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Stop(context.Background()) })
			if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v2"}, detail); err == nil {
				t.Fatal("incomplete v2 configuration was accepted")
			}
			if credentials.Load() != 0 {
				t.Fatalf("credential hook calls = %d", credentials.Load())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("spawn marker exists: %v", err)
			}
		})
	}
}

func TestRuntimeBindingV1DoesNotInvokeV2Registrar(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	cell := daemonExecutionCell()
	binding := executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-v1", WorkerID: "worker-local", PlacementID: cell.Placement.ID}
	provider := &orderedPreflightProvider{mu: &mu, order: &order, receipt: readyPreflightReceipt(t, binding, strings.Repeat("a", 64))}
	store := &orderedReplayStore{mu: &mu, order: &order, store: NewFileExecutionPreflightStore(t.TempDir())}
	registrar := &orderedRegistrar{mu: &mu, order: &order, response: func(executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
		return executioncell.PreflightRegistrationResponse{}, errors.New("v1 must not call registrar")
	}}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startV2Daemon(t, provider, store, registrar, &order, &mu, &credentials, marker)
	detail := &SessionDetail{SessionID: binding.RequestID, WorkerID: binding.WorkerID, AdmissionReceipt: json.RawMessage(`{"valid":"admission"}`), EffectiveCell: rawJSON(t, cell), ExecutionRuntimeBinding: rawJSON(t, binding)}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID, ProjectID: "project-v1"}, detail); err != nil {
		t.Fatal(err)
	}
	if registrar.calls.Load() != 0 || credentials.Load() != 1 {
		t.Fatalf("registrar=%d credential=%d", registrar.calls.Load(), credentials.Load())
	}
}

func TestPersistExecutionPreflightV2ReaffirmsExactBytes(t *testing.T) {
	store := NewFileExecutionPreflightStore(t.TempDir())
	binding := executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "session-1", WorkerID: "worker", PlacementID: "host"}
	receipt := readyPreflightReceipt(t, binding, strings.Repeat("a", 64))
	if _, err := persistExecutionPreflightV2(store, "session-1", receipt); err != nil {
		t.Fatal(err)
	}
	got, err := persistExecutionPreflightV2(store, "session-1", slices.Clone(receipt))
	if err != nil || string(got) != string(receipt) {
		t.Fatalf("exact replay = %s, %v", got, err)
	}
	changed := slices.Clone(receipt)
	changed = append(changed[:len(changed)-1], ' ', '}')
	if _, err := persistExecutionPreflightV2(store, "session-1", changed); err == nil {
		t.Fatal("changed receipt replay was accepted")
	}
}

func TestPersistExecutionPreflightV2ReaffirmsDirectoryAfterPublishedSyncFailure(t *testing.T) {
	store := NewFileExecutionPreflightStore(t.TempDir())
	var syncCalls atomic.Int32
	store.syncRoot = func(root *os.Root) error {
		if syncCalls.Add(1) == 1 {
			return errors.New("simulated directory sync failure after publish")
		}
		return syncExecutionPreflightRoot(root)
	}
	binding := executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingV2ContractVersion, RequestID: "session-sync-retry", WorkerID: "worker", PlacementID: "host", PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"}}
	receipt := readyPreflightReceipt(t, binding, strings.Repeat("a", 64))
	if _, err := persistExecutionPreflightV2(store, binding.RequestID, receipt); err == nil {
		t.Fatal("published receipt with failed directory sync was accepted")
	}
	got, err := persistExecutionPreflightV2(store, binding.RequestID, receipt)
	if err != nil || !slices.Equal(got, receipt) {
		t.Fatalf("reaffirmed receipt = %s, %v", got, err)
	}
	if syncCalls.Load() != 2 {
		t.Fatalf("directory sync calls = %d, want publish failure plus replay reaffirmation", syncCalls.Load())
	}
}

func TestFileExecutionPreflightRegistrarPersistsAndReplaysExactRequest(t *testing.T) {
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-local", WorkerID: "worker", PlacementID: "host",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"},
	}
	request, err := executioncell.NewPreflightRegistrationRequest(binding, readyPreflightReceipt(t, binding, strings.Repeat("a", 64)), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	registrar := NewFileExecutionPreflightRegistrar(t.TempDir())
	unauthorized, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil || unauthorized.Decision != "refused" || unauthorized.Code != executioncell.PreflightAuthorizationUnavailable {
		t.Fatalf("unadmitted request = %+v, %v", unauthorized, err)
	}
	if err := registrar.admitExecutionPreflight(context.Background(), localAdmissionFor(request)); err != nil {
		t.Fatal(err)
	}
	first, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil || second.RegistrationID != first.RegistrationID || second.AuthorizationRevision != first.AuthorizationRevision {
		t.Fatalf("replay = %+v, %v", second, err)
	}

	changedBinding := binding
	changedBinding.PreflightRegistration = &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"}
	changedReceipt := readyPreflightReceipt(t, changedBinding, strings.Repeat("a", 64))
	changedReceipt = append(changedReceipt[:len(changedReceipt)-1], ' ', '}')
	changed, err := executioncell.NewPreflightRegistrationRequest(changedBinding, changedReceipt, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	refused, err := registrar.RegisterExecutionPreflight(context.Background(), changed)
	if err != nil || refused.Decision != "refused" || refused.Code != executioncell.PreflightBindingMismatch {
		t.Fatalf("changed replay = %+v, %v", refused, err)
	}

	responses := make(chan executioncell.PreflightRegistrationResponse, 2)
	errorsSeen := make(chan error, 2)
	for range 2 {
		go func() {
			response, callErr := registrar.RegisterExecutionPreflight(context.Background(), request)
			responses <- response
			errorsSeen <- callErr
		}()
	}
	for range 2 {
		if callErr := <-errorsSeen; callErr != nil {
			t.Fatal(callErr)
		}
		if response := <-responses; response.RegistrationID != first.RegistrationID {
			t.Fatalf("contender response = %+v", response)
		}
	}
}

func TestFileExecutionPreflightRegistrarRefusesConsumedAndRetiredAuthority(t *testing.T) {
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-lifecycle", WorkerID: "worker", PlacementID: "host", ClaimID: "claim",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"},
	}
	request, err := executioncell.NewPreflightRegistrationRequest(binding, readyPreflightReceipt(t, binding, strings.Repeat("a", 64)), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	admission := localAdmissionFor(request)
	admission.ClaimReceiptSHA256 = strings.Repeat("d", 64)
	dir := t.TempDir()
	registrar := NewFileExecutionPreflightRegistrar(dir)
	if err := registrar.admitExecutionPreflight(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	response, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := registrar.beginExecutionPreflightStart(context.Background(), request, response); err != nil {
		t.Fatal(err)
	}
	started, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil || started.Decision != "refused" || started.Code != executioncell.PreflightAlreadyStarted {
		t.Fatalf("started response = %+v, %v", started, err)
	}
	if err := registrar.retireExecutionPreflight(context.Background(), binding.RequestID); err != nil {
		t.Fatal(err)
	}
	registrar = NewFileExecutionPreflightRegistrar(dir)
	retired, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil || retired.Decision != "refused" || retired.Code != executioncell.PreflightClaimRetired {
		t.Fatalf("retired response = %+v, %v", retired, err)
	}
}

func TestFileExecutionPreflightRegistrarRefusesDeniedReceiptAndCancellation(t *testing.T) {
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-denied", WorkerID: "worker", PlacementID: "host",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"},
	}
	denied := rawJSON(t, executioncell.HostAdaptationReceipt{ContractVersion: executioncell.HostAdaptationContractVersion, RequestID: binding.RequestID, WorkerID: binding.WorkerID, PlacementID: binding.PlacementID, Decision: "denied", Denial: "unsupported"})
	request, err := executioncell.NewPreflightRegistrationRequest(binding, denied, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	registrar := NewFileExecutionPreflightRegistrar(t.TempDir())
	if err := registrar.admitExecutionPreflight(context.Background(), localAdmissionFor(request)); err != nil {
		t.Fatal(err)
	}
	response, err := registrar.RegisterExecutionPreflight(context.Background(), request)
	if err != nil || response.Decision != "refused" || response.Code != executioncell.PreflightReceiptDenied {
		t.Fatalf("denied response = %+v, %v", response, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registrar.RegisterExecutionPreflight(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestHTTPExecutionPreflightRegistrarUsesFixedOriginAndRotatingToken(t *testing.T) {
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-http", WorkerID: "worker", PlacementID: "host",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"},
	}
	request, err := executioncell.NewPreflightRegistrationRequest(binding, readyPreflightReceipt(t, binding, strings.Repeat("a", 64)), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	token := "token-one"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, incoming *http.Request) {
		if incoming.URL.Path != "/api/daemon/sessions/request-http/execution-preflight-registration" {
			t.Errorf("path = %q", incoming.URL.Path)
		}
		if incoming.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", incoming.Header.Get("Authorization"))
		}
		var got executioncell.PreflightRegistrationRequest
		if err := json.NewDecoder(incoming.Body).Decode(&got); err != nil || got.ReceiptSHA256 != request.ReceiptSHA256 {
			t.Errorf("request = %+v, %v", got, err)
		}
		response, _ := authorizedRegistration(request)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	registrar, err := NewHTTPExecutionPreflightRegistrar(HTTPExecutionPreflightRegistrarOptions{BaseURL: server.URL, Token: func() string { return token }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registrar.RegisterExecutionPreflight(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	token = "token-two"
	if _, err := registrar.RegisterExecutionPreflight(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPExecutionPreflightRegistrarRefusesRedirectAndUntrustedOrigin(t *testing.T) {
	binding := executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingV2ContractVersion,
		RequestID:       "request-http", WorkerID: "worker", PlacementID: "host",
		PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: "challenge"},
	}
	request, err := executioncell.NewPreflightRegistrationRequest(binding, readyPreflightReceipt(t, binding, strings.Repeat("a", 64)), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, incoming *http.Request) {
		http.Redirect(w, incoming, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	registrar, err := NewHTTPExecutionPreflightRegistrar(HTTPExecutionPreflightRegistrarOptions{BaseURL: redirector.URL, Token: func() string { return "token" }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registrar.RegisterExecutionPreflight(context.Background(), request); err == nil {
		t.Fatal("redirect was accepted")
	}
	if redirected.Load() {
		t.Fatal("registrar followed a caller redirect")
	}
	for _, baseURL := range []string{"http://example.com", "https://user@example.com", "https://example.com?callback=https://evil.invalid"} {
		if _, err := NewHTTPExecutionPreflightRegistrar(HTTPExecutionPreflightRegistrarOptions{BaseURL: baseURL, Token: func() string { return "token" }}); err == nil {
			t.Fatalf("untrusted base URL accepted: %s", baseURL)
		}
	}
}

func TestPreflightRegistrationCapabilityOnlyWhenGateConfigured(t *testing.T) {
	base := effectiveRegistrationCapabilities(nil)
	registrar := NewFileExecutionPreflightRegistrar(t.TempDir())
	replayable := NewFileExecutionPreflightStore(t.TempDir())
	provider := &orderedPreflightProvider{mu: &sync.Mutex{}, order: &[]string{}}
	if slices.Contains(preflightRegistrationCapabilities(base, nil, replayable, provider), ExecutionPreflightRegistrationCapability) {
		t.Fatal("unconfigured daemon advertised preflight registration")
	}
	if slices.Contains(preflightRegistrationCapabilities(base, registrar, &countingExecutionStore{}, provider), ExecutionPreflightRegistrationCapability) {
		t.Fatal("daemon without exact receipt replay advertised preflight registration")
	}
	if slices.Contains(preflightRegistrationCapabilities(base, registrar, replayable, nil), ExecutionPreflightRegistrationCapability) {
		t.Fatal("daemon without execution compiler advertised preflight registration")
	}
	configured := preflightRegistrationCapabilities(base, registrar, replayable, provider)
	if !slices.Contains(configured, ExecutionPreflightRegistrationCapability) {
		t.Fatal("configured daemon omitted preflight registration capability")
	}
}
