package confinement

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

var bothModes = []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled}

// cacheWorld is a confiner with a probe executable on disk and a cache
// directory under its state home.
type cacheWorld struct {
	w        specWorld
	c        *Confiner
	opts     SelfTestOptions
	launches *atomic.Int32
}

func newCacheWorld(t *testing.T, backend Backend, extra ExtraRules, digest string) cacheWorld {
	t.Helper()
	w := newSpecWorld(t)
	probe := filepath.Join(w.base, "probe")
	if err := testexec.WriteFile(probe, []byte("probe executable v1"), 0o700); err != nil { //nolint:gosec // G306: a stand-in executable.
		t.Fatal(err)
	}
	launches := &atomic.Int32{}
	count := func(context.Context, []string, []string, string) (int, error) {
		launches.Add(1)
		return 0, nil
	}
	return cacheWorld{
		w: w,
		c: newTestConfiner(t, w, backend, extra, digest),
		opts: SelfTestOptions{
			ProbeCommand: []string{probe},
			ScratchDir:   filepath.Join(w.base, "scratch"),
			CacheDir:     filepath.Join(w.stateHome, "selftest-cache"),
			Launchers:    map[agent.PromptSessionMode]Launcher{agent.PromptModeAutonomous: count, agent.PromptModeHumanControlled: count},
		},
		launches: launches,
	}
}

// seed writes a passing record taken at testedAt into cw's cache slot.
func (cw cacheWorld) seed(t *testing.T, testedAt time.Time) (selfTestCache, SelfTestRecord) {
	t.Helper()
	current, err := cw.c.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	slot, ok := cw.c.selfTestCache(cw.opts, current)
	if !ok {
		t.Fatal("caching is off")
	}
	record := passingRecord(t, cw.c, bothModes...)
	record.TestedAt = testedAt
	record.Digest = record.computeDigest()
	slot.save(record)
	return slot, record
}

// writeProvenFileForTest plants one proven-record file under dir without
// going through recordProvenSelfTest's whole-record gate, so the test can
// prove LatestProvenRecord refuses shapes the recorder never writes (a
// degraded file smuggled onto disk, a wrong version, a tampered digest).
func writeProvenFileForTest(t *testing.T, dir string, name string, file provenSelfTestFile) {
	t.Helper()
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProvenRecord_StatusReadsWhatSeatsProved pins the status path's
// evidence seam on every host: a passing, whole record recorded through
// recordProvenSelfTest reads back through LatestProvenRecord, while a
// degraded record, a failing record, a tampered digest, and an empty
// directory read back as no evidence. Deleting the degraded or digest
// checks in LatestProvenRecord turns this test red.
func TestProvenRecord_StatusReadsWhatSeatsProved(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)

	passing := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().UTC(),
	}
	passing.Digest = passing.computeDigest()
	recordProvenSelfTest(passing)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
		t.Fatalf("LatestProvenRecord = %+v, %v; want the proven %s", latest, ok, passing.Digest)
	}
	// A degraded record proves a partial boundary only: recording it
	// keeps no evidence, so the previous whole record still reads back —
	// and a degraded file smuggled onto disk is refused as well.
	degraded := passing
	degraded.Degraded = "the scope layer is unenforced"
	degraded.SessionModes = nil
	degraded.Digest = degraded.computeDigest()
	recordProvenSelfTest(degraded)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
		t.Fatalf("LatestProvenRecord after a degraded record = %+v, %v; want the whole %s", latest, ok, passing.Digest)
	}
	if dir := latestSelfTestCacheDir(); dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	} else {
		// The smuggled file keeps its session modes: only the Degraded
		// check refuses it. (A degraded record that dropped its modes
		// is already refused by the empty-modes check.)
		modesKept := degraded
		modesKept.SessionModes = bothModes
		// Newer than the whole record, so without the Degraded check
		// it would win the latest-pick and read back as evidence.
		modesKept.TestedAt = passing.TestedAt.Add(time.Hour)
		modesKept.Digest = modesKept.computeDigest()
		writeProvenFileForTest(t, dir, "proven-smuggled.json", provenSelfTestFile{Version: provenSelfTestFileVersion, Record: modesKept})
		if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
			t.Fatalf("LatestProvenRecord with a smuggled degraded file = %+v, %v; want only the whole %s", latest, ok, passing.Digest)
		}
		if err := os.Remove(filepath.Join(dir, "proven-smuggled.json")); err != nil {
			t.Fatal(err)
		}
	}
	// A failing record proves nothing either.
	failing := passing
	failing.Passed = false
	failing.Digest = failing.computeDigest()
	recordProvenSelfTest(failing)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
		t.Fatalf("LatestProvenRecord after a failing record = %+v, %v; want the whole %s", latest, ok, passing.Digest)
	}
	// A tampered file is not evidence: the digest no longer covers it.
	if dir := latestSelfTestCacheDir(); dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	} else if entries, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "proven-") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var file provenSelfTestFile
			if err := json.Unmarshal(raw, &file); err != nil {
				t.Fatal(err)
			}
			file.Record.Probes = append(file.Record.Probes, ProbeOutcome{ID: "q"})
			raw, err = json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := LatestProvenRecord(); ok {
		t.Fatal("LatestProvenRecord reads a tampered file as evidence")
	}
}

// TestProvenRecord_RefusesStaleRecord pins the proven-window gate: a
// passing, whole record older than the proven TTL is no evidence, so the
// status path reports unproven instead of attesting from it. Deleting the
// age check in LatestProvenRecord turns this test red.
func TestProvenRecord_RefusesStaleRecord(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)

	stale := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes:   []ProbeOutcome{{ID: "p", Pass: true}},
		TestedAt: time.Now().Add(-2 * provenRecordTTL).UTC(),
	}
	stale.Digest = stale.computeDigest()
	recordProvenSelfTest(stale)
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a stale record as evidence: %+v", latest)
	}
}

// TestProvenRecord_IgnoresOversizedFile pins the read bound: a proven
// file larger than the per-file cap is skipped, so a bloated directory
// cannot stall the status path or smuggle evidence past the digest gate.
// Deleting the size check in LatestProvenRecord turns this test red: the
// oversized file would read back as evidence.
func TestProvenRecord_IgnoresOversizedFile(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)

	passing := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}
	passing.Digest = passing.computeDigest()
	recordProvenSelfTest(passing)
	dir := latestSelfTestCacheDir()
	if dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	}
	// Pad a copy of the proven record past the per-file cap while keeping
	// its digest valid, so only the size gate refuses it. The bloated copy
	// is newer but still within the proven window and not in the future,
	// so without the size gate it would win the latest-pick.
	bloated := passing
	pad := strings.Repeat("x", maxProvenFileBytes+1)
	bloated.Probes = append(bloated.Probes, ProbeOutcome{ID: pad, Pass: true})
	bloated.TestedAt = passing.TestedAt.Add(time.Hour)
	bloated.Digest = bloated.computeDigest()
	writeProvenFileForTest(t, dir, "proven-bloated.json", provenSelfTestFile{Version: provenSelfTestFileVersion, Record: bloated})
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
		t.Fatalf("LatestProvenRecord with an oversized file = %+v, %v; want only the whole %s", latest, ok, passing.Digest)
	}
}

// TestProvenRecord_FailureSupersedesPass pins the fail-closed latest outcome:
// a failure recorded after a pass (a failing self-test or a backend Check
// failure) supersedes the pass, so the status path reports unproven instead
// of attesting from the stale pass. A later pass supersedes the failure
// again. Ignoring the latest file (reading the older proven pass) turns this
// test red.
func TestProvenRecord_FailureSupersedesPass(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	dir := latestSelfTestCacheDir()
	if dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	}
	passing := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().Add(-3 * time.Hour).UTC(),
	}
	passing.Digest = passing.computeDigest()
	recordProvenSelfTest(passing)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != passing.Digest {
		t.Fatalf("LatestProvenRecord = %+v, %v; want the proven %s", latest, ok, passing.Digest)
	}
	// A failure an hour later (the Check-failure shape: not passing, with a
	// digest) supersedes the pass.
	failing := passing
	failing.Passed = false
	failing.TestedAt = passing.TestedAt.Add(time.Hour)
	failing.Digest = failing.computeDigest()
	recordLatestSelfTest(dir, failing)
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a pass superseded by a failure as evidence: %+v", latest)
	}
	// A later pass supersedes the failure again.
	repassed := passing
	repassed.TestedAt = failing.TestedAt.Add(time.Hour)
	repassed.Digest = repassed.computeDigest()
	recordLatestSelfTest(dir, repassed)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != repassed.Digest {
		t.Fatalf("LatestProvenRecord = %+v, %v; want the repassed %s", latest, ok, repassed.Digest)
	}
}

// TestProvenRecord_FutureStampIsNoEvidence pins the future-stamp arm in both
// readers: a record stamped in the future (clock skew at test time) is no
// evidence, through the per-record scan and through the latest file. Deleting
// either future check turns this test red.
func TestProvenRecord_FutureStampIsNoEvidence(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	dir := latestSelfTestCacheDir()
	if dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	}
	future := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().Add(time.Hour).UTC(),
	}
	future.Digest = future.computeDigest()
	recordLatestSelfTest(dir, future)
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a future-stamped latest as evidence: %+v", latest)
	}
	if err := os.Remove(filepath.Join(dir, latestSelfTestFileName)); err != nil {
		t.Fatal(err)
	}
	writeProvenFileForTest(t, dir, "proven-future.json", provenSelfTestFile{Version: provenSelfTestFileVersion, Record: future})
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a future-stamped proven file as evidence: %+v", latest)
	}
}

// TestProvenRecord_FreshPassReadAmongManyStale pins the newest-first scan:
// with more stale files than the read cap plus one fresh pass, the fresh
// pass is still read. The stale names sort before the fresh name, so an
// alphabetical scan capped at 128 would break before the fresh file and
// report no evidence; the newest-first (modification-time) scan reads it.
// Deleting the ordering (or the prune that keeps the directory small) turns
// this test red on the old scan.
func TestProvenRecord_FreshPassReadAmongManyStale(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	dir := latestSelfTestCacheDir()
	if dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().Add(-2 * provenRecordTTL).UTC()
	oldMod := staleAt
	for i := 0; i < maxProvenRecordsPerRead+1; i++ {
		stale := SelfTestRecord{
			Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
			ExecutableDigest: "sha256:test",
			SessionModes:     bothModes, Passed: true,
			Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: staleAt.Add(time.Duration(i) * time.Second),
		}
		stale.Digest = stale.computeDigest()
		// Zero-padded sequence sorts before the fresh name, so an
		// alphabetical scan capped at 128 breaks before the fresh file.
		name := fmt.Sprintf("proven-%04d.json", i)
		writeProvenFileForTest(t, dir, name, provenSelfTestFile{Version: provenSelfTestFileVersion, Record: stale})
		if err := os.Chtimes(filepath.Join(dir, name), oldMod, oldMod); err != nil {
			t.Fatal(err)
		}
	}
	fresh := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().Add(-time.Hour).UTC(),
	}
	fresh.Digest = fresh.computeDigest()
	writeProvenFileForTest(t, dir, "proven-zzzz-fresh.json", provenSelfTestFile{Version: provenSelfTestFileVersion, Record: fresh})
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != fresh.Digest {
		t.Fatalf("LatestProvenRecord among %d stale files = %+v, %v; want the fresh %s", maxProvenRecordsPerRead+1, latest, ok, fresh.Digest)
	}
}

// TestProvenRecord_PruneKeepsOnlyLatest pins the write-time prune: recording
// a second passing record deletes the first file, so the directory never
// grows one file per self-test. Deleting the prune turns this test red (two
// files remain).
func TestProvenRecord_PruneKeepsOnlyLatest(t *testing.T) {
	base := t.TempDir()
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	dir := latestSelfTestCacheDir()
	if dir == "" {
		t.Fatal("latestSelfTestCacheDir is empty; want the shared proven directory")
	}
	first := SelfTestRecord{
		Backend: BackendMacOSSeatbelt, BackendVersion: "v", ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: "sha256:test",
		SessionModes:     bothModes, Passed: true,
		Probes: []ProbeOutcome{{ID: "p", Pass: true}}, TestedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}
	first.Digest = first.computeDigest()
	recordProvenSelfTest(first)
	second := first
	second.TestedAt = time.Now().Add(-time.Hour).UTC()
	second.Digest = second.computeDigest()
	recordProvenSelfTest(second)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var proven []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "proven-") && strings.HasSuffix(entry.Name(), ".json") {
			proven = append(proven, entry.Name())
		}
	}
	if len(proven) != 1 {
		t.Fatalf("proven files = %v, want exactly the latest", proven)
	}
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != second.Digest {
		t.Fatalf("LatestProvenRecord = %+v, %v; want the second %s", latest, ok, second.Digest)
	}
}

// checkFailingBackend is a backend whose Check fails, as a host does once
// user namespaces are disabled or the launcher goes missing after a pass.
type checkFailingBackend struct{ noopBackend }

func (checkFailingBackend) Check() error {
	return refuse(ReasonNamespaceUnavailable, "user namespaces are disabled")
}

// provenWorld is a confiner wired the way the harness adapters wire it: its
// state home is the host state home (under an isolated base home), so the
// latest outcome and proven files its self-test writes land exactly where
// the public LatestProvenRecord reader looks. Its launchers only count, so
// a real SelfTest through it runs every probe and fails them all (nothing
// is applied): a completed, failing self-test.
type provenWorld struct {
	base string
	c    *Confiner
	opts SelfTestOptions
	dir  string
}

func newProvenWorld(t *testing.T, backend Backend) provenWorld {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	stateHome := statehome.StateDir("")
	profileDir := filepath.Join(stateHome, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(base, "probe")
	if err := testexec.WriteFile(probe, []byte("probe executable v1"), 0o700); err != nil { //nolint:gosec // G306: a stand-in executable.
		t.Fatal(err)
	}
	count := func(context.Context, []string, []string, string) (int, error) { return 0, nil }
	pw := provenWorld{
		base: base,
		opts: SelfTestOptions{
			ProbeCommand: []string{probe},
			ScratchDir:   filepath.Join(base, "scratch"),
			Launchers:    map[agent.PromptSessionMode]Launcher{agent.PromptModeAutonomous: count, agent.PromptModeHumanControlled: count},
		},
		dir: latestSelfTestCacheDir(),
	}
	pw.c = pw.confiner(t, backend)
	if pw.dir == "" || pw.c.provenDir() != pw.dir {
		t.Fatalf("confiner proven dir %q, status reader dir %q: want the same directory", pw.c.provenDir(), pw.dir)
	}
	return pw
}

// confiner returns a confiner over pw's directories with backend.
func (pw provenWorld) confiner(t *testing.T, backend Backend) *Confiner {
	t.Helper()
	stateHome := statehome.StateDir("")
	c, err := New(Options{Backend: backend, ProfileDir: filepath.Join(stateHome, "profiles"), Home: pw.base, StateHome: stateHome, ExecutableDigest: "sha256:harness"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// seedPass records a passing, whole record taken age ago through the writers
// a passing SelfTest calls (the latest outcome plus the proven file), and
// checks that the public reader attests from it.
func (pw provenWorld) seedPass(t *testing.T, age time.Duration) SelfTestRecord {
	t.Helper()
	pass := passingRecord(t, pw.c, bothModes...)
	pass.TestedAt = time.Now().Add(-age).UTC()
	pass.Digest = pass.computeDigest()
	pw.c.recordLatest(pass)
	pw.c.recordProven(pass)
	if latest, ok := LatestProvenRecord(); !ok || latest.Digest != pass.Digest {
		t.Fatalf("LatestProvenRecord after a pass = %+v, %v; want the pass %s", latest, ok, pass.Digest)
	}
	return pass
}

// failSelfTest runs the real SelfTest through c and requires it to fail.
func (pw provenWorld) failSelfTest(t *testing.T, c *Confiner) {
	t.Helper()
	record, err := c.SelfTest(t.Context(), pw.opts)
	if err == nil || record.Passed {
		t.Fatalf("SelfTest = %+v, %v; want a failing self-test", record.Passed, err)
	}
}

// TestSelfTest_FailureAfterPassIsNoEvidence pins the latest-outcome write
// after a completed self-test: a real SelfTest that fails its probes (the
// backend Check passes) after a pass supersedes the pass, so the public
// reader reports no evidence. The status path's live Check cannot catch this
// failure, so deleting the write in SelfTest turns this test red.
func TestSelfTest_FailureAfterPassIsNoEvidence(t *testing.T) {
	pw := newProvenWorld(t, noopBackend{})
	pw.seedPass(t, time.Hour)
	pw.failSelfTest(t, pw.c)
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord after a failing self-test reads the earlier pass as evidence: %+v", latest)
	}
}

// TestSelfTest_CheckFailureAfterPassIsNoEvidence pins the latest-outcome
// write on a backend Check failure: a real SelfTest whose Check fails after a
// pass supersedes the pass, so the public reader reports no evidence.
// Deleting the write on the Check-failure path turns this test red.
func TestSelfTest_CheckFailureAfterPassIsNoEvidence(t *testing.T) {
	pw := newProvenWorld(t, noopBackend{})
	pw.seedPass(t, time.Hour)
	pw.failSelfTest(t, pw.confiner(t, checkFailingBackend{}))
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord after a Check failure reads the earlier pass as evidence: %+v", latest)
	}
}

// TestProvenRecord_StaleLatestPassIsNoEvidence pins the proven window on the
// latest outcome: a passing latest record older than the window is no
// evidence. Deleting the age check on the latest file turns this test red.
func TestProvenRecord_StaleLatestPassIsNoEvidence(t *testing.T) {
	pw := newProvenWorld(t, noopBackend{})
	stale := passingRecord(t, pw.c, bothModes...)
	stale.TestedAt = time.Now().Add(-provenRecordTTL - time.Minute).UTC()
	stale.Digest = stale.computeDigest()
	pw.c.recordLatest(stale)
	pw.c.recordProven(stale)
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a latest pass older than the proven window as evidence: %+v", latest)
	}
}

// TestProvenRecord_TamperedLatestIsNoEvidence pins the digest check on the
// latest outcome: a latest record re-stamped by hand (its time moved forward,
// its digest left as it was) is no evidence, and the intact older proven file
// is not read in its place. Deleting the digest check on the latest file
// turns this test red.
func TestProvenRecord_TamperedLatestIsNoEvidence(t *testing.T) {
	pw := newProvenWorld(t, noopBackend{})
	pass := pw.seedPass(t, 2*time.Hour)
	tampered := pass
	tampered.TestedAt = time.Now().UTC()
	writeProvenFileForTest(t, pw.dir, latestSelfTestFileName, provenSelfTestFile{Version: provenSelfTestFileVersion, Record: tampered})
	if latest, ok := LatestProvenRecord(); ok {
		t.Fatalf("LatestProvenRecord reads a tampered latest outcome as evidence: %+v", latest)
	}
}

// TestProvenRecord_BadLatestNeverFallsBack pins the fail-closed read of the
// latest outcome. An older pass stays on disk as a proven file while a real
// failing SelfTest records the newest outcome; then that newest outcome is
// made unreadable (a directory where the file was, an error even a root
// reader gets), undecodable, or of a version this binary does not read. Each
// is no evidence through the public reader: falling back to the older pass
// would attest a host whose newest self-test failed. Only a missing latest
// file falls back to the per-record scan, so records that predate the latest
// file still read. Treating any read or decode failure as "missing" turns the
// bad cases red; treating "missing" as no evidence turns the last case red.
func TestProvenRecord_BadLatestNeverFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, latest string, pass SelfTestRecord)
		want   bool
	}{
		{"unreadable", func(t *testing.T, latest string, _ SelfTestRecord) {
			if err := os.Remove(latest); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(latest, 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"undecodable", func(t *testing.T, latest string, _ SelfTestRecord) {
			if err := os.WriteFile(latest, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"unknown version", func(t *testing.T, latest string, pass SelfTestRecord) {
			// A newer binary's latest outcome: a shape this binary does not
			// read, even where its record would decode as a pass.
			writeProvenFileForTest(t, filepath.Dir(latest), filepath.Base(latest), provenSelfTestFile{Version: provenSelfTestFileVersion + 1, Record: pass})
		}, false},
		{"missing", func(t *testing.T, latest string, _ SelfTestRecord) {
			if err := os.Remove(latest); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pw := newProvenWorld(t, noopBackend{})
			pass := pw.seedPass(t, time.Hour)
			pw.failSelfTest(t, pw.c)
			latest := filepath.Join(pw.dir, latestSelfTestFileName)
			if _, err := os.Stat(latest); err != nil {
				t.Fatalf("the failing self-test recorded no latest outcome: %v", err)
			}
			tc.damage(t, latest, pass)
			got, ok := LatestProvenRecord()
			if ok != tc.want {
				t.Fatalf("LatestProvenRecord with a %s latest outcome = %+v, %v; want evidence %v", tc.name, got, ok, tc.want)
			}
			if ok && got.Digest != pass.Digest {
				t.Fatalf("LatestProvenRecord with a %s latest outcome read %s; want the older pass %s", tc.name, got.Digest, pass.Digest)
			}
		})
	}
}

// TestSelfTest_ReusesACachedPassingRecord: a later process on the same host
// reuses a cached passing self-test without spawning a probe, and Prepare
// accepts it; once the record is older than the TTL the self-test runs
// again, and a failing run never replaces the cache.
func TestSelfTest_ReusesACachedPassingRecord(t *testing.T) {
	cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slot, seeded := cw.seed(t, time.Now().Add(-time.Hour))

	fresh := newTestConfiner(t, cw.w, noopBackend{}, nil, "sha256:harness")
	record, err := fresh.SelfTest(t.Context(), cw.opts)
	if err != nil {
		t.Fatalf("SelfTest from cache: %v", err)
	}
	if cw.launches.Load() != 0 {
		t.Fatalf("the probe was launched %d times despite a reusable cached record", cw.launches.Load())
	}
	if record.Digest != seeded.Digest || !record.TestedAt.Equal(seeded.TestedAt) {
		t.Fatalf("SelfTest returned %s from %s, want the cached %s from %s", record.Digest, record.TestedAt, seeded.Digest, seeded.TestedAt)
	}
	if _, ok := fresh.Attestation(); !ok {
		t.Fatal("a cached passing record does not attest")
	}

	expired := newTestConfiner(t, cw.w, noopBackend{}, nil, "sha256:harness")
	cw.opts.CacheTTL = time.Minute
	if _, err := expired.SelfTest(t.Context(), cw.opts); err == nil {
		t.Fatal("an expired cache was reused: the self-test did not run")
	}
	if cw.launches.Load() == 0 {
		t.Fatal("the self-test did not launch a probe once the cache expired")
	}
	raw, err := os.ReadFile(slot.path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var entry selfTestCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Record.Digest != seeded.Digest {
		t.Fatalf("a failing self-test replaced the cached record (err %v)", err)
	}
}

// TestSelfTestCache_KeyCoversWhatTheRecordProves: a different backend
// version, harness executable, probe executable or host directory selects
// a different slot, and a composer callback turns caching off.
func TestSelfTestCache_KeyCoversWhatTheRecordProves(t *testing.T) {
	base := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
	slotOf := func(cw cacheWorld) (string, bool) {
		current, err := cw.c.fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		slot, ok := cw.c.selfTestCache(cw.opts, current)
		return slot.path, ok
	}
	basePath, ok := slotOf(base)
	if !ok {
		t.Fatal("caching is off for a plain confiner")
	}
	otherProbe := base
	otherProbe.opts.ProbeCommand = []string{filepath.Join(base.w.base, "probe2")}
	if err := testexec.WriteFile(otherProbe.opts.ProbeCommand[0], []byte("probe executable v2"), 0o700); err != nil { //nolint:gosec // G306: a stand-in executable.
		t.Fatal(err)
	}
	otherHarness := base
	otherHarness.c = newTestConfiner(t, base.w, noopBackend{}, nil, "sha256:other-harness")
	otherBackend := base
	otherBackend.c = newTestConfiner(t, base.w, renderingBackend{version: "rendering-v2"}, nil, "sha256:harness")
	otherHome := base
	otherHome.c = newTestConfiner(t, base.w, noopBackend{}, nil, "sha256:harness")
	otherHome.c.opts.Home = filepath.Join(base.w.base, "other-home")
	for name, cw := range map[string]cacheWorld{
		"probe executable":   otherProbe,
		"harness executable": otherHarness,
		"backend version":    otherBackend,
		"operator home":      otherHome,
	} {
		path, ok := slotOf(cw)
		if !ok || path == basePath {
			t.Errorf("%s: slot %q (caching %v), want a slot other than %q", name, path, ok, basePath)
		}
	}
	composed := base
	composed.c = newTestConfiner(t, base.w, noopBackend{}, func(RuleContext) []Rule { return nil }, "sha256:harness")
	if _, ok := slotOf(composed); ok {
		t.Error("caching is on for a confiner with a composer callback")
	}
	unreadable := base
	unreadable.opts.ProbeCommand = []string{filepath.Join(base.w.base, "absent")}
	if _, ok := slotOf(unreadable); ok {
		t.Error("caching is on for a probe executable that cannot be digested")
	}
}

// TestSelfTestCache_LoadRefusesWhatItCannotVouchFor: a cached record is a
// miss unless it is the same cache version and key, passing in both modes,
// current by the staleness fingerprint, intact by its digest, never degraded
// and within the TTL.
func TestSelfTestCache_LoadRefusesWhatItCannotVouchFor(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*selfTestCacheEntry)
		hit    bool
	}{
		{"reusable", func(*selfTestCacheEntry) {}, true},
		{"other cache version", func(e *selfTestCacheEntry) { e.Version = 99 }, false},
		{"pre-marking cache version", func(e *selfTestCacheEntry) {
			e.Version = selfTestCacheVersion - 1
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"degraded record", func(e *selfTestCacheEntry) {
			e.Record.Degraded = "the scope layer is unenforced"
			e.Record.SessionModes = nil
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"other key", func(e *selfTestCacheEntry) { e.Key.ProbeDigest = "sha256:other" }, false},
		{"failing record", func(e *selfTestCacheEntry) { e.Record.Passed = false; e.Record.Digest = e.Record.computeDigest() }, false},
		{"one session mode", func(e *selfTestCacheEntry) {
			e.Record.SessionModes = bothModes[:1]
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"a failed probe", func(e *selfTestCacheEntry) {
			e.Record.Probes = append(e.Record.Probes, ProbeOutcome{ID: "q"})
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"stale probe set", func(e *selfTestCacheEntry) {
			e.Record.ProbeSetVersion = "executor-confinement-probes-v3"
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"tampered", func(e *selfTestCacheEntry) { e.Record.TestedAt = now }, false},
		{"expired", func(e *selfTestCacheEntry) {
			e.Record.TestedAt = now.Add(-25 * time.Hour)
			e.Record.Digest = e.Record.computeDigest()
		}, false},
		{"from the future", func(e *selfTestCacheEntry) {
			e.Record.TestedAt = now.Add(time.Hour)
			e.Record.Digest = e.Record.computeDigest()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cw := newCacheWorld(t, noopBackend{}, nil, "sha256:harness")
			slot, _ := cw.seed(t, now.Add(-time.Hour))
			raw, err := os.ReadFile(slot.path)
			if err != nil {
				t.Fatal(err)
			}
			var entry selfTestCacheEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&entry)
			raw, err = json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(slot.path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, hit := slot.load(now); hit != tc.hit {
				t.Fatalf("load hit = %v, want %v", hit, tc.hit)
			}
		})
	}
}
