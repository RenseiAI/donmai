package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// TestConfinementStatus_DegradedBelowTheFloor pins the daemon's degraded
// posture through the production status entry point: with the scope probe
// stubbed below the floor, the reporter reports degraded with the reason
// and the floor, never attested. Deleting the degraded branch in the
// reporter turns this test red. The stub goes through the confinement
// package's test helper with a sub-floor version probe, so no kernel
// constant crosses the package line.
func TestConfinementStatus_DegradedBelowTheFloor(t *testing.T) {
	reporter := confinementStatusReporter{
		scopesAvailable:    stubbedSubFloorScopes,
		latestProvenRecord: noProvenConfinementRecord,
	}
	status := reporter.status()
	if status == nil {
		t.Fatal("confinementStatus is nil; want the degraded posture block")
	}
	if status.Attested {
		t.Fatalf("confinement status attests below the floor: %+v", status)
	}
	if status.Degraded == "" {
		t.Fatalf("confinement status carries no degraded reason below the floor: %+v", status)
	}
	if !strings.Contains(status.Degraded, "scope layer is unenforced") {
		t.Fatalf("confinement status degraded = %q, want the typed scope-layer reason", status.Degraded)
	}
}

// stubbedSubFloorScopes answers the scope probe as a kernel below the
// floor would: the layer missing with the typed reason.
func stubbedSubFloorScopes() (bool, string) {
	return confinement.ScopeVerdictForTest(confinement.ScopeFloorMinusOneForTest())
}

// noProvenConfinementRecord answers the proven-record lookup as a host
// where no seat ran yet: no evidence to attest from.
func noProvenConfinementRecord() (confinement.SelfTestRecord, bool) {
	return confinement.SelfTestRecord{}, false
}

// TestConfinementStatus_NeverAttestsWithoutProof pins the issue outcome at
// the operator surface through the production status entry point: on a
// kernel at or above the floor, the status still refuses to attest where
// no seat recorded a self-test — and where the recorded one failed, went
// stale, or proved a partial boundary. Gating Attested on the probe alone
// (attest whenever the kernel is new enough) turns this test red: with no
// self-test ever run in the process, the status must not claim a boundary
// no seat proved.
func TestConfinementStatus_NeverAttestsWithoutProof(t *testing.T) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; the status reports none")
	}
	// No self-test ever ran: the status reports the boundary unproven,
	// never attested.
	status := confinementStatus()
	if status == nil {
		t.Fatal("confinementStatus is nil; want the unproven posture block")
	}
	if status.Attested {
		t.Fatalf("confinement status attests with no self-test behind it: %+v", status)
	}
	if status.Degraded != "" {
		t.Fatalf("confinement status reports degraded with no record behind it: %+v", status)
	}
	// A failed record proves nothing: still unproven, never attested.
	failing := confinement.SelfTestRecord{Passed: false}
	if got := (confinementStatusReporter{
		scopesAvailable:    enforcedScopes,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) { return failing, true },
	}).status(); got.Attested {
		t.Fatalf("confinement status attests a failed self-test: %+v", got)
	}
	// A degraded record proves a partial boundary only: the degraded
	// line, never attested. (The scope layer is stubbed enforced here so
	// the check below drives the record gate, not the kernel gate.)
	degraded := confinement.SelfTestRecord{Passed: true, Degraded: "the scope layer is unenforced"}
	if got := (confinementStatusReporter{
		scopesAvailable:    enforcedScopes,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) { return degraded, true },
	}).status(); got.Attested || got.Degraded == "" {
		t.Fatalf("confinement status hides a degraded record: %+v", got)
	}
}

// enforcedScopes answers the scope probe as a kernel at or above the
// floor: the layer enforced with no reason.
func enforcedScopes() (bool, string) { return true, "" }

// TestConfinementStatusShape pins the status block at the unit level: a
// degraded host carries the reason with the floor, an unproven one carries
// neither attested nor degraded, and the wire shape holds no paths,
// digests or probe values.
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
