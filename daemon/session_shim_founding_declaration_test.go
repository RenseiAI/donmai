package daemon

// session_shim_founding_declaration_test.go — the ONE refresh that first
// presents a deferred composition, and what it is allowed to demand.
//
// An embedder's readiness resolver answers for the primary host, and the
// primary host id is what the founding refresh's receipt carries. Asking the
// resolver before that receipt is retained asks for a fact the round trip is
// still producing; the embedder that hit this learned the id by presenting the
// attestation itself, outside the credential refresher, and the control plane
// answered the flip-flop with an attestation conflict. These tests pin the
// ordering that removes the reason to do that.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	errHostAuthorityUnknown = errors.New("host authority unknown: no primary receipt has been delivered")
	errReadinessRefused     = errors.New("proof-v2 readiness refused")
)

// foundingEmbedder mirrors the real composer's shape: its readiness resolver
// answers for the primary host and can only do so once AcquireRecoveryScopes
// has handed it the primary receipt, which it records exactly as delivered.
type foundingEmbedder struct {
	mu           sync.Mutex
	primary      SessionShimScopeCredentialReceipt
	primaryKnown bool

	readinessCalls  atomic.Int32
	refuseReadiness atomic.Bool
	// refuseScopes, when set, is what AcquireRecoveryScopes fails with — after
	// recording the primary, as a real embedder would.
	refuseScopes error
}

func (e *foundingEmbedder) acquireRecoveryScopes(
	_ context.Context, _ SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt,
) ([]SessionShimScopeCredentialReceipt, error) {
	e.mu.Lock()
	e.primary, e.primaryKnown = primary, true
	e.mu.Unlock()
	return nil, e.refuseScopes
}

func (e *foundingEmbedder) readiness() (SessionShimCarrierProofV2Readiness, error) {
	e.readinessCalls.Add(1)
	e.mu.Lock()
	known := e.primaryKnown
	e.mu.Unlock()
	if !known {
		return SessionShimCarrierProofV2Readiness{}, errHostAuthorityUnknown
	}
	if e.refuseReadiness.Load() {
		return SessionShimCarrierProofV2Readiness{}, errReadinessRefused
	}
	return testSessionShimProofV2Readiness()
}

func (e *foundingEmbedder) recordedPrimary() (SessionShimScopeCredentialReceipt, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.primary, e.primaryKnown
}

func acceptingBatch(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
	return SessionShimAdoptionBatchReceipt{
		DurableCorrelation: []byte("composition"), AdoptionRevision: "revision-batch",
	}, nil
}

// compositionRefreshCounts splits the composed refreshes the control plane saw
// from the stand-down ones, by the flat attestation each presented.
func compositionRefreshCounts(t *testing.T, h *compositionHarness) (composed, standDowns int, last map[string]any) {
	t.Helper()
	for _, raw := range h.refreshes() {
		last = shimKeysIn(t, raw)
		switch last["sessionShimSupported"] {
		case true:
			composed++
		case false:
			standDowns++
		default:
			t.Fatalf("a refresh presented no session-shim posture: %#v", last)
		}
	}
	return composed, standDowns, last
}

// TestFoundingDeclarationResolvesHostAuthorityBeforeReadinessIsAsked is the
// ordering itself. The resolver here cannot answer until the primary receipt
// has been delivered — exactly the real composer — and the install must still
// succeed, with the resolver consulted (deferred, not dropped).
func TestFoundingDeclarationResolvesHostAuthorityBeforeReadinessIsAsked(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	embedder := &foundingEmbedder{}
	cfg := h.composedConfig(acceptingBatch)
	cfg.GetCarrierProofV2Readiness = embedder.readiness
	cfg.AcquireRecoveryScopes = embedder.acquireRecoveryScopes
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install with a resolver that needs the primary receipt: %v", err)
	}

	if embedder.readinessCalls.Load() == 0 {
		t.Fatal("the readiness resolver was never consulted during the install: the check was dropped, not deferred")
	}
	if _, known := embedder.recordedPrimary(); !known {
		t.Fatal("the install completed without delivering the primary receipt")
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.exactEqual(h.attestation) {
		t.Fatalf("attestation after the install = %#v, want the composed attestation", got)
	}
	if !h.daemon.SessionShimAdoptionComplete() || h.daemon.SessionShimCompositionPending() {
		t.Fatalf("install left adoption complete=%v pending=%v",
			h.daemon.SessionShimAdoptionComplete(), h.daemon.SessionShimCompositionPending())
	}
	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after the install = %q, want %q", state, StateRunning)
	}
}

// TestAcquireRecoveryScopesReceivesTheDeclarationsPrimaryReceipt pins what the
// embedder is handed: the primary scope's receipt exactly as the declaring
// refresh resolved it, and nothing re-derived.
func TestAcquireRecoveryScopesReceivesTheDeclarationsPrimaryReceipt(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	embedder := &foundingEmbedder{}
	cfg := h.composedConfig(acceptingBatch)
	cfg.AcquireRecoveryScopes = embedder.acquireRecoveryScopes
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install: %v", err)
	}

	// The control plane answers the declaring refresh with these, and with
	// nothing else on any other round trip: the stand-down registration earns
	// no receipt at all.
	want := SessionShimScopeCredentialReceipt{
		Scope: h.orgID, WorkerHostID: "stable-host-composition", AdoptionRevision: "revision-declared",
	}
	got, known := embedder.recordedPrimary()
	if !known {
		t.Fatal("AcquireRecoveryScopes was never called")
	}
	if got != want {
		t.Fatalf("primary receipt handed to AcquireRecoveryScopes = %+v, want the declaration's %+v", got, want)
	}
	// What the daemon retained is the same authority; only the revision has
	// moved on, because the adoption pass that followed published its own.
	retained := h.daemon.sessionShimCredentialReceipts()
	if len(retained) != 1 || retained[0].Scope != want.Scope || retained[0].WorkerHostID != want.WorkerHostID {
		t.Fatalf("retained receipts = %+v, want exactly the delivered primary's scope and host", retained)
	}
}

// TestPostInstallRefreshStillChecksReadinessBeforeAdoptingTheReceipt guards
// the deferral from over-reaching. Only the founding refresh may skip the
// readiness check; an ordinary refresh after the install still runs it first,
// and a refusal there refuses the credential before any lane sees it.
func TestPostInstallRefreshStillChecksReadinessBeforeAdoptingTheReceipt(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	embedder := &foundingEmbedder{}
	cfg := h.composedConfig(acceptingBatch)
	cfg.GetCarrierProofV2Readiness = embedder.readiness
	cfg.AcquireRecoveryScopes = embedder.acquireRecoveryScopes
	if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
		t.Fatalf("composition install: %v", err)
	}
	h.setRefreshReceiptState(SessionShimCredentialStateReady)

	// Control: with readiness intact an ordinary refresh is adopted, so the
	// refusal below can only be the resolver's doing.
	if _, err := h.daemon.credentials.Refresh(ctx, "proactive-expiry"); err != nil {
		t.Fatalf("control: post-install refresh with readiness intact: %v", err)
	}
	_, jwtBefore := h.daemon.credentials.Current()
	callsBefore := embedder.readinessCalls.Load()

	embedder.refuseReadiness.Store(true)
	_, err := h.daemon.credentials.Refresh(ctx, "proactive-expiry")
	if !errors.Is(err, errReadinessRefused) {
		t.Fatalf("post-install refresh with readiness refused: err = %v, want the resolver's refusal", err)
	}
	if embedder.readinessCalls.Load() == callsBefore {
		t.Fatal("the post-install refresh never consulted the readiness resolver")
	}
	if _, jwtAfter := h.daemon.credentials.Current(); jwtAfter != jwtBefore {
		t.Fatalf("a refresh refused for readiness still adopted its credential: %q -> %q", jwtBefore, jwtAfter)
	}
}

// TestFailedInstallAfterAnAcceptedDeclarationWithdrawsItExactlyOnce is the
// rollback from the other direction. Once the control plane has accepted the
// composed attestation, every later failure of the install has to withdraw it
// — a refresher left presenting a composition for a daemon standing down is
// the same flip-flop, started from the daemon's side.
func TestFailedInstallAfterAnAcceptedDeclarationWithdrawsItExactlyOnce(t *testing.T) {
	errScopesRefused := errors.New("satellite scope unavailable")
	for name, tc := range map[string]struct {
		configure func(*foundingEmbedder)
		want      error
	}{
		"recovery scopes refused after the primary was delivered": {
			configure: func(e *foundingEmbedder) { e.refuseScopes = errScopesRefused },
			want:      errScopesRefused,
		},
		"readiness refused once the primary was delivered": {
			configure: func(e *foundingEmbedder) { e.refuseReadiness.Store(true) },
			want:      errReadinessRefused,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCompositionHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.start(ctx)

			embedder := &foundingEmbedder{}
			tc.configure(embedder)
			cfg := h.composedConfig(acceptingBatch)
			cfg.GetCarrierProofV2Readiness = embedder.readiness
			cfg.AcquireRecoveryScopes = embedder.acquireRecoveryScopes
			installErr := h.daemon.InstallSessionShimComposition(ctx, cfg)
			if !errors.Is(installErr, tc.want) {
				t.Fatalf("install err = %v, want %v", installErr, tc.want)
			}

			composed, standDowns, last := compositionRefreshCounts(t, h)
			if composed != 1 {
				t.Fatalf("composed declarations = %d, want exactly the one the control plane accepted", composed)
			}
			if standDowns != 1 {
				t.Fatalf("stand-down re-declarations after the failed install = %d, want exactly 1", standDowns)
			}
			if last["sessionShimSupported"] != false {
				t.Fatalf("the last refresh presented %#v, want the stand-down", last)
			}

			// And the daemon is serving, stood down, on every surface.
			if state := h.daemon.State(); state != StateRunning {
				t.Fatalf("daemon state after the failed install = %q, want %q", state, StateRunning)
			}
			if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
				t.Fatalf("attestation after the failed install = %#v, want the stand-down", got)
			}
			if got := h.daemon.credentials.SessionShimAttestation(); !got.StandsDown() {
				t.Fatalf("credential lane attestation after the failed install = %#v, want the stand-down", got)
			}
			if retained := h.daemon.sessionShimCredentialReceipts(); len(retained) != 0 {
				t.Fatalf("receipts retained for a withdrawn composition: %+v", retained)
			}
			if h.daemon.SessionShimCompositionPending() {
				t.Fatal("a withdrawn composition is still reported pending")
			}
			if h.daemon.SessionShimOwnsSession(SessionSpec{SessionID: "session-withdrawn", Mode: interactiveRunMode}) {
				t.Fatal("a withdrawn composition still claims interactive sessions")
			}
		})
	}
}
