package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/executioncell"
)

type countingExecutionPreflight struct {
	calls   atomic.Int32
	receipt json.RawMessage
	err     error
}

func (p *countingExecutionPreflight) Names() []string { return []string{"stub"} }
func (p *countingExecutionPreflight) Capabilities(string) (map[string]any, bool) {
	return map[string]any{}, true
}

func (p *countingExecutionPreflight) PreflightExecution(json.RawMessage) (json.RawMessage, error) {
	p.calls.Add(1)
	return append(json.RawMessage(nil), p.receipt...), p.err
}

type countingExecutionStore struct{ calls atomic.Int32 }

func (s *countingExecutionStore) Persist(string, json.RawMessage) error { s.calls.Add(1); return nil }

func daemonExecutionCell() executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: "codex", Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "gpt-test", Author: "openai"},
		Endpoint:        executioncell.ServingEndpointRef{ID: "endpoint", Protocol: "openai-responses", Operator: "openai", Revision: "r1"},
		AuthBinding:     executioncell.AuthBindingRef{ID: "auth", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled, Authority: "openai", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment},
		Placement:       executioncell.PlacementRef{ID: "host-local", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode:     executioncell.SessionAutonomous, GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("a", 64), RuntimeInventoryDigest: strings.Repeat("b", 64),
	}
}

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func startExecutionPreflightDaemon(t *testing.T, provider ProviderRegistry, store ExecutionPreflightStore, credentials *atomic.Int32, marker string) *Daemon {
	t.Helper()
	tmp := t.TempDir()
	d := New(Options{
		ConfigPath: filepath.Join(tmp, "daemon.yaml"), JWTPath: filepath.Join(tmp, "daemon.jwt"),
		SkipWizard: true, SkipRegistration: true, ProviderRegistry: provider, ExecutionPreflightStore: store,
		SpawnerOptions: SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", "printf spawned > " + marker},
			OnPreSpawn:    func(_ SessionSpec, env []string) ([]string, error) { credentials.Add(1); return env, nil },
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

func TestReceiptPairCrossWorkerReplayStopsBeforeAllHostSideEffects(t *testing.T) {
	for _, claimBound := range []bool{false, true} {
		claimBound := claimBound
		t.Run(map[bool]string{false: "exact", true: "claim"}[claimBound], func(t *testing.T) {
			provider := &countingExecutionPreflight{receipt: json.RawMessage(`{"decision":"ready"}`)}
			store := &countingExecutionStore{}
			var credentials atomic.Int32
			marker := filepath.Join(t.TempDir(), "spawned")
			d := startExecutionPreflightDaemon(t, provider, store, &credentials, marker)
			cell := daemonExecutionCell()
			binding := executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-1", WorkerID: "worker-local", PlacementID: cell.Placement.ID}
			detail := &SessionDetail{SessionID: "request-1", WorkerID: "worker-local", AdmissionReceipt: json.RawMessage(`{"paired":"foreign"}`), EffectiveCell: rawJSON(t, cell)}
			if claimBound {
				claim := executioncell.ClaimReceipt{ContractVersion: executioncell.ContractVersion, ClaimReceiptID: "claim-receipt", AdmissionReceiptID: "admission", ClaimID: "claim-foreign", Decision: executioncell.ClaimClaimed, EffectiveCell: &cell, RecordedAt: "2026-08-06T12:00:00Z"}
				detail.ClaimReceipt = rawJSON(t, claim)
				binding.ClaimID = "claim-active-local"
			} else {
				binding.WorkerID = "worker-foreign"
			}
			detail.ExecutionRuntimeBinding = rawJSON(t, binding)
			if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: "request-1"}, detail); err == nil {
				t.Fatal("cross-worker replay was accepted")
			}
			if provider.calls.Load() != 0 || store.calls.Load() != 0 || credentials.Load() != 0 {
				t.Fatalf("side effects compiler=%d store=%d credential=%d", provider.calls.Load(), store.calls.Load(), credentials.Load())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worker spawn marker exists: %v", err)
			}
		})
	}
}

func TestAdaptationDeniedIsPersistedBeforeCredentialOrChildSideEffects(t *testing.T) {
	provider := &countingExecutionPreflight{receipt: json.RawMessage(`{"contractVersion":"host-adaptation/v1","requestId":"request-denied","workerId":"worker-local","placementId":"host-local","decision":"denied","denial":"required lifecycle unsupported"}`), err: errors.New("required lifecycle unsupported")}
	store := &countingExecutionStore{}
	var credentials atomic.Int32
	marker := filepath.Join(t.TempDir(), "spawned")
	d := startExecutionPreflightDaemon(t, provider, store, &credentials, marker)
	cell := daemonExecutionCell()
	detail := &SessionDetail{SessionID: "request-denied", WorkerID: "worker-local", AdmissionReceipt: json.RawMessage(`{"valid":"admission"}`), EffectiveCell: rawJSON(t, cell), ExecutionRuntimeBinding: rawJSON(t, executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingContractVersion, RequestID: "request-denied", WorkerID: "worker-local", PlacementID: cell.Placement.ID})}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: detail.SessionID}, detail); err == nil {
		t.Fatal("adaptation denial was accepted")
	} else {
		t.Logf("denial: %v", err)
	}
	if provider.calls.Load() != 1 || store.calls.Load() != 1 || credentials.Load() != 0 {
		t.Fatalf("compiler=%d durableStore=%d credential=%d", provider.calls.Load(), store.calls.Load(), credentials.Load())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker spawn marker exists: %v", err)
	}
}

func TestFileExecutionPreflightStoreIsDurableAndAppendOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewFileExecutionPreflightStore(dir)
	receipt := json.RawMessage(`{"contractVersion":"host-adaptation/v1","requestId":"session-1","workerId":"worker-1","placementId":"host-local","decision":"denied","denial":"unsupported"}`)
	if err := store.Persist("session-1", receipt); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, executionPreflightReceiptName("session-1")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(receipt) {
		t.Fatalf("stored receipt = %s, want %s", got, receipt)
	}
	changed := json.RawMessage(`{"contractVersion":"host-adaptation/v1","requestId":"session-1","workerId":"worker-1","placementId":"host-local","decision":"denied","denial":"changed"}`)
	if err := store.Persist("session-1", changed); err == nil {
		t.Fatal("immutable receipt was overwritten")
	}
	got, err = os.ReadFile(filepath.Join(dir, executionPreflightReceiptName("session-1")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(receipt) {
		t.Fatalf("stored receipt after refused overwrite = %s, want %s", got, receipt)
	}
}

// A redelivered claim recompiles the same receipt. Persisting identical bytes
// again must succeed without touching the stored receipt; anything else for
// the same session is refused with an error that wraps os.ErrExist.
func TestFileExecutionPreflightStorePersistIsIdempotentForIdenticalBytes(t *testing.T) {
	const sessionID = "session-retry"
	first := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	changed := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-2",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	tests := []struct {
		name    string
		second  json.RawMessage
		wantErr bool
	}{
		{name: "identical bytes", second: first, wantErr: false},
		{name: "same session, different receipt", second: changed, wantErr: true},
		{name: "same receipt, extra trailing byte", second: append(append(json.RawMessage(nil), first...), '\n'), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewFileExecutionPreflightStore(dir)
			if err := store.Persist(sessionID, first); err != nil {
				t.Fatalf("first persist: %v", err)
			}
			err := store.Persist(sessionID, tc.second)
			if tc.wantErr {
				if err == nil {
					t.Fatal("second persist with a different receipt succeeded")
				}
				if !errors.Is(err, os.ErrExist) {
					t.Fatalf("second persist error = %v, want it to wrap os.ErrExist", err)
				}
			} else if err != nil {
				t.Fatalf("second persist with identical bytes: %v", err)
			}
			got, err := store.Load(sessionID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if string(got) != string(first) {
				t.Fatalf("stored receipt = %s, want %s", got, first)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("receipt dir entries = %d, want 1 (no leftover pending files)", len(entries))
			}
		})
	}
}

func TestFileExecutionPreflightStorePersistIdenticalRefusesSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	sessionID := "session-symlink-identical"
	receipt := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	// An in-root symlink to a file holding identical bytes must still be
	// refused: without the Lstat regular-file guard the follow-and-compare
	// would read the target and wrongly report equality.
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.json", filepath.Join(dir, executionPreflightReceiptName(sessionID))); err != nil {
		t.Fatal(err)
	}
	if err := NewFileExecutionPreflightStore(dir).Persist(sessionID, receipt); err == nil {
		t.Fatal("symlink destination with identical bytes was accepted")
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink persist error = %v, want it to wrap os.ErrExist", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(receipt) {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
}

func TestFileExecutionPreflightStorePersistIdenticalRefusesNonRegularDestination(t *testing.T) {
	dir := t.TempDir()
	sessionID := "session-dir-identical"
	receipt := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	if err := os.Mkdir(filepath.Join(dir, executionPreflightReceiptName(sessionID)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := NewFileExecutionPreflightStore(dir).Persist(sessionID, receipt); err == nil {
		t.Fatal("non-regular destination with identical length was accepted")
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatalf("non-regular persist error = %v, want it to wrap os.ErrExist", err)
	}
}

func TestFileExecutionPreflightStorePersistIdenticalPropagatesReaffirmFailure(t *testing.T) {
	dir := t.TempDir()
	sessionID := "session-reaffirm-failure"
	receipt := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	store := NewFileExecutionPreflightStore(dir)
	if err := store.Persist(sessionID, receipt); err != nil {
		t.Fatal(err)
	}
	store.syncRoot = func(*os.Root) error { return errors.New("simulated directory sync failure") }
	if err := store.Persist(sessionID, receipt); err == nil {
		t.Fatal("identical retry with failing directory sync was accepted")
	}
}

func TestFileExecutionPreflightStorePersistRefusesOversizedReceipt(t *testing.T) {
	dir := t.TempDir()
	sessionID := "session-oversized"
	big := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          strings.Repeat("x", executioncell.MaxPreflightRegistrationReceiptBytes),
	})
	if len(big) <= executioncell.MaxPreflightRegistrationReceiptBytes {
		t.Fatalf("fixture is not oversized: %d bytes", len(big))
	}
	if err := NewFileExecutionPreflightStore(dir).Persist(sessionID, big); err == nil {
		t.Fatal("oversized receipt was accepted")
	}
}

func TestFileExecutionPreflightStoreHashesUntrustedSessionIDInsideRoot(t *testing.T) {
	dir := t.TempDir()
	store := NewFileExecutionPreflightStore(dir)
	sessionID := "../../outside"
	receipt := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	if err := store.Persist(sessionID, receipt); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != executionPreflightReceiptName(sessionID) {
		t.Fatalf("receipt entries = %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt escaped configured root: %v", err)
	}
}

func TestFileExecutionPreflightStoreRefusesDestinationSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := "session-symlink"
	if err := os.Symlink(outside, filepath.Join(dir, executionPreflightReceiptName(sessionID))); err != nil {
		t.Fatal(err)
	}
	receipt := rawJSON(t, executioncell.HostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion,
		RequestID:       sessionID,
		WorkerID:        "worker-1",
		PlacementID:     "host-local",
		Decision:        "denied",
		Denial:          "unsupported",
	})
	if err := NewFileExecutionPreflightStore(dir).Persist(sessionID, receipt); err == nil {
		t.Fatal("symlink destination was replaced")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "unchanged" {
		t.Fatalf("outside target changed: %q err=%v", got, err)
	}
}
