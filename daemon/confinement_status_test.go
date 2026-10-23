package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// errBackendUnavailableForTest stands in for a backend that cannot apply a
// boundary now (user namespaces disabled, a nested sandbox, a missing
// launcher): the status path must report unproven from it.
var errBackendUnavailableForTest = errors.New("backend unavailable")

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
	// never attested. The reporter is stubbed enforced with no record so
	// the case stays hermetic on every kernel and with any host state on
	// disk: production confinementStatus reads the running kernel and the
	// shared proven directory, so it reports degraded below the floor and
	// attests where a seat already proved the boundary.
	status := (confinementStatusReporter{
		scopesAvailable:    enforcedScopes,
		latestProvenRecord: noProvenConfinementRecord,
	}).status()
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

// TestConfinementStatus_NilProbeNeverAttests pins the fail-closed probe
// gate: a reporter with no scope probe never attests, even with a current,
// passing, whole record behind it. Deleting the nil-probe guard
// (defaulting a missing probe to enforced) turns this test red: the
// reporter would attest through the record gate alone.
func TestConfinementStatus_NilProbeNeverAttests(t *testing.T) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; the status reports none")
	}
	version, err := backend.Version()
	if err != nil {
		t.Skipf("backend version unavailable: %v", err)
	}
	passing := confinement.SelfTestRecord{
		Backend: backend.Name(), BackendVersion: version,
		ProbeSetVersion: confinement.ProbeSetVersion,
		Passed:          true,
		SessionModes: []agent.PromptSessionMode{
			agent.PromptModeAutonomous, agent.PromptModeHumanControlled,
		},
	}
	reporter := confinementStatusReporter{
		scopesAvailable: nil,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) {
			return passing, true
		},
	}
	if got := reporter.status(); got.Attested {
		t.Fatalf("confinement status attests with no scope probe: %+v", got)
	}
}

// TestConfinementStatus_CheckFailureNeverAttests pins the live backend check
// in the status path: a current, passing, whole record still reports unproven
// (never attested) where the backend cannot apply a boundary now. Deleting
// the check gate turns this test red.
func TestConfinementStatus_CheckFailureNeverAttests(t *testing.T) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; the status reports none")
	}
	version, err := backend.Version()
	if err != nil {
		t.Skipf("backend version unavailable: %v", err)
	}
	passing := confinement.SelfTestRecord{
		Backend: backend.Name(), BackendVersion: version,
		ProbeSetVersion: confinement.ProbeSetVersion,
		Passed:          true,
		SessionModes: []agent.PromptSessionMode{
			agent.PromptModeAutonomous, agent.PromptModeHumanControlled,
		},
	}
	reporter := confinementStatusReporter{
		scopesAvailable: enforcedScopes,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) {
			return passing, true
		},
		checkBackend: func() error { return errBackendUnavailableForTest },
	}
	if got := reporter.status(); got.Attested || got.Degraded != "" {
		t.Fatalf("confinement status attests or degrades with a failing backend: %+v", got)
	}
}

// TestConfinementStatus_StaleBackendVersionIsUnproven pins the staleness gate
// on the backend version: a passing record taken under another version
// reports unproven, never attested. Weakening the version compare (accepting
// any non-empty running version) turns this test red.
func TestConfinementStatus_StaleBackendVersionIsUnproven(t *testing.T) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; the status reports none")
	}
	version, err := backend.Version()
	if err != nil {
		t.Skipf("backend version unavailable: %v", err)
	}
	if version == "" {
		t.Skip("backend version is empty; nothing to differ from")
	}
	stale := confinement.SelfTestRecord{
		Backend: backend.Name(), BackendVersion: version + ".stale",
		ProbeSetVersion: confinement.ProbeSetVersion,
		Passed:          true,
		SessionModes: []agent.PromptSessionMode{
			agent.PromptModeAutonomous, agent.PromptModeHumanControlled,
		},
	}
	reporter := confinementStatusReporter{
		scopesAvailable: enforcedScopes,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) {
			return stale, true
		},
		checkBackend: func() error { return nil },
	}
	if got := reporter.status(); got.Attested {
		t.Fatalf("confinement status attests a stale backend version: %+v", got)
	}
}

// TestConfinementStatus_StaleProbeSetIsUnproven pins the staleness gate on
// the probe set: a passing record taken under another probe set reports
// unproven, never attested. Deleting the staleness gate turns this test red.
func TestConfinementStatus_StaleProbeSetIsUnproven(t *testing.T) {
	backend := confinement.DefaultBackend()
	if backend == nil {
		t.Skip("no confinement backend on this host; the status reports none")
	}
	version, err := backend.Version()
	if err != nil {
		t.Skipf("backend version unavailable: %v", err)
	}
	stale := confinement.SelfTestRecord{
		Backend: backend.Name(), BackendVersion: version,
		ProbeSetVersion: confinement.ProbeSetVersion + ".stale",
		Passed:          true,
		SessionModes: []agent.PromptSessionMode{
			agent.PromptModeAutonomous, agent.PromptModeHumanControlled,
		},
	}
	reporter := confinementStatusReporter{
		scopesAvailable: enforcedScopes,
		latestProvenRecord: func() (confinement.SelfTestRecord, bool) {
			return stale, true
		},
		checkBackend: func() error { return nil },
	}
	if got := reporter.status(); got.Attested {
		t.Fatalf("confinement status attests a stale probe set: %+v", got)
	}
}

// TestConfinementStatus_EmptyReasonIsDegraded pins the malformed-probe
// gate: a missing layer with no reason still reports degraded with a
// non-empty line, never an empty degraded marker. Deleting the generic
// fallback (passing the empty reason through) turns this test red.
func TestConfinementStatus_EmptyReasonIsDegraded(t *testing.T) {
	reporter := confinementStatusReporter{
		scopesAvailable:    func() (bool, string) { return false, "" },
		latestProvenRecord: noProvenConfinementRecord,
	}
	got := reporter.status()
	if got.Attested {
		t.Fatalf("confinement status attests a missing layer with no reason: %+v", got)
	}
	if got.Degraded == "" {
		t.Fatalf("confinement status carries an empty degraded line: %+v", got)
	}
}

// TestConfinementStatus_InconsistentProbeIsDegraded pins the inconsistent
// probe gate: enforced with a reason still reports degraded rather than
// attesting. Deleting the inconsistency check (trusting the boolean
// alone) turns this test red.
func TestConfinementStatus_InconsistentProbeIsDegraded(t *testing.T) {
	reporter := confinementStatusReporter{
		scopesAvailable:    func() (bool, string) { return true, "the scope layer is unenforced" },
		latestProvenRecord: noProvenConfinementRecord,
	}
	got := reporter.status()
	if got.Attested {
		t.Fatalf("confinement status attests an inconsistent probe: %+v", got)
	}
	if got.Degraded == "" {
		t.Fatalf("confinement status carries no degraded line for an inconsistent probe: %+v", got)
	}
}

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
