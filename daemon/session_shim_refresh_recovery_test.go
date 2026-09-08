package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionShimRefreshRecoversAnExpiredHeartbeatWithoutRestart(t *testing.T) {
	for _, localRecovering := range []bool{false, true} {
		name := "locally-ready"
		if localRecovering {
			name = "locally-recovering"
		}
		t.Run(name, func(t *testing.T) {
			d, _ := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
			d.setState(StateRunning)
			if localRecovering {
				d.sessionShimReadinessWithdrawn.Store(true)
				d.setState(StateRecovering)
			}
			beforeState := d.State()
			beforeWithdrawn := d.sessionShimReadinessWithdrawn.Load()
			att := d.SessionShimHostAttestation()
			var refreshes, accepted atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/refresh-token") {
					if r.Header.Get("Authorization") != "Bearer rsp_live_x" {
						t.Error("refresh lost registration authority")
					}
					refreshes.Add(1)
					_ = json.NewEncoder(w).Encode(refreshResponse{
						RuntimeToken: "fresh.jwt", RuntimeTokenExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
						SessionShim: activationTestCredentialReceipt(att, SessionShimCredentialStateRecovering, "stable-host-readiness", "31"),
					})
					return
				}
				if r.Header.Get("Authorization") != "Bearer fresh.jwt" {
					http.Error(w, `{"error":"Runtime token expired"}`, http.StatusUnauthorized)
					return
				}
				var body heartbeatRequestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				accepted.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true, "sessionShim": body.SessionShim})
			}))
			t.Cleanup(server.Close)
			opts := testRefresherOptions(t, server.URL, "worker-recovery", "expired.jwt")
			opts.Registration.SessionShim = att
			opts.ValidateRefresh = d.validateAndRetainSessionShimRefreshReceipt
			refresher := NewCredentialRefresher(opts)
			poll := &recordingLane{}
			beat := NewHeartbeatService(HeartbeatOptions{
				WorkerID: "worker-recovery", RuntimeJWT: "expired.jwt", OrchestratorURL: server.URL,
				HTTPClient: server.Client(), OnReregister: refresher.OnReregister,
				GetActiveCount: func() int { return 0 }, GetMaxCount: func() int { return 4 },
				GetStatus: d.RegistrationStatus,
				GetSessionShim: func() (SessionShimHeartbeatProjection, error) {
					return d.SessionShimHeartbeatProjection(d.sessionShimConfig().orgID())
				},
			})
			refresher.Attach(beat, poll)
			if err := beat.sendOneResult(context.Background()); err != nil {
				t.Fatalf("expired heartbeat did not recover: %v", err)
			}
			if refreshes.Load() != 1 || accepted.Load() != 1 {
				t.Fatalf("refreshes=%d accepted beats=%d", refreshes.Load(), accepted.Load())
			}
			if worker, jwt, _ := poll.current(); worker != "worker-recovery" || jwt != "fresh.jwt" {
				t.Fatal("poll retained expired credentials")
			}
			cached, err := LoadCachedJWT(opts.Registration.JWTPath)
			if err != nil || cached == nil || cached.RuntimeToken != "fresh.jwt" {
				t.Fatalf("recovery credential cache: %v", err)
			}
			if d.State() != beforeState || d.sessionShimReadinessWithdrawn.Load() != beforeWithdrawn {
				t.Fatal("renewing credentials changed local admission readiness")
			}
		})
	}
}

func TestSessionShimRefreshStateDoesNotGrantOrRequireLocalReadiness(t *testing.T) {
	for _, state := range []string{SessionShimCredentialStateReady, SessionShimCredentialStateRecovering} {
		t.Run(state, func(t *testing.T) {
			d, _ := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
			d.setState(StateRecovering)
			d.shims.adoptionComplete = false
			d.shims.carrierActivationComplete = false
			d.sessionShimReadinessWithdrawn.Store(true)
			receipt := activationTestCredentialReceipt(d.SessionShimHostAttestation(), state, "stable-host-readiness", "31")
			if err := d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: receipt}); err != nil {
				t.Fatal(err)
			}
			if d.State() != StateRecovering || d.SessionShimAdoptionComplete() || d.SessionShimCarrierActivationComplete() || !d.sessionShimReadinessWithdrawn.Load() {
				t.Fatal("a credential receipt granted adoption or admission")
			}
		})
	}
}

func TestSessionShimRefreshRefusalDoesNotRetainInvalidAuthority(t *testing.T) {
	for _, invalid := range []string{"state", "host"} {
		t.Run(invalid, func(t *testing.T) {
			d, _ := readinessTestDaemon(t, SessionShimConfig{}, healthyReadiness)
			d.setState(StateRunning)
			before, ok := d.SessionShimScopeAuthority(d.sessionShimConfig().orgID())
			if !ok {
				t.Fatal("fixture has no scope authority")
			}
			receipt := activationTestCredentialReceipt(d.SessionShimHostAttestation(), SessionShimCredentialStateReady, before.WorkerHostID, "32")
			if invalid == "state" {
				receipt.State = "invalid"
			} else {
				receipt.WorkerHostID = "another-host"
			}
			if err := d.validateAndRetainSessionShimRefreshReceipt(&RefreshTokenResult{SessionShim: receipt}); err == nil {
				t.Fatal("invalid receipt retained")
			}
			after, _ := d.SessionShimScopeAuthority(d.sessionShimConfig().orgID())
			if after != before {
				t.Fatal("refused receipt changed retained authority")
			}
		})
	}
}
