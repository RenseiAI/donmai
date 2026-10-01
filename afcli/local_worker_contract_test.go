package afcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func TestLocalWorkerUnsupportedContractRefusesBeforeDetail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contract string
		local          bool
	}{{"unknown", "local/v3", true}, {"wrong mode", "local/v2", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
			t.Cleanup(server.Close)
			err := runAgentRun(context.Background(), &cobra.Command{}, &agentRunOpts{sessionID: "contract-check", daemonURL: server.URL, localRuntime: tc.local, localRuntimeContract: tc.contract})
			if err == nil || !strings.Contains(err.Error(), "unsupported local worker transport contract") || calls.Load() != 0 {
				t.Fatalf("err=%v detail requests=%d", err, calls.Load())
			}
		})
	}
}
