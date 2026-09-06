package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSessionShimRecoverySignals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "heartbeat projection rejection", err: &heartbeatHTTPError{status: http.StatusBadRequest, body: `unknown sessionShim projection key`}, want: true},
		{name: "unrelated heartbeat rejection", err: &heartbeatHTTPError{status: http.StatusBadRequest, body: `invalid hostname`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionShimReconciliationRequired(tc.err); got != tc.want {
				t.Fatalf("isSessionShimReconciliationRequired = %v, want %v", got, tc.want)
			}
		})
	}
	var heartbeatCalls atomic.Int32
	heartbeat := &HeartbeatService{opts: HeartbeatOptions{OnSessionShimRevisionStale: func() { heartbeatCalls.Add(1) }}}
	heartbeat.noteSessionShimRevisionStaleBeat(&heartbeatHTTPError{status: http.StatusBadRequest, body: "sessionShim projection rejected"})
	if got := heartbeatCalls.Load(); got != 1 {
		t.Fatalf("heartbeat reconciliation calls = %d, want 1", got)
	}

	var pollCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "SESSION_SHIM_ADOPTION_NOT_READY", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	poll := NewPollService(PollOptions{
		WorkerID: "worker", RuntimeJWT: "jwt", OrchestratorURL: srv.URL,
		OnWork:                        func(PollWorkItem) error { return nil },
		OnSessionShimAdoptionNotReady: func() { pollCalls.Add(1) },
	})
	poll.pollOnce(context.Background())
	if got := pollCalls.Load(); got != 1 {
		t.Fatalf("poll reconciliation calls = %d, want 1", got)
	}
}

func TestClassifySessionShimCarrierLoss(t *testing.T) {
	t.Parallel()
	if got := classifySessionShimCarrierLoss(ErrSessionShimCarrierLostPlatform); got != shimStreamCarrierLostPlatform {
		t.Fatalf("platform sentinel cause = %d, want platform carrier loss", got)
	}
	if got := classifySessionShimCarrierLoss(errors.New("persist durable HostFrame: context deadline exceeded")); got != shimStreamCarrierLostPlatform {
		t.Fatalf("platform persistence literal cause = %d, want platform carrier loss", got)
	}
	if got := classifySessionShimCarrierLoss(errors.New("carrier refused output")); got != shimStreamCarrierLost {
		t.Fatalf("ordinary carrier error cause = %d, want ordinary carrier loss", got)
	}
}
