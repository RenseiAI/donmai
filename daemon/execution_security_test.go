package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// The exact stamp shape the control plane sends: every dimension in levels
// and sources, the "sha256:" digest of the canonical levels, and the parent
// session a "session" source names. The digests are computed independently
// of this package (sha256 of the "<dimension>=<level>" lines).
const (
	platformStampIndexZero = `{"version":1,` +
		`"levels":{"toolApproval":"bypass","fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"},` +
		`"sources":{"toolApproval":"system","fileRead":"system","fileWrite":"org","network":"system","credentials":"project","isolation":"session"},` +
		`"digest":"sha256:7a648e38b88ab49be6eb837709b6905be87abaa9e36dadbdf97bb56324a4ebe9",` +
		`"parentSessionId":"parent-session-1"}`
	platformStampWrongDigest = `{"version":1,` +
		`"levels":{"toolApproval":"bypass","fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"},` +
		`"sources":{"toolApproval":"system","fileRead":"system","fileWrite":"system","network":"system","credentials":"system","isolation":"system"},` +
		`"digest":"sha256:b9e51a0a19bb9cbc21112dcbe2a4e831ea284635aeb3a49fca1d6aefcdc44157"}`
)

// TestAcceptWorkReadsExecutionSecurityBeforeSpawn covers the daemon side of
// both lanes: every accepted work item's stamp is read with the closed
// decoder before a child exists. The exact control-plane shape is accepted;
// a malformed stamp — an explicit null, a digest that does not match its
// levels, an unknown member or version — is refused as
// execution_security_unresolvable with nothing spawned.
func TestAcceptWorkReadsExecutionSecurityBeforeSpawn(t *testing.T) {
	cases := []struct {
		name    string
		stamp   string
		wantErr bool
	}{
		{name: "absent section", stamp: ""},
		{name: "exact platform shape", stamp: platformStampIndexZero},
		{name: "explicit null", stamp: "null", wantErr: true},
		{name: "digest does not match the levels", stamp: platformStampWrongDigest, wantErr: true},
		{name: "unknown member", stamp: strings.Replace(platformStampIndexZero, `"version":1,`, `"version":1,"override":true,`, 1), wantErr: true},
		{name: "unsupported version", stamp: strings.Replace(platformStampIndexZero, `"version":1`, `"version":2`, 1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := startClaimGateDaemon(t, &stubClaimGateProvider{})
			sessionID := "es-" + strings.ReplaceAll(tc.name, " ", "-")
			payload := `{"sessionId":"` + sessionID + `"}`
			if tc.stamp != "" {
				payload = `{"sessionId":"` + sessionID + `","executionSecurity":` + tc.stamp + `}`
			}
			detail := &SessionDetail{SessionID: sessionID, OperationalPayload: json.RawMessage(payload)}
			handle, err := d.AcceptWorkWithDetail(SessionSpec{SessionID: sessionID, ProjectID: "claim-gate-project"}, detail)
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

// recordingOrchestrator captures the terminal-status and NACK calls a
// refused poll item produces.
type recordingOrchestrator struct {
	mu           sync.Mutex
	statusBodies []map[string]json.RawMessage
	nacks        int
	statusCode   int
}

func (o *recordingOrchestrator) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		defer o.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode status body: %v", err)
			}
			o.statusBodies = append(o.statusBodies, body)
			if o.statusCode != 0 {
				w.WriteHeader(o.statusCode)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/nack"):
			o.nacks++
		}
		_, _ = w.Write([]byte(`{}`))
	}
}

// TestHandlePollWorkItemRefusesMalformedStampPermanently pins the permanent
// denial: a claimed item whose stamp is malformed is reported as a terminal
// failure with failureMode "execution-security" and the typed refusal, and
// is not NACKed back onto the queue. Only when that report cannot be
// delivered does the legacy NACK run, so the claim is never stranded.
func TestHandlePollWorkItemRefusesMalformedStampPermanently(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantNacks  int
	}{
		{name: "terminal report delivered", wantNacks: 0},
		{name: "terminal report undeliverable falls back to nack", statusCode: http.StatusBadRequest, wantNacks: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orchestrator := &recordingOrchestrator{statusCode: tc.statusCode}
			server := httptest.NewServer(orchestrator.handler(t))
			t.Cleanup(server.Close)
			d := startClaimGateDaemon(t, &stubClaimGateProvider{})
			d.mu.Lock()
			d.workerID, d.jwt = "worker-es", "runtime-es"
			d.mu.Unlock()

			var item PollWorkItem
			raw := `{"sessionId":"es-poll-refusal","issueId":"issue-1","issueIdentifier":"ES-1","priority":1,"queuedAt":1,` +
				`"executionSecurity":` + platformStampWrongDigest + `}`
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				t.Fatal(err)
			}
			err := d.handlePollWorkItem(item, server.URL)
			if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
				t.Fatalf("handlePollWorkItem err = %v, want execution_security_unresolvable", err)
			}

			orchestrator.mu.Lock()
			defer orchestrator.mu.Unlock()
			if len(orchestrator.statusBodies) == 0 {
				t.Fatal("no terminal status was reported for the refused session")
			}
			body := orchestrator.statusBodies[0]
			if string(body["status"]) != `"failed"` || string(body["failureMode"]) != `"execution-security"` {
				t.Fatalf("status body status=%s failureMode=%s", body["status"], body["failureMode"])
			}
			if got := string(body["executionSecurityRefusal"]); got != `{"code":"execution_security_unresolvable"}` {
				t.Fatalf("executionSecurityRefusal = %s", got)
			}
			if orchestrator.nacks != tc.wantNacks {
				t.Fatalf("nacks = %d, want %d", orchestrator.nacks, tc.wantNacks)
			}
		})
	}
}
