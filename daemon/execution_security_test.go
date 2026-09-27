package daemon

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestAcceptWorkReadsExecutionSecurityBeforeSpawn covers the daemon side of
// both lanes: every accepted work item's stamped levels are read with the
// closed decoder before a child exists, so a malformed stamp is refused as
// execution_security_unresolvable with nothing spawned, while a valid or
// absent one proceeds.
func TestAcceptWorkReadsExecutionSecurityBeforeSpawn(t *testing.T) {
	levels := `{"toolApproval":"bypass","fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"}`
	cases := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "absent section", payload: `{"sessionId":"es-absent"}`},
		{name: "valid section", payload: `{"sessionId":"es-valid","executionSecurity":{"version":1,"levels":` + levels + `}}`},
		{name: "unknown level", payload: `{"sessionId":"es-unknown","executionSecurity":{"version":1,"levels":{"toolApproval":"whatever"}}}`, wantErr: true},
		{name: "unsupported version", payload: `{"sessionId":"es-version","executionSecurity":{"version":2,"levels":` + levels + `}}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := startClaimGateDaemon(t, &stubClaimGateProvider{})
			var identity struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal([]byte(tc.payload), &identity); err != nil {
				t.Fatal(err)
			}
			detail := &SessionDetail{SessionID: identity.SessionID, OperationalPayload: json.RawMessage(tc.payload)}
			handle, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: identity.SessionID, ProjectID: "claim-gate-project"}, detail)
			if tc.wantErr {
				if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
					t.Fatalf("err = %v, want execution_security_unresolvable", err)
				}
				if handle != nil {
					t.Fatal("a malformed stamp produced a session handle")
				}
				return
			}
			if err != nil {
				t.Fatalf("AcceptWorkWithDetail: %v", err)
			}
		})
	}
}
