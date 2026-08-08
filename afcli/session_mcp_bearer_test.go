package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

// TestDetailToQueuedWork_CarriesSessionMCPBearer closes the last hop of the
// wire: SessionDetail → runner.QueuedWork. Both branches are asserted because
// the OperationalPayload branch re-decodes the whole struct from the canonical
// projection and then restores the credential fields by hand; a field added to
// the literal but forgotten in the restore is the silently-dropped-field bug
// this codebase has hit repeatedly.
func TestDetailToQueuedWork_CarriesSessionMCPBearer(t *testing.T) {
	const (
		sessionBearer = "session-scoped-bearer"
		workerBearer  = "worker-runtime-bearer"
		expiry        = "2026-08-08T19:04:05Z"
	)

	cases := []struct {
		name               string
		operationalPayload []byte
	}{
		{"no operational payload", nil},
		{"operational payload restore branch", []byte(`{"sessionId":"sess_1","projectId":"proj_1"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &daemon.SessionDetail{
				SessionID:             "sess_1",
				WorkerID:              "worker_1",
				PlatformURL:           "https://platform.example.com",
				AuthToken:             workerBearer,
				McpAuthToken:          sessionBearer,
				McpAuthTokenExpiresAt: expiry,
				OperationalPayload:    tc.operationalPayload,
				ResolvedProfile:       &daemon.SessionResolvedProfile{Provider: "stub"},
			}
			qw := detailToQueuedWork(d)

			if qw.McpAuthToken != sessionBearer {
				t.Errorf("McpAuthToken = %q, want %q", qw.McpAuthToken, sessionBearer)
			}
			if qw.McpAuthTokenExpiresAt != expiry {
				t.Errorf("McpAuthTokenExpiresAt = %q, want %q", qw.McpAuthTokenExpiresAt, expiry)
			}
			if qw.AuthToken != workerBearer {
				t.Errorf("AuthToken = %q, want the worker bearer %q — the two must stay independent",
					qw.AuthToken, workerBearer)
			}
		})
	}
}

// TestDetailToQueuedWork_SessionMCPBearerAbsent pins the older-platform lane:
// no session-scoped bearer means empty fields and an untouched worker bearer,
// which is byte-identical to the behaviour before this change.
func TestDetailToQueuedWork_SessionMCPBearerAbsent(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess_1",
		AuthToken:       "worker-runtime-bearer",
		ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
	}
	qw := detailToQueuedWork(d)
	if qw.McpAuthToken != "" || qw.McpAuthTokenExpiresAt != "" {
		t.Errorf("want both empty when the platform stamps none; got %q / %q",
			qw.McpAuthToken, qw.McpAuthTokenExpiresAt)
	}
	if qw.AuthToken != "worker-runtime-bearer" {
		t.Errorf("AuthToken = %q, want it preserved", qw.AuthToken)
	}
}
