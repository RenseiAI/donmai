package daemon

import (
	"runtime"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// confinementKernelFloor names the kernel floor for a full seat boundary:
// the scope layer needs Landlock ABI 6, which ships in Linux 6.12. Below it
// seats run with a degraded boundary: the mount tree, the process namespace
// and the filesystem rules hold, while signals and abstract sockets outside
// stay reachable, so the host reports a partial boundary, never a confined one.
const confinementKernelFloor = "Linux 6.12 (Landlock ABI 6) for the full boundary"

// confinementStatusReporter answers the daemon's OS-confinement self-check
// for status and doctor output. Production probes the running host: the
// scope layer is cheap and uncached (one Landlock version syscall), while
// the last proven self-test is whatever the workers recorded.
type confinementStatusReporter struct {
	// scopesAvailable reports whether the scope layer is enforced,
	// defaulting to the production kernel probe below.
	scopesAvailable func() (bool, string)
	// latestProvenRecord reports the last proven self-test, if any.
	latestProvenRecord func() (record confinement.SelfTestRecord, ok bool)
	// checkBackend reports whether the backend can apply a boundary from
	// this process now. Nil means the running backend's Check. A failure
	// reports unproven even where a passing record remains: the boundary
	// seats would start from no longer holds.
	checkBackend func() error
}

// productionConfinementReporter is the status path's reporter: the running
// kernel's scope probe plus the latest self-test the seat layer recorded
// on this host.
var productionConfinementReporter = confinementStatusReporter{
	scopesAvailable:    confinement.ScopesAvailable,
	latestProvenRecord: latestProvenConfinementRecord,
}

// latestProvenConfinementRecord reports the last proven seat self-test on
// this host, if one was recorded. It runs in the daemon: seats prove the
// boundary in their own worker processes, so the daemon answers from the
// record they recorded, never by probing itself. Where no seat has yet
// recorded a self-test on this boot's kernel, there is no evidence to
// attest from and the status says so instead of claiming a boundary no
// seat proved.
func latestProvenConfinementRecord() (confinement.SelfTestRecord, bool) {
	return confinement.LatestProvenRecord()
}

// confinementStatus is the daemon's OS-confinement self-check for /status
// and /doctor. It reports the reporter carried: the scope layer the running
// kernel enforces, plus the last self-test a seat proved on it. It attests
// only where a current, passing, non-degraded record proves the full
// boundary: a host whose self-test failed, went stale, never ran, or proved
// a partial boundary reports unproven or degraded instead. Secret-free: it
// names the backend and the missing layer, never paths, digests or probe
// values.
//
// The kernel probe is cheap (one Landlock version syscall) and uncached, so
// a status reader always sees this boot's kernel rather than a stale note.
func confinementStatus() *afclient.ConfinementStatus {
	return productionConfinementReporter.status()
}

// status renders one confinement posture from the reporter's answers.
// It fails closed: a missing scope probe never reads as enforced, and a
// malformed probe answer (enforced with a reason, or missing with none)
// reports degraded rather than attesting.
func (r confinementStatusReporter) status() *afclient.ConfinementStatus {
	backend := confinement.DefaultBackend()
	if backend == nil {
		return &afclient.ConfinementStatus{}
	}
	status := &afclient.ConfinementStatus{Backend: string(backend.Name())}
	if r.scopesAvailable == nil {
		status.Detail = "seat boundary unproven at " + confinementKernelFloor + " or newer: scope probe unavailable on this host"
		return status
	}
	scopeOK, scopeWhy := r.scopesAvailable()
	// The backend check is live, not cached: a host whose backend now fails
	// (user namespaces disabled after a pass, a nested sandbox, a missing
	// launcher) reports unproven even where a passing record remains, so the
	// operator surface never attests a boundary seats can no longer start from.
	checkErr := error(nil)
	if r.checkBackend != nil {
		checkErr = r.checkBackend()
	} else {
		checkErr = backend.Check()
	}
	record, recordOK := confinement.SelfTestRecord{}, false
	if r.latestProvenRecord != nil {
		record, recordOK = r.latestProvenRecord()
	}
	// The scope layer decides the degraded line wherever it is missing or
	// the probe answers inconsistently: a host below the floor reports the
	// partial boundary even before any seat ran, and a proven record from
	// one never attests. An empty reason with a missing layer still
	// reports degraded with a generic line, never an empty degraded
	// marker a reader could misread as whole.
	if !scopeOK || scopeWhy != "" {
		degraded := scopeWhy
		if degraded == "" {
			degraded = "the scope layer is unenforced: Landlock scope probe reports the layer missing"
		}
		status.Degraded = degraded
		status.Detail = "seat boundary is partial below " + confinementKernelFloor + ": same-user signals and abstract sockets outside stay reachable"
		return status
	}
	// The scope layer holds; attestation still needs proof. A record the
	// seat layer recorded on this boot's kernel — passing, current and
	// whole — is the only evidence the status attests from. A degraded
	// record proves a partial boundary only: it keeps the degraded line
	// even where the kernel probe reports the layer enforced (a record
	// taken below the floor, read back above it).
	if recordOK && record.Degraded != "" {
		status.Degraded = record.Degraded
		status.Detail = "seat boundary is partial below " + confinementKernelFloor + ": same-user signals and abstract sockets outside stay reachable"
		return status
	}
	if !recordOK || !record.Passed || len(record.SessionModes) == 0 {
		status.Detail = "seat boundary unproven at " + confinementKernelFloor + " or newer: no passing self-test recorded on this host"
		return status
	}
	if checkErr != nil {
		status.Detail = "seat boundary unproven at " + confinementKernelFloor + " or newer: the backend cannot apply a boundary on this host"
		return status
	}
	if !recordStillCurrent(backend, record) {
		status.Detail = "seat boundary unproven at " + confinementKernelFloor + " or newer: the last self-test is stale on this host"
		return status
	}
	status.Attested = true
	if runtime.GOOS == "linux" {
		status.Detail = "seat boundary holds at " + confinementKernelFloor + " or newer"
	}
	return status
}

// recordStillCurrent reports whether the proven record is still the
// boundary seats start from: the same backend version under the same probe
// set. It mirrors the confiner's staleness gate without a confiner: the
// status path has no spawn binding to consult, only the record the seats
// recorded.
func recordStillCurrent(backend confinement.Backend, record confinement.SelfTestRecord) bool {
	version, err := backend.Version()
	if err != nil {
		return false
	}
	return record.Backend == backend.Name() &&
		record.BackendVersion == version &&
		record.ProbeSetVersion == confinement.ProbeSetVersion
}
