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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errHostAuthorityUnknown = errors.New("host authority unknown: no primary receipt has been delivered")

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
		// A DEFINITE refusal, expressed the only way the contract lets an
		// embedder express one: an answer whose facts are not all true. An
		// error out of the resolver means "could not be consulted", which is
		// unknown and never withdraws an established readiness.
		return SessionShimCarrierProofV2Readiness{}, nil
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

// TestPollRefusalDoesNotReconcileDuringFoundingDeclaration pins the real
// daemon poll callback while a deferred composition is still founding its
// authority. The control plane correctly refuses claims until adoption is
// published; that refusal is not evidence of a stale committed projection.
//
// Before the founding receipt is retained, reconciliation cannot resolve the
// primary host and its ResolveNow readiness check asks the embedder a question
// the declaration itself is still answering. The poll callback must therefore
// defer this one trigger. Once the composition is installed, the same callback
// remains the ordinary fail-closed recovery path.
func TestPollRefusalDoesNotReconcileDuringFoundingDeclaration(t *testing.T) {
	for _, priorWithdrawal := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior-claim-gate-withdrawal=%v", priorWithdrawal), func(t *testing.T) {
			h := newCompositionHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.start(ctx)
			// Start schedules an immediate poll even with the fixture's long
			// interval. Join only this fixture's loop so the cause under test is
			// explicit; retain its actual daemon callback and live heartbeat.
			if h.daemon.poller == nil || h.daemon.poller.opts.OnSessionShimAdoptionNotReady == nil {
				t.Fatal("daemon poll refusal callback is not wired")
			}
			if err := h.daemon.poller.StopContext(ctx); err != nil {
				t.Fatalf("join fixture poll loop: %v", err)
			}
			pollCallback := h.daemon.poller.opts.OnSessionShimAdoptionNotReady
			embedder := &foundingEmbedder{}
			var callbackCalls atomic.Int32
			pollRefusal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "SESSION_SHIM_ADOPTION_NOT_READY", http.StatusServiceUnavailable)
			}))
			t.Cleanup(pollRefusal.Close)
			cfg := h.composedConfig(acceptingBatch)
			cfg.GetCarrierProofV2Readiness = embedder.readiness
			var ackMu sync.Mutex
			var acknowledgements []SessionShimPublishedBatchReceipt
			originalAck := cfg.OnCarrierActivationAcknowledged
			cfg.OnCarrierActivationAcknowledged = func(receipt SessionShimPublishedBatchReceipt) {
				ackMu.Lock()
				acknowledgements = append(acknowledgements, receipt)
				ackMu.Unlock()
				originalAck(receipt)
			}
			cfg.AcquireRecoveryScopes = func(
				cbCtx context.Context, attestation SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt,
			) ([]SessionShimScopeCredentialReceipt, error) {
				if !h.daemon.SessionShimCompositionPending() {
					return nil, errors.New("scope acquisition was not inside founding")
				}
				if priorWithdrawal {
					// Exercise the real startup claim predicate, not an atomic flag
					// assignment. Unestablished readiness legitimately closes it.
					if suspended, _ := h.daemon.claimSuspended(); !suspended {
						return nil, errors.New("unestablished readiness admitted a claim")
					}
				}
				beforeWithdrawn := h.daemon.sessionShimReadinessWithdrawn.Load()
				beforeReadinessCalls := embedder.readinessCalls.Load()
				if beforeWithdrawn != priorWithdrawal || h.daemon.reconcilingScopes() != 0 {
					return nil, fmt.Errorf("unexpected pre-poll state: withdrawn=%v scopes=%d", beforeWithdrawn, h.daemon.reconcilingScopes())
				}
				poll := NewPollService(PollOptions{
					WorkerID: "founding-poll", RuntimeJWT: "founding-poll-jwt", OrchestratorURL: pollRefusal.URL,
					OnWork:                        func(PollWorkItem) error { return nil },
					OnSessionShimAdoptionNotReady: func() { callbackCalls.Add(1); pollCallback() },
				})
				poll.pollOnce(cbCtx)
				if callbackCalls.Load() != 1 {
					return nil, fmt.Errorf("actual 503 invoked refusal callback %d times, want 1", callbackCalls.Load())
				}
				if got := h.daemon.reconcilingScopes(); got != 0 {
					return nil, fmt.Errorf("poll refusal armed %d reconciliation pass(es) before founding authority installed", got)
				}
				if h.daemon.sessionShimReadinessWithdrawn.Load() != beforeWithdrawn || embedder.readinessCalls.Load() != beforeReadinessCalls {
					return nil, errors.New("founding 503 changed readiness or consulted its resolver")
				}
				return embedder.acquireRecoveryScopes(cbCtx, attestation, primary)
			}
			if err := h.daemon.InstallSessionShimComposition(ctx, cfg); err != nil {
				t.Fatalf("composition install after founding poll refusal: %v", err)
			}
			if h.daemon.SessionShimCompositionPending() || !h.daemon.SessionShimAdoptionComplete() || !h.daemon.SessionShimCarrierActivationComplete() {
				t.Fatalf("composition incomplete: pending=%v adopted=%v activated=%v", h.daemon.SessionShimCompositionPending(), h.daemon.SessionShimAdoptionComplete(), h.daemon.SessionShimCarrierActivationComplete())
			}
			if h.daemon.sessionShimReadinessWithdrawn.Load() || h.daemon.State() != StateRunning {
				t.Fatalf("successful install did not reopen readiness: withdrawn=%v state=%s", h.daemon.sessionShimReadinessWithdrawn.Load(), h.daemon.State())
			}
			if embedder.readinessCalls.Load() == 0 {
				t.Fatal("founding install never performed its deferred readiness check")
			}
			ackMu.Lock()
			acks := append([]SessionShimPublishedBatchReceipt(nil), acknowledgements...)
			ackMu.Unlock()
			if len(acks) != 1 || acks[0].Scope != h.orgID || acks[0].AdoptionRevision != "revision-batch" {
				t.Fatalf("exact carrier heartbeat acknowledgement = %+v", acks)
			}
			beat, ok := h.lastHeartbeat()
			if !ok || beat.SessionShim == nil || beat.SessionShim.AdoptionRevision != acks[0].AdoptionRevision {
				t.Fatalf("acknowledgement has no matching actual heartbeat: %+v", beat)
			}
			t.Logf("prior claim-gate withdrawal=%v; real 503 callbacks=%d; exact ACK=%s; installed state=%s withdrawn=%v", priorWithdrawal, callbackCalls.Load(), acks[0].AdoptionRevision, h.daemon.State(), h.daemon.sessionShimReadinessWithdrawn.Load())
			// Deferral is founding-only. The same real callback must still
			// reconcile and withdraw on definite post-install revocation.
			embedder.refuseReadiness.Store(true)
			pollCallback()
			waitForCondition(t, 5*time.Second, "post-install poll refusal to withdraw readiness", func() bool {
				return h.daemon.sessionShimReadinessWithdrawn.Load()
			})
		})
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

// TestPostInstallRefreshDoesNotGateRecoveryCredentialsOnReadiness proves that
// an ordinary refresh can install the credentials required to repair degraded
// readiness; claims and adoption preparation retain their separate gates.
func TestPostInstallRefreshDoesNotGateRecoveryCredentialsOnReadiness(t *testing.T) {
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
	embedder.refuseReadiness.Store(true)
	if _, err := h.daemon.credentials.Refresh(ctx, "proactive-expiry"); err != nil {
		t.Fatalf("post-install refresh with readiness refused: %v", err)
	}
	if _, jwtAfter := h.daemon.credentials.Current(); jwtAfter == jwtBefore {
		t.Fatal("refresh with readiness refused did not install fresh credentials")
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
			want:      ErrSessionShimReadinessRejected,
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
			// The deterministic stand-in for a poll tick landing inside the
			// install window: the pending identity is live, the resolver
			// cannot answer yet, and admission gets withdrawn. CI hits this
			// timing on its own; the test must not depend on winning a race.
			var withdrewDuringWindow atomic.Bool
			cfg.AcquireRecoveryScopes = func(
				cbCtx context.Context, att SessionShimHostAttestation, primary SessionShimScopeCredentialReceipt,
			) ([]SessionShimScopeCredentialReceipt, error) {
				h.daemon.withdrawSessionShimProofV2Readiness()
				withdrewDuringWindow.Store(h.daemon.sessionShimReadinessWithdrawn.Load())
				return embedder.acquireRecoveryScopes(cbCtx, att, primary)
			}
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

			// The forced withdrawal really latched inside the window — without
			// this the reopening assertions below could pass by never having
			// had anything to reopen.
			if !withdrewDuringWindow.Load() {
				t.Fatal("the install window never withdrew admission; the probe proves nothing")
			}

			// And the daemon is serving, stood down, on every surface —
			// including admission, which the install window's own withdrawal
			// had closed.
			if h.daemon.sessionShimReadinessWithdrawn.Load() {
				t.Error("the install window's readiness withdrawal outlived the failed install")
			}
			if _, err := h.daemon.AcceptWorkWithDetail(SessionSpec{}, &SessionDetail{}); err == nil ||
				strings.Contains(err.Error(), "withdrawn") {
				t.Errorf("work admission after the failed install: err = %v, want the benign spec refusal", err)
			}
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

// TestRefusedFoundingDeclarationLetsAnotherCompositionFound is the retry
// contract: when the platform refuses one founder's declaring refresh, the
// install rolls back to stand-down with a retryable founding refusal — never
// the batch refusal — and a later install with another composed configuration
// founds the composition. An accepted composition still refuses a second
// install.
func TestRefusedFoundingDeclarationLetsAnotherCompositionFound(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	first := h.composedConfig(acceptingBatch)
	h.setRefuseRefreshForController(first.ControllerID)
	firstErr := h.daemon.InstallSessionShimComposition(ctx, first)
	var refused *SessionShimFoundingRefused
	if !errors.As(firstErr, &refused) {
		t.Fatalf("refused founding install error = %v, want it classified as a founding refusal", firstErr)
	}
	var batchRefused *SessionShimDurabilityRefused
	if errors.As(firstErr, &batchRefused) {
		t.Fatalf("refused founding install error = %v, want no batch refusal", firstErr)
	}
	if refused.Scope != h.orgID {
		t.Fatalf("refused scope = %q, want %q", refused.Scope, h.orgID)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation after a refused founding = %#v, want the stand-down", got)
	}
	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after a refused founding = %q, want %q", state, StateRunning)
	}
	retained := h.daemon.SessionShimDurabilityRefusal()
	if retained == nil || retained.Scope != h.orgID || retained.Reason == "" {
		t.Fatalf("retained refusal = %+v, want the scope and a reason", retained)
	}

	second := h.composedConfig(acceptingBatch)
	second.ControllerID = "controller-second-founder"
	second.AttestationCapabilities = append([]string(nil), first.AttestationCapabilities...)
	if err := h.daemon.InstallSessionShimComposition(ctx, second); err != nil {
		t.Fatalf("a refused founder blocked a later healthy founder: %v", err)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the second install = %#v, want the composed attestation", got)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the daemon has since recovered from")
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the second install never reached a projected heartbeat")
	}

	if err := h.daemon.InstallSessionShimComposition(ctx, second); err == nil {
		t.Fatal("a second install over an accepted composition was accepted")
	}
}

// TestFoundingRefusalClassifierIsDefiniteOnlyForHeardAndAnsweredClientErrors
// is the negative side: every status that is not a heard-and-answered
// client error keeps its ordinary error — including the republishable 409
// revision-stale conflict and 400 projection rejection the heartbeat path
// recovers from by republishing, which must never classify as a founding
// refusal even on the founding round trip itself.
func TestFoundingRefusalClassifierIsDefiniteOnlyForHeardAndAnsweredClientErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		definite bool
	}{
		{name: "bad request", err: &refreshHTTPError{status: 400, body: "bad"}, definite: true},
		{name: "forbidden", err: &refreshHTTPError{status: 403, body: "no"}, definite: true},
		{name: "plain conflict", err: &refreshHTTPError{status: 409, body: "conflict"}, definite: true},
		{name: "unprocessable", err: &refreshHTTPError{status: 422, body: "no"}, definite: true},
		{name: "heartbeat forbidden", err: &heartbeatHTTPError{status: 403, body: "no"}, definite: true},
		{name: "heartbeat plain conflict", err: &heartbeatHTTPError{status: 409, body: "conflict"}, definite: true},
		{
			name: "heartbeat revision-stale conflict republishes, never refuses",
			err:  &heartbeatHTTPError{status: 409, body: `{"error":"SESSION_SHIM_ADOPTION_REVISION_STALE"}`},
		},
		{
			name: "declaring refresh carrying the stale code is behind, not refused",
			err:  &refreshHTTPError{status: 409, body: "SESSION_SHIM_ADOPTION_REVISION_STALE"},
		},
		{
			name: "heartbeat projection rejection republishes, never refuses",
			err:  &heartbeatHTTPError{status: 400, body: "unknown sessionshim projection"},
		},
		{name: "unauthorized stays ordinary", err: &refreshHTTPError{status: 401, body: "expired"}},
		{name: "not found stays ordinary", err: &refreshHTTPError{status: 404, body: "gone"}},
		{name: "server error stays ordinary", err: &refreshHTTPError{status: 503, body: "down"}},
		{name: "heartbeat server error stays ordinary", err: &heartbeatHTTPError{status: 500, body: "down"}},
		{name: "opaque error stays ordinary", err: errors.New("boom")},
		{name: "no error", err: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionShimFoundingRefusalIsDefinite(tc.err); got != tc.definite {
				t.Fatalf("sessionShimFoundingRefusalIsDefinite(%v) = %v, want %v", tc.err, got, tc.definite)
			}
			refused := newSessionShimFoundingRefused("scope", tc.err)
			if (refused != nil) != tc.definite {
				t.Fatalf("newSessionShimFoundingRefused(%v) classified = %v, want %v", tc.err, refused != nil, tc.definite)
			}
		})
	}
}

// TestRevisionStaleFirstHeartbeatRepublishesInsteadOfRefusing drives the real
// install through a fake platform whose first heartbeat answers with the
// closed revision-stale 409. The install must NOT classify it as a founding
// refusal — that answer is recovered by republishing, so the error stays
// ordinary and retryable by the heartbeat path rather than by founding again.
func TestRevisionStaleFirstHeartbeatRepublishesInsteadOfRefusing(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	h.setHeartbeatRequireRevision("revision-never-presented")
	first := h.composedConfig(acceptingBatch)
	firstErr := h.daemon.InstallSessionShimComposition(ctx, first)
	if firstErr == nil {
		t.Fatal("install over a stale first heartbeat succeeded")
	}
	var founding *SessionShimFoundingRefused
	if errors.As(firstErr, &founding) {
		t.Fatalf("revision-stale first heartbeat = %v, want no founding refusal", firstErr)
	}
	if !isSessionShimReconciliationRequired(firstErr) {
		t.Fatalf("revision-stale first heartbeat = %v, want the republishable answer", firstErr)
	}
}

// TestFoundingRetryLoopFoundsWithAnotherHealthyFounder drives the real retry
// entry point: the first founder is refused and the loop founds the
// composition with the next healthy founder, backing off between attempts —
// with no sleeps in the test. Durable sessions come up for the second
// founder. With no other founder listed, the loop retries the same
// configuration with backoff instead of staying off until restart.
func TestFoundingRetryLoopFoundsWithAnotherHealthyFounder(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	first := h.composedConfig(acceptingBatch)
	h.setRefuseRefreshForController(first.ControllerID)
	second := h.composedConfig(acceptingBatch)
	second.ControllerID = "controller-second-founder"
	second.AttestationCapabilities = append([]string(nil), first.AttestationCapabilities...)
	var waits []time.Duration
	policy := SessionShimFoundingRetryPolicy{
		Configs:        []SessionShimConfig{second},
		MaxAttempts:    2,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	if err := h.daemon.InstallSessionShimCompositionRetrying(ctx, first, policy); err != nil {
		t.Fatalf("retrying install with a healthy second founder: %v", err)
	}
	if len(waits) != 1 {
		t.Fatalf("backoff waits = %v, want exactly one between the two founders", waits)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the retrying install = %#v, want the composed attestation", got)
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the retrying install never reached a projected heartbeat")
	}

	// A founder that recovers later does not create a second composition.
	if err := h.daemon.InstallSessionShimComposition(ctx, first); err == nil {
		t.Fatal("an install over an accepted composition was accepted")
	}
}

// TestFoundingRetryLoopRetriesTheSameFounderWithBackoff is the no-other-founder
// half: with no further configuration listed, the loop retries the same
// founder with doubling backoff instead of staying off until restart, and
// gives up with the founding refusal after its attempt budget is spent.
func TestFoundingRetryLoopRetriesTheSameFounderWithBackoff(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	first := h.composedConfig(acceptingBatch)
	h.setRefuseRefreshForController(first.ControllerID)
	var waits []time.Duration
	policy := SessionShimFoundingRetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	err := h.daemon.InstallSessionShimCompositionRetrying(ctx, first, policy)
	var refused *SessionShimFoundingRefused
	if !errors.As(err, &refused) {
		t.Fatalf("exhausted retrying install error = %v, want the founding refusal", err)
	}
	if len(waits) != 2 || waits[0] != time.Millisecond || waits[1] != 2*time.Millisecond {
		t.Fatalf("backoff waits = %v, want the doubling 1ms then 2ms", waits)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation after the exhausted retrying install = %#v, want the stand-down", got)
	}
}

// TestRefusedFirstHeartbeatLetsAnotherCompositionFound is the same retry
// contract for the other founding leg: when the platform refuses one
// founder's first projected heartbeat, the install rolls back to stand-down
// with a retryable founding refusal, and a later install with another
// composed configuration founds the composition.
func TestRefusedFirstHeartbeatLetsAnotherCompositionFound(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	h.setRefuseFirstProjectedHeartbeat(true)
	first := h.composedConfig(acceptingBatch)
	firstErr := h.daemon.InstallSessionShimComposition(ctx, first)
	var refused *SessionShimFoundingRefused
	if !errors.As(firstErr, &refused) {
		t.Fatalf("refused first-heartbeat install error = %v, want it classified as a founding refusal", firstErr)
	}
	var batchRefused *SessionShimDurabilityRefused
	if errors.As(firstErr, &batchRefused) {
		t.Fatalf("refused first-heartbeat install error = %v, want no batch refusal", firstErr)
	}
	if refused.Scope != h.orgID {
		t.Fatalf("refused scope = %q, want %q", refused.Scope, h.orgID)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.StandsDown() {
		t.Fatalf("attestation after a refused first heartbeat = %#v, want the stand-down", got)
	}
	if state := h.daemon.State(); state != StateRunning {
		t.Fatalf("daemon state after a refused first heartbeat = %q, want %q", state, StateRunning)
	}
	retained := h.daemon.SessionShimDurabilityRefusal()
	if retained == nil || retained.Scope != h.orgID || retained.Reason == "" {
		t.Fatalf("retained refusal = %+v, want the scope and a reason", retained)
	}

	second := h.composedConfig(acceptingBatch)
	second.ControllerID = "controller-second-founder"
	second.AttestationCapabilities = append([]string(nil), first.AttestationCapabilities...)
	if err := h.daemon.InstallSessionShimComposition(ctx, second); err != nil {
		t.Fatalf("a refused first heartbeat blocked a later healthy founder: %v", err)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the second install = %#v, want the composed attestation", got)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the daemon has since recovered from")
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the second install never reached a projected heartbeat")
	}
}

// TestFoundingRetryLoopRefusedOnceThenAcceptedThroughRetrying is the
// refused-then-accepted half the other retry tests leave out: the platform
// refuses the founder's declaring refresh exactly once, and the retry loop
// founds the composition with the SAME configuration after backoff — no other
// founder listed. It drives the production retry entry point, not a helper.
func TestFoundingRetryLoopRefusedOnceThenAcceptedThroughRetrying(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	first := h.composedConfig(acceptingBatch)
	h.setRefuseRefreshForControllerOnce(first.ControllerID)
	var waits []time.Duration
	policy := SessionShimFoundingRetryPolicy{
		MaxAttempts:    2,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	if err := h.daemon.InstallSessionShimCompositionRetrying(ctx, first, policy); err != nil {
		t.Fatalf("retrying install after a one-shot founding refusal: %v", err)
	}
	if len(waits) != 1 || waits[0] != time.Millisecond {
		t.Fatalf("backoff waits = %v, want exactly one 1ms wait between the refused and accepted attempts", waits)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the retrying install = %#v, want the composed attestation", got)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the retrying install has since recovered from")
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the retrying install never reached a projected heartbeat")
	}

	// A recovered founder still holds the composition: no second install.
	if err := h.daemon.InstallSessionShimComposition(ctx, first); err == nil {
		t.Fatal("an install over the recovered composition was accepted")
	}
}

// TestRefusedFoundingLetsDifferentOrgFound is the retry contract across an
// organization boundary: when the platform refuses one org's founding
// declaration, a later install for a DIFFERENT org (different OrgID, not just
// a different controller) founds the composition. The refusal answers one
// founder, never the composition.
func TestRefusedFoundingLetsDifferentOrgFound(t *testing.T) {
	h := newCompositionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.start(ctx)

	first := h.composedConfig(acceptingBatch)
	h.setRefuseRefreshForController(first.ControllerID)
	firstErr := h.daemon.InstallSessionShimComposition(ctx, first)
	var refused *SessionShimFoundingRefused
	if !errors.As(firstErr, &refused) {
		t.Fatalf("refused founding install error = %v, want it classified as a founding refusal", firstErr)
	}
	if refused.Scope != h.orgID {
		t.Fatalf("refused scope = %q, want %q", refused.Scope, h.orgID)
	}

	second := h.composedConfig(acceptingBatch)
	second.OrgID = "org-second-composition"
	second.ControllerID = "controller-second-org"
	second.AttestationCapabilities = append([]string(nil), first.AttestationCapabilities...)
	if err := h.daemon.InstallSessionShimComposition(ctx, second); err != nil {
		t.Fatalf("a refused founder blocked a later healthy org: %v", err)
	}
	if got := h.daemon.sessionShimConfig().orgID(); got != second.OrgID {
		t.Fatalf("installed configuration org = %q, want %q", got, second.OrgID)
	}
	if got := h.daemon.SessionShimHostAttestation(); !got.Supports() {
		t.Fatalf("attestation after the second install = %#v, want the composed attestation", got)
	}
	if got := h.daemon.SessionShimHostAttestation().ControllerID; got != second.ControllerID {
		t.Fatalf("installed attestation controller = %q, want %q", got, second.ControllerID)
	}
	if h.daemon.SessionShimDiagnostics().DurabilityRefusal != nil {
		t.Fatal("host status still reports a refusal the daemon has since recovered from")
	}
	beat, ok := h.lastHeartbeat()
	if !ok || beat.SessionShim == nil {
		t.Fatal("the second install never reached a projected heartbeat")
	}
	if beat.SessionShim.ControllerID != second.ControllerID {
		t.Fatalf("projected heartbeat controller = %q, want %q", beat.SessionShim.ControllerID, second.ControllerID)
	}
	retained := h.daemon.sessionShimCredentialReceipts()
	if len(retained) != 1 || retained[0].Scope != second.OrgID {
		t.Fatalf("retained receipts = %+v, want exactly the second org's authority", retained)
	}
}
