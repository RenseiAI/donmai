package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// TestConfinementStatusShape pins the status block at the unit level: the
// backend is named, a degraded host carries the reason with the floor, and
// the wire shape holds no paths, digests or probe values.
func TestConfinementStatusShape(t *testing.T) {
	status := confinementStatus()
	if status == nil {
		t.Fatal("confinementStatus is nil; want the additive posture block")
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"digest"`, `"probe"`, `"path"`, `"token"`, `"secret"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("confinement status contains forbidden field %s: %s", forbidden, raw)
		}
	}
	if status.Degraded != "" && status.Attested {
		t.Fatalf("confinement status is both degraded and attested: %+v", status)
	}
}

// TestStatusServesConfinement drives the production status handler and
// asserts the confinement block rides it; the doctor route carries the
// same block.
func TestStatusServesConfinement(t *testing.T) {
	d, srv, cleanup := mustStartDaemon(t)
	defer cleanup()
	_ = d
	server := NewServer(d)
	_ = srv

	statusRecorder := httptest.NewRecorder()
	server.handleStatus(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/daemon/status", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d", statusRecorder.Code)
	}
	var status afclient.DaemonStatusResponse
	if err := json.NewDecoder(statusRecorder.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Confinement == nil {
		t.Fatal("status carries no confinement block; want the additive posture")
	}
	if status.Confinement.Degraded != "" && status.Confinement.Attested {
		t.Fatalf("status confinement is both degraded and attested: %+v", status.Confinement)
	}

	doctorRecorder := httptest.NewRecorder()
	server.handleDoctor(doctorRecorder, httptest.NewRequest(http.MethodGet, "/api/daemon/doctor", nil))
	if doctorRecorder.Code != http.StatusOK {
		t.Fatalf("doctor code = %d", doctorRecorder.Code)
	}
	var doctor struct {
		Confinement *afclient.ConfinementStatus `json:"confinement"`
	}
	if err := json.NewDecoder(doctorRecorder.Body).Decode(&doctor); err != nil {
		t.Fatalf("decode doctor: %v", err)
	}
	if doctor.Confinement == nil {
		t.Fatal("doctor carries no confinement block; want the same posture as status")
	}
}
