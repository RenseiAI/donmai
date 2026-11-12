package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// The quarantine detail says whether the harness was alive at release. It must
// answer for the lost controller's own shim incarnation, from what the
// registry holds at release, and never from the controller's adoption-time
// Hello, which still says running or orphaned for a shim that has died.

// quarantinedRow reads the withdrawn row straight from the daemon's state, so
// a tombstone sweep on the projection path cannot consume it first.
func quarantinedRow(t *testing.T, d *Daemon, id sessionshim.Identity) sessionshim.QuarantinedSession {
	t.Helper()
	d.shims.mu.RLock()
	defer d.shims.mu.RUnlock()
	for _, q := range d.shims.quarantined {
		if q.OrgID == id.OrgID && q.SessionID == id.SessionID {
			return q
		}
	}
	t.Fatalf("no quarantined row for %s in %+v", id, d.shims.quarantined)
	return sessionshim.QuarantinedSession{}
}

func newReleaseLivenessFixture(t *testing.T) *readoptFixture {
	t.Helper()
	return newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		policy:      SessionShimReadoptionPolicy{Attempts: 1, Backoff: time.Millisecond},
		adoption:    refusingCarrier(),
		lineageLive: func(context.Context, sessionshim.Identity) bool { return false },
	})
}

// settledLostEntry is lostEntry once the fixture's shim has finished reacting
// to the loss.
//
// Closing the controller makes the in-process shim republish its OWN record
// (phase orphaned, a fresh orphan deadline) from its connection teardown, a
// few milliseconds later and on its own goroutine. A test that rewrites or
// removes the record before that republish lands has its edit silently
// replaced by the lost incarnation's record. Once the republish is visible the
// shim writes nothing more until its orphan deadline, which this fixture sets
// a minute out, so the registry is the test's to arrange.
func (f *readoptFixture) settledLostEntry(t *testing.T) adoptedShim {
	t.Helper()
	lost := f.lostEntry(t)
	waitFor(t, 10*time.Second, "the lost shim's orphan record", func() bool {
		rec, err := f.registry.Get(f.id)
		return err == nil && rec.ShimID == lost.shimID &&
			rec.Phase == shimwire.PhaseOrphaned && rec.OrphanDeadlineUnixNano != 0
	})
	return lost
}

// A shim whose discovery record is gone has no harness this daemon can stand
// behind. The carrier-loss release must say so, not repeat the Hello.
func TestReleaseWithRecordGoneReportsHarnessNotAlive(t *testing.T) {
	t.Parallel()
	f := newReleaseLivenessFixture(t)
	// Settle the loss first, or the shim's own orphan republish can put the
	// record back between this Remove and the release's read.
	f.settledLostEntry(t)
	if err := f.registry.Remove(f.id); err != nil {
		t.Fatalf("remove record: %v", err)
	}
	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamCarrierLost)

	projected := f.daemon.QuarantinedSessions()
	if len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want exactly the lineage", projected)
	}
	detail := projected[0].Detail
	if !strings.HasSuffix(detail, "; harness alive at release: false (record gone)") {
		t.Fatalf("detail = %q, want harness alive false for a removed record", detail)
	}
}

// A tombstone for the exact incarnation is the shim's own record that its
// harness ended.
func TestReleaseOfTombstonedShimReportsHarnessNotAlive(t *testing.T) {
	t.Parallel()
	f := newReleaseLivenessFixture(t)
	lost := f.lostEntry(t)
	hello := lost.controller.Hello()
	if err := f.registry.PutTombstone(sessionshim.Tombstone{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         f.id.OrgID, SessionID: f.id.SessionID,
		ShimID: lost.shimID, ProcessEpoch: hello.ProcessEpoch,
		HarnessPID: os.Getpid(), HarnessStartedAt: 1,
		ExitCode: 137, Signal: "SIGKILL",
		ObservedAtUnixNano: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("PutTombstone: %v", err)
	}
	f.daemon.quarantineLostSessionShim(f.id, lost, sessionshim.QuarantineSocketUnreachable, sessionShimControllerLostDetail)

	q := quarantinedRow(t, f.daemon, f.id)
	if want := sessionShimControllerLostDetail + "; harness alive at release: false (tombstoned)"; q.Detail != want {
		t.Fatalf("detail = %q, want %q", q.Detail, want)
	}
}

// A record now under the identity that names another incarnation says nothing
// about the lost one: neither the liveness nor the published Phase may come
// from it.
func TestReleaseIgnoresANewerIncarnationsRecord(t *testing.T) {
	t.Parallel()
	f := newReleaseLivenessFixture(t)
	// The lost shim's orphan republish must land BEFORE the newer record is
	// written, not after it: otherwise it replaces the newer record and the
	// release reads the lost incarnation's own record instead.
	lost := f.settledLostEntry(t)
	hello := lost.controller.Hello()
	rec, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	// A live phase the lost controller's Hello does not carry, so a leak of
	// the newer record into the published row is visible.
	newerPhase := shimwire.PhaseOrphaned
	if hello.Phase == shimwire.PhaseOrphaned {
		newerPhase = shimwire.PhaseRunning
	}
	rec.ShimID = "shim-newer-incarnation"
	rec.ProcessEpoch = hello.ProcessEpoch + 1
	rec.Phase = newerPhase
	if err := f.registry.Put(rec); err != nil {
		t.Fatalf("put newer incarnation: %v", err)
	}
	f.daemon.quarantineLostSessionShim(f.id, lost, sessionshim.QuarantineSocketUnreachable, sessionShimControllerLostDetail)

	// The case under test is the one the registry still holds: a release that
	// read anything but the newer record proves nothing about it.
	if got, err := f.registry.Get(f.id); err != nil || got.ShimID != rec.ShimID || got.ProcessEpoch != rec.ProcessEpoch {
		t.Fatalf("registry after release = %+v, %v; want the newer incarnation %s/%d still recorded", got, err, rec.ShimID, rec.ProcessEpoch)
	}
	q := quarantinedRow(t, f.daemon, f.id)
	if want := sessionShimControllerLostDetail + "; harness alive at release: false (record superseded)"; q.Detail != want {
		t.Fatalf("detail = %q, want %q", q.Detail, want)
	}
	if q.ShimID != lost.shimID || q.ProcessEpoch != hello.ProcessEpoch {
		t.Fatalf("row incarnation = %s/%d, want the lost %s/%d", q.ShimID, q.ProcessEpoch, lost.shimID, hello.ProcessEpoch)
	}
	if q.Phase == newerPhase {
		t.Fatalf("row Phase = %q, taken from the newer incarnation's record", q.Phase)
	}
}

// The lost incarnation's own record is the evidence, and its current phase
// replaces the Hello's.
func TestReleaseUsesTheLostIncarnationsOwnRecord(t *testing.T) {
	t.Parallel()
	f := newReleaseLivenessFixture(t)
	lost := f.settledLostEntry(t)
	rec, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	if rec.ShimID != lost.shimID {
		t.Fatalf("fixture record shim = %q, want the lost controller's %q", rec.ShimID, lost.shimID)
	}
	f.daemon.quarantineLostSessionShim(f.id, lost, sessionshim.QuarantineSocketUnreachable, sessionShimControllerLostDetail)

	q := quarantinedRow(t, f.daemon, f.id)
	got, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get after release: %v", err)
	}
	if !got.Phase.HarnessLive() {
		t.Skipf("shim moved to %q before the read; the live-record case is not observable", got.Phase)
	}
	if !strings.HasPrefix(q.Detail, sessionShimControllerLostDetail+"; harness alive at release: true (phase=") {
		t.Fatalf("detail = %q, want harness alive true from the incarnation's own record", q.Detail)
	}
	if !q.Phase.HarnessLive() {
		t.Fatalf("row Phase = %q, want the record's live phase", q.Phase)
	}
}

func TestSessionShimLivenessAtRelease(t *testing.T) {
	t.Parallel()
	if got := sessionShimLivenessAtRelease(nil, sessionshim.Identity{}, "", 0); got.alive != "unknown" || got.phase != "" {
		t.Fatalf("nil registry = %+v, want unknown with no phase", got)
	}
	f := newReleaseLivenessFixture(t)
	rec, err := f.registry.Get(f.id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	own := sessionShimLivenessAtRelease(f.registry, f.id, rec.ShimID, rec.ProcessEpoch)
	if own.phase != rec.Phase || own.evidence != "phase="+string(rec.Phase) {
		t.Fatalf("own record = %+v, want phase %q", own, rec.Phase)
	}
	if other := sessionShimLivenessAtRelease(f.registry, f.id, rec.ShimID, rec.ProcessEpoch+1); other.alive != "false" || other.phase != "" {
		t.Fatalf("other epoch = %+v, want false with no phase", other)
	}
	if other := sessionShimLivenessAtRelease(f.registry, f.id, "shim-other", rec.ProcessEpoch); other.alive != "false" || other.phase != "" {
		t.Fatalf("other shim = %+v, want false with no phase", other)
	}
	// An undecodable record is not evidence either way.
	path := filepath.Join(f.dir, "registry", f.id.RecordName())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), info.Mode().Perm()); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}
	if got := sessionShimLivenessAtRelease(f.registry, f.id, rec.ShimID, rec.ProcessEpoch); got.alive != "unknown" || got.evidence != "record unreadable" || got.phase != "" {
		t.Fatalf("unreadable record = %+v, want unknown (record unreadable)", got)
	}
}
