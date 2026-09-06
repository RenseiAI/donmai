package daemon

import (
	"errors"
	"net/http"
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
	if !isSessionShimAdoptionNotReady(&PollHTTPError{Status: http.StatusServiceUnavailable, Body: "SESSION_SHIM_ADOPTION_NOT_READY"}) {
		t.Fatal("session-shim adoption-not-ready poll response did not request reconciliation")
	}
}

func TestClassifySessionShimCarrierLoss(t *testing.T) {
	t.Parallel()
	if got := classifySessionShimCarrierLoss(errors.Join(errors.New("persist durable HostFrame: context deadline exceeded"), ErrSessionShimCarrierLostPlatform)); got != shimStreamCarrierLostPlatform {
		t.Fatalf("platform persist error cause = %d, want platform carrier loss", got)
	}
	if got := classifySessionShimCarrierLoss(errors.New("carrier refused output")); got != shimStreamCarrierLost {
		t.Fatalf("ordinary carrier error cause = %d, want ordinary carrier loss", got)
	}
}
