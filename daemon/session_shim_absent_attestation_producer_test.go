package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// absentProducerFixture is one host with one lineage, a real registry on disk,
// and a fake composer that records every terminal report it is handed.
type absentProducerFixture struct {
	t        *testing.T
	dir      string
	registry *sessionshim.Registry
	daemon   *Daemon

	mu        sync.Mutex
	terminals []SessionShimTerminalEvidence
	batches   []SessionShimAdoptionBatch
	refuse    error
}

func newAbsentProducerFixture(t *testing.T) *absentProducerFixture {
	t.Helper()
	// A unix socket path has a short platform limit and t.TempDir() bakes the
	// test name into it, so the registry lives on a short path — the same
	// reason the re-adoption fixture does it.
	dir, err := os.MkdirTemp("/tmp", "abs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return newAbsentProducerFixtureOver(t, dir)
}

// newAbsentProducerFixtureOver builds a fixture over an EXISTING registry
// directory. It is how the restart test stands a second daemon up over the
// first one's on-disk state.
func newAbsentProducerFixtureOver(t *testing.T, dir string) *absentProducerFixture {
	t.Helper()
	f := &absentProducerFixture{t: t, dir: dir}
	f.daemon = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true,
		RegistryDir:    dir,
		HostIDForOrg:   func(context.Context, string) (string, error) { return "wh_test_host", nil },
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.refuse != nil {
				return f.refuse
			}
			if evidence.Absent != nil {
				absent := *evidence.Absent
				evidence.Absent = &absent
			}
			f.terminals = append(f.terminals, evidence)
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.batches = append(f.batches, cloneSessionShimAdoptionBatch(batch))
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("rev-1"), AdoptionRevision: "1",
			}, nil
		},
	}})
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	f.registry = registry
	f.daemon.shims.mu.Lock()
	f.daemon.shims.registry = registry
	f.daemon.shims.mu.Unlock()
	return f
}

// putRecord publishes a discovery record naming process, with a socket path
// that nothing is listening on unless the test starts one.
func (f *absentProducerFixture) putRecord(
	id sessionshim.Identity, shimID string, epoch uint64,
	process sessionshim.ProcessIdentity, socketPath string,
) {
	f.t.Helper()
	if err := f.registry.Put(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: epoch,
		PID: process.PID, ProcessStartedAt: process.StartedAt,
		SocketPath:  socketPath,
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}); err != nil {
		f.t.Fatalf("Put record: %v", err)
	}
}

// deadSocketPath names a socket inside the registry dir that nothing binds.
func (f *absentProducerFixture) deadSocketPath() string {
	return filepath.Join(f.dir, "gone.sock")
}

// liveSocket binds and serves a unix socket, so a dial against it answers —
// the "a live shim answers whatever the process table says" case.
func (f *absentProducerFixture) liveSocket() string {
	f.t.Helper()
	path := filepath.Join(f.dir, "live.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		f.t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	f.t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return path
}

func (f *absentProducerFixture) quarantine(id sessionshim.Identity, shimID string, epoch uint64) {
	f.t.Helper()
	q := sessionshim.NewQuarantinedSession(sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: epoch,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}, sessionshim.QuarantineSocketUnreachable, sessionShimControllerLostDetail, time.Now())
	f.daemon.shims.mu.Lock()
	f.daemon.upsertShimQuarantineLocked(q)
	f.daemon.shims.mu.Unlock()
}

func (f *absentProducerFixture) reported() []SessionShimTerminalEvidence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionShimTerminalEvidence(nil), f.terminals...)
}

// deadProcess is a recorded identity that provably is not running: a pid that
// exists paired with a start time it does not have. Alive() answers (false,
// nil) for it without the test having to find a free pid, which is racy.
func deadProcess() sessionshim.ProcessIdentity {
	return sessionshim.ProcessIdentity{PID: os.Getpid(), StartedAt: 1}
}

func liveProcess(t *testing.T) sessionshim.ProcessIdentity {
	t.Helper()
	self, err := sessionshim.Self()
	if err != nil {
		t.Fatalf("own process identity: %v", err)
	}
	return self
}

// TestSweepAttestsOnlyAnUnobservableLineage is the producer's whole contract in
// one table, driven through the periodic reconcile the occupancy and heartbeat
// surfaces already call.
//
// Before this producer existed the first row was silent: a quarantined lineage
// whose shim had been SIGKILLed had no tombstone to hand over, so every pass
// skipped it and its recovery obligation stayed `active` — with the host's
// batch composition wedged behind it — for the life of the host.
//
// The other rows are what keeps the discharge honest. A tombstone is ORDINARY
// terminal evidence and is strictly stronger, so it must be handed over as such
// and never downgraded to an attestation. A shim that is still running is
// quarantined, not gone. And a shim whose socket answers is observable whatever
// the process table says — the case that matters because the process table is
// exactly what is unreliable here.
func TestSweepAttestsOnlyAnUnobservableLineage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		process         func(*testing.T) sessionshim.ProcessIdentity
		liveSocket      bool
		tombstone       bool
		wantReports     int
		wantAbsent      bool
		wantQuarantined int
		wantRecord      bool
	}{
		{
			name:        "process gone with no tombstone is attested absent",
			process:     func(*testing.T) sessionshim.ProcessIdentity { return deadProcess() },
			wantReports: 1, wantAbsent: true, wantQuarantined: 0, wantRecord: false,
		},
		{
			name:      "a group-reaped tombstone is handed over as terminal evidence, never as an attestation",
			process:   func(*testing.T) sessionshim.ProcessIdentity { return deadProcess() },
			tombstone: true,
			// Mutual exclusion: the report carries the tombstone and no
			// attestation. Reading a proven reap as mere unobservability would
			// convert a resolvable obligation into an abandoned one, which can
			// never permit the release the tombstone was proof for.
			wantReports: 1, wantAbsent: false, wantQuarantined: 0, wantRecord: false,
		},
		{
			name:        "a live shim is not attested at all",
			process:     liveProcess,
			wantReports: 0, wantAbsent: false, wantQuarantined: 1, wantRecord: true,
		},
		{
			// The process table says gone; the socket says otherwise. On darwin
			// four errnos including EIO collapse onto "no such process", and on
			// linux a foreign pid namespace or hidepid reads ENOENT for a live
			// one. A shim that answers its socket is observable, and the veto
			// is what keeps a misread from discharging a running harness.
			name:        "a shim whose socket still answers is never attested on the process table alone",
			process:     func(*testing.T) sessionshim.ProcessIdentity { return deadProcess() },
			liveSocket:  true,
			wantReports: 0, wantAbsent: false, wantQuarantined: 1, wantRecord: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAbsentProducerFixture(t)
			id := sessionshim.Identity{OrgID: "org-absent", SessionID: "session-absent"}
			const shimID = "shim-absent"
			const epoch = uint64(7)

			if tc.tombstone {
				if err := f.registry.PutTombstone(sessionshim.Tombstone{
					SchemaVersion: sessionshim.RecordSchemaVersion,
					OrgID:         id.OrgID, SessionID: id.SessionID,
					ShimID: shimID, ProcessEpoch: epoch,
					HarnessPID: os.Getpid(), HarnessStartedAt: 1,
					ExitCode: 137, Signal: "SIGKILL", GroupReaped: true,
					ObservedAtUnixNano: time.Now().UnixNano(),
				}); err != nil {
					t.Fatalf("PutTombstone: %v", err)
				}
			}
			socket := f.deadSocketPath()
			if tc.liveSocket {
				socket = f.liveSocket()
			}
			f.putRecord(id, shimID, epoch, tc.process(t), socket)
			f.quarantine(id, shimID, epoch)

			f.daemon.reconcileQuarantinedTombstones()

			reports := f.reported()
			if len(reports) != tc.wantReports {
				t.Fatalf("composer received %d terminal reports, want %d: %+v", len(reports), tc.wantReports, reports)
			}
			if tc.wantReports > 0 {
				got := reports[0]
				if got.Identity != id || got.ShimID != shimID || got.ProcessEpoch != epoch {
					t.Errorf("report named %+v, want the exact incarnation %s/%s/%d", got, id, shimID, epoch)
				}
				if got.HostID != "wh_test_host" {
					t.Errorf("report carried host %q", got.HostID)
				}
				switch {
				case tc.wantAbsent:
					if got.Absent == nil || !got.Absent.Complete() {
						t.Fatalf("report carried absent=%+v, want a complete attestation", got.Absent)
					}
					if got.Tombstone != (sessionshim.Tombstone{}) {
						t.Errorf("an attestation carried a tombstone: %+v", got.Tombstone)
					}
					if got.Adoption != nil {
						t.Errorf("an attestation carried an adopted-kind correlation: %+v", got.Adoption)
					}
				default:
					if got.Absent != nil {
						t.Errorf("terminal tombstone evidence carried an attestation: %+v", got.Absent)
					}
					if !got.Tombstone.GroupReaped {
						t.Errorf("terminal evidence did not carry the group-reap proof: %+v", got.Tombstone)
					}
				}
			}
			if got := len(f.daemon.QuarantinedSessions()); got != tc.wantQuarantined {
				t.Errorf("quarantine projection holds %d lineages, want %d", got, tc.wantQuarantined)
			}
			present, err := f.registry.HasIncarnation(id, shimID, epoch)
			if err != nil {
				t.Fatalf("HasIncarnation: %v", err)
			}
			if present != tc.wantRecord {
				t.Errorf("discovery record present = %v, want %v — the attestation's second fact is that the record is gone",
					present, tc.wantRecord)
			}
			if tc.wantAbsent {
				// A discharged lineage leaves NOTHING behind: no sidecar, no
				// retained proof, no handoff mark. The mark is consulted from
				// every occupancy and heartbeat surface, and releasing it
				// re-reads it to close its in-flight channel — so forgetting in
				// the wrong order silently re-inserts what the forget removed.
				if _, ok, sidecarErr := f.registry.GetWithdrawnAbsence(id, shimID, epoch); sidecarErr != nil || ok {
					t.Errorf("the withdrawn-record sidecar outlived its durable acceptance (ok=%v, err=%v)", ok, sidecarErr)
				}
				f.daemon.shims.mu.RLock()
				marks := len(f.daemon.shims.reportingTerminal)
				proofs := len(f.daemon.shims.absenceProofs)
				f.daemon.shims.mu.RUnlock()
				if marks != 0 || proofs != 0 {
					t.Errorf("a discharged lineage left %d handoff marks and %d retained proofs behind", marks, proofs)
				}
			}
		})
	}
}

// TestAbsenceProbeTreatsEveryUncertainReadingAsRetain is the
// second-observation control, one row per way a reading can be wrong.
//
// One reading was enough while a misread was inert: the lineage was classified
// stale, logged, and its record kept as diagnostic evidence. It is not enough
// now that the same misread withdraws the record, abandons the obligation and
// drops the row. ProcessIdentity.Alive cannot discriminate for us — darwin
// folds ESRCH, EINVAL, EIO and ENOENT onto one answer, and on linux a foreign
// pid namespace or hidepid reads ENOENT for a live process — so the probe has
// to get its confidence from somewhere else: two separated readings that agree,
// and a socket nothing answers.
//
// Every row here must end in the SAME place: the lineage retained, nothing
// reported, the record still on disk. "Unknown" and "gone" are different
// answers and only one of them may discharge anything.
func TestAbsenceProbeTreatsEveryUncertainReadingAsRetain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		liveness func(reading *int) func(sessionshim.ProcessIdentity) (bool, error)
		mutate   func(*absentProducerFixture, sessionshim.Identity, string, uint64)
	}{
		{
			// The darwin sysctl's surprising members. A reading that errored
			// answered nothing, and nothing is not death.
			name: "an unverifiable first reading",
			liveness: func(*int) func(sessionshim.ProcessIdentity) (bool, error) {
				return func(sessionshim.ProcessIdentity) (bool, error) {
					return false, errors.New("input/output error")
				}
			},
		},
		{
			// The transient misread, and the reason the second reading exists
			// at all: the record never changes, so nothing but re-reading can
			// catch it.
			name: "a live process that reads gone exactly once",
			liveness: func(reading *int) func(sessionshim.ProcessIdentity) (bool, error) {
				return func(sessionshim.ProcessIdentity) (bool, error) {
					*reading++
					return *reading > 1, nil
				}
			},
		},
		{
			// The second reading errors after a clean first one. Same rule.
			name: "an unverifiable second reading",
			liveness: func(reading *int) func(sessionshim.ProcessIdentity) (bool, error) {
				return func(sessionshim.ProcessIdentity) (bool, error) {
					if *reading++; *reading > 1 {
						return false, errors.New("input/output error")
					}
					return false, nil
				}
			},
		},
		{
			name: "the process comes back between the two readings",
			mutate: func(f *absentProducerFixture, id sessionshim.Identity, shimID string, epoch uint64) {
				f.putRecord(id, shimID, epoch, liveProcess(f.t), f.deadSocketPath())
			},
		},
		{
			name: "the record is withdrawn between the two readings",
			mutate: func(f *absentProducerFixture, id sessionshim.Identity, shimID string, epoch uint64) {
				if err := f.registry.RemoveIncarnation(id, shimID, epoch); err != nil {
					f.t.Fatalf("RemoveIncarnation: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAbsentProducerFixture(t)
			id := sessionshim.Identity{OrgID: "org-flap", SessionID: "session-flap"}
			const shimID = "shim-flap"
			const epoch = uint64(2)
			f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())
			f.quarantine(id, shimID, epoch)

			reading := 0
			f.daemon.shims.mu.Lock()
			if tc.liveness != nil {
				f.daemon.shims.absenceLiveness = tc.liveness(&reading)
			}
			if tc.mutate != nil {
				f.daemon.shims.afterFirstAbsenceObservation = func(shimIncarnation) {
					tc.mutate(f, id, shimID, epoch)
				}
			}
			f.daemon.shims.mu.Unlock()

			f.daemon.reconcileQuarantinedTombstones()

			if got := f.reported(); len(got) != 0 {
				t.Fatalf("an uncertain reading discharged the lineage anyway: %+v", got)
			}
			if got := len(f.daemon.QuarantinedSessions()); got != 1 {
				t.Errorf("quarantine projection holds %d lineages, want the lineage retained", got)
			}
			if _, withdrawn, err := f.registry.GetWithdrawnAbsence(id, shimID, epoch); err != nil || withdrawn {
				t.Errorf("an uncertain reading withdrew the discovery record (withdrawn=%v, err=%v)", withdrawn, err)
			}
		})
	}
}

// TestTombstoneWrittenDuringTheProbeBlocksTheAttestation is the ordering
// control for the mutual-exclusion re-read.
//
// A shim publishes its tombstone and only then spends its courtesy waits before
// exiting, so a process that reads as gone has already written one if its
// finalizer ran at all. Reading the tombstone only at the top of the probe
// leaves a window in which a proven reap is downgraded to mere unobservability
// — which holds the release predicate forever and strands the tombstone on
// disk, because the reconcile reaches a lineage only through the quarantine set
// this discharge removes it from.
func TestTombstoneWrittenDuringTheProbeBlocksTheAttestation(t *testing.T) {
	t.Parallel()
	f := newAbsentProducerFixture(t)
	id := sessionshim.Identity{OrgID: "org-race", SessionID: "session-race"}
	const shimID = "shim-race"
	const epoch = uint64(4)
	f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())
	f.quarantine(id, shimID, epoch)

	f.daemon.shims.mu.Lock()
	f.daemon.shims.afterFirstAbsenceObservation = func(shimIncarnation) {
		if err := f.registry.PutTombstone(sessionshim.Tombstone{
			SchemaVersion: sessionshim.RecordSchemaVersion,
			OrgID:         id.OrgID, SessionID: id.SessionID,
			ShimID: shimID, ProcessEpoch: epoch,
			HarnessPID: os.Getpid(), HarnessStartedAt: 1,
			ExitCode: 0, GroupReaped: true, ObservedAtUnixNano: time.Now().UnixNano(),
		}); err != nil {
			t.Errorf("PutTombstone: %v", err)
		}
		// PutTombstone withdraws the live record as it publishes. Put it back:
		// a crash between the two leaves exactly that pair on disk — the state
		// sessionshim.Adopt calls out by name — and leaving the record gone
		// would let the second-observation guard catch this case instead, which
		// would make this test a control over the wrong mechanism.
		f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())
	}
	f.daemon.shims.mu.Unlock()

	f.daemon.reconcileQuarantinedTombstones()

	for _, got := range f.reported() {
		if got.Absent != nil {
			t.Fatalf("a lineage that published a group-reaped tombstone was attested merely absent: %+v", got.Absent)
		}
	}
	if _, ok, err := f.registry.GetWithdrawnAbsence(id, shimID, epoch); err != nil || ok {
		t.Errorf("a tombstoned lineage had its record withdrawn for absence (ok=%v, err=%v)", ok, err)
	}
}

// TestStartupAdoptionAttestsAStaleRecord covers the seam that owes the
// attestation FIRST. sessionshim.Adopt classifies a record stale only after
// proving its process identity is not running and finding no tombstone, which
// is §D10's shim-absent case exactly — and the startup pass used to log the
// stale set and drop it, leaving the composer holding an obligation for a
// lineage this daemon had silently stopped reporting.
func TestStartupAdoptionAttestsAStaleRecord(t *testing.T) {
	t.Parallel()
	f := newAbsentProducerFixture(t)
	id := sessionshim.Identity{OrgID: "org-stale", SessionID: "session-stale"}
	const shimID = "shim-stale"
	const epoch = uint64(3)
	f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())

	if err := f.daemon.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("startup adoption: %v", err)
	}

	reports := f.reported()
	if len(reports) != 1 {
		t.Fatalf("composer received %d terminal reports, want the stale lineage's attestation: %+v", len(reports), reports)
	}
	if reports[0].Absent == nil || !reports[0].Absent.Complete() {
		t.Fatalf("startup report carried absent=%+v, want a complete attestation", reports[0].Absent)
	}
	if reports[0].Identity != id || reports[0].ShimID != shimID || reports[0].ProcessEpoch != epoch {
		t.Errorf("startup report named %+v, want %s/%s/%d", reports[0], id, shimID, epoch)
	}
	present, err := f.registry.HasIncarnation(id, shimID, epoch)
	if err != nil {
		t.Fatalf("HasIncarnation: %v", err)
	}
	if present {
		t.Error("the stale discovery record survived its attestation; the next start would rediscover the same dead lineage")
	}
	// The batch the boot pass then composes may legitimately omit the lineage:
	// the composer discharged it before the batch was built.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, batch := range f.batches {
		for _, q := range batch.Quarantined {
			if q.Identity() == id {
				t.Errorf("the boot batch still declared a lineage the composer had already discharged: %+v", q)
			}
		}
	}
}

// TestARefusedDischargeSurvivesADaemonRestart is the durability control, and it
// is the reason the withdrawal is a rename rather than an unlink.
//
// The failure it pins is a correlated pair, not a coincidence: a control-plane
// deploy refuses the report, and a host upgrade — the very next thing an
// operator does — restarts the daemon. With the record unlinked and the proof
// only in the old heap, the lineage is invisible to Adopt, absent from the
// batch, and still held by the composer, which then refuses every complete
// batch the host publishes for omitting a correlation it holds. A sidecar the
// next daemon can read turns that into an ordinary retry.
func TestARefusedDischargeSurvivesADaemonRestart(t *testing.T) {
	t.Parallel()
	first := newAbsentProducerFixture(t)
	first.mu.Lock()
	first.refuse = errors.New("control plane is redeploying")
	first.mu.Unlock()

	id := sessionshim.Identity{OrgID: "org-restart", SessionID: "session-restart"}
	const shimID = "shim-restart"
	const epoch = uint64(9)
	first.putRecord(id, shimID, epoch, deadProcess(), first.deadSocketPath())
	first.quarantine(id, shimID, epoch)

	first.daemon.reconcileQuarantinedTombstones()

	if got := len(first.reported()); got != 0 {
		t.Fatalf("a refused report was recorded %d times", got)
	}
	if got := len(first.daemon.QuarantinedSessions()); got != 1 {
		t.Fatalf("quarantine projection holds %d lineages after a refusal, want the lineage retained", got)
	}
	present, err := first.registry.HasIncarnation(id, shimID, epoch)
	if err != nil {
		t.Fatalf("HasIncarnation: %v", err)
	}
	if present {
		t.Error("the discovery record survived the withdrawal; the attestation's second fact was not true when it was composed")
	}
	sidecar, ok, err := first.registry.GetWithdrawnAbsence(id, shimID, epoch)
	if err != nil || !ok {
		t.Fatalf("no withdrawn-record sidecar after a refused discharge (ok=%v, err=%v)", ok, err)
	}
	if sidecar.PID != deadProcess().PID || sidecar.ProcessStartedAt != deadProcess().StartedAt {
		t.Fatalf("the sidecar lost the process identity it must re-prove from: %+v", sidecar)
	}

	// THE RESTART. A brand-new daemon over the same directory: no retained
	// proof, no quarantine projection, and — because the record is no longer a
	// discovery record — nothing for Adopt to classify stale either.
	second := newAbsentProducerFixtureOver(t, first.dir)
	if err := second.daemon.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("restart adoption: %v", err)
	}

	reports := second.reported()
	if len(reports) != 1 {
		t.Fatalf("the restarted daemon reported %d terminal facts, want the withdrawn lineage re-submitted: %+v",
			len(reports), reports)
	}
	if reports[0].Absent == nil || !reports[0].Absent.Complete() {
		t.Fatalf("re-submitted report carried absent=%+v, want a complete attestation", reports[0].Absent)
	}
	if reports[0].Identity != id || reports[0].ShimID != shimID || reports[0].ProcessEpoch != epoch {
		t.Errorf("re-submitted report named %+v, want %s/%s/%d", reports[0], id, shimID, epoch)
	}
	if _, stillThere, err := second.registry.GetWithdrawnAbsence(id, shimID, epoch); err != nil || stillThere {
		t.Errorf("the sidecar outlived its durable acceptance (ok=%v, err=%v)", stillThere, err)
	}
}

// TestAnInterruptedWithdrawalIsCompletedNotStranded covers the crash the
// publish-then-unlink order deliberately allows.
//
// The sidecar is written before the record is unlinked, so a crash between the
// two leaves BOTH on disk — the recoverable direction, and the reason that
// order was chosen over the reverse. But "both on disk" means the
// attestation's second fact is not true yet, so a pass that only re-proved
// from the sidecar would refuse forever on a lineage nothing else can reach:
// the record is stale to Adopt, and the sidecar is invisible to it.
func TestAnInterruptedWithdrawalIsCompletedNotStranded(t *testing.T) {
	t.Parallel()
	f := newAbsentProducerFixture(t)
	id := sessionshim.Identity{OrgID: "org-interrupted", SessionID: "session-interrupted"}
	const shimID = "shim-interrupted"
	const epoch = uint64(8)

	// Exactly what a crash between publish and unlink leaves behind.
	f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())
	if _, err := f.registry.WithdrawIncarnationForAbsence(id, shimID, epoch); err != nil {
		t.Fatalf("WithdrawIncarnationForAbsence: %v", err)
	}
	f.putRecord(id, shimID, epoch, deadProcess(), f.deadSocketPath())
	f.quarantine(id, shimID, epoch)

	f.daemon.reconcileQuarantinedTombstones()

	reports := f.reported()
	if len(reports) != 1 || reports[0].Absent == nil || !reports[0].Absent.Complete() {
		t.Fatalf("an interrupted withdrawal was stranded rather than completed: %+v", reports)
	}
	present, err := f.registry.HasIncarnation(id, shimID, epoch)
	if err != nil {
		t.Fatalf("HasIncarnation: %v", err)
	}
	if present {
		t.Error("the discovery record survived; the attestation's second fact was not true when it was composed")
	}
	if _, ok, err := f.registry.GetWithdrawnAbsence(id, shimID, epoch); err != nil || ok {
		t.Errorf("the sidecar outlived its durable acceptance (ok=%v, err=%v)", ok, err)
	}
}

// TestAbsentAttestationRefusalThrottlesTheProbe pins the sweep's own contract:
// the pass that owns a lineage's handoff does the work and every other pass
// skips immediately.
//
// The sweep runs from every occupancy and heartbeat surface. Probing before
// claiming made each of those passes pay a tombstone read, a full registry scan
// and a process syscall per quarantined lineage — for the ordinary steady state
// of that set, which is lineages whose shims are ALIVE and which therefore
// never reach the claim at all. Claiming first also puts the refusal cool-down
// in front of the disk work, not just the round trip.
func TestAbsentAttestationRefusalThrottlesTheProbe(t *testing.T) {
	t.Parallel()
	f := newAbsentProducerFixture(t)
	id := sessionshim.Identity{OrgID: "org-throttle", SessionID: "session-throttle"}
	const shimID = "shim-throttle"
	const epoch = uint64(6)
	// A LIVE shim: the probe can never prove this one absent, so the only way
	// the sweep can be throttled is if the claim was taken before the probe.
	f.putRecord(id, shimID, epoch, liveProcess(t), f.deadSocketPath())
	f.quarantine(id, shimID, epoch)

	f.daemon.reconcileQuarantinedTombstones()

	key := shimIncarnation{identity: id, shimID: shimID, processEpoch: epoch}
	f.daemon.shims.mu.RLock()
	state, held := f.daemon.shims.reportingTerminal[key]
	f.daemon.shims.mu.RUnlock()
	if !held {
		t.Fatal("the sweep probed a lineage without claiming it; every occupancy and heartbeat surface pays that scan")
	}
	if !time.Now().Before(state.retryAt) {
		t.Errorf("no cool-down was armed after an unprovable probe (retryAt %v)", state.retryAt)
	}
	// A second pass inside the cool-down must not even reach the probe, which
	// is observable as the claim being refused.
	if own, _ := f.daemon.claimSessionShimTerminalReport(key, time.Now()); own {
		t.Error("a second pass inside the cool-down claimed the lineage again")
	}
}

// TestControllerLossDrivesTheAbsenceSweep is the reachability control for the
// dominant failure.
//
// `shimStreamEnded` is the DEFAULT stream-end classification — "the shim, or
// its socket, ended the stream" — so a SIGKILLed or OOM-killed shim, whose
// socket dies with it, takes that path and no other. It reaches the discharge
// through quarantineLostSessionShim, which publishes through
// publishQuarantineAfterConsumingTerminalProof and drives the sweep
// synchronously with the row already in the projection.
//
// This drives the real releaseShimIfLive, so it fails if the sweep's call site
// goes away — which a test calling the producer directly, or scanning the
// source for an identifier, could not see. It asserts both halves at once: the
// absent lineage is discharged, and the fixture's own shim — which is alive and
// whose controller was just lost — is quarantined and left alone.
//
// The absent lineage is a SEPARATE identity because the fixture's shim runs
// in-process and therefore cannot vanish: doctoring its record to name a dead
// process is undone by the shim itself, which republishes the record when its
// controller goes. (That is the second-observation guard doing its job, and it
// is pinned in its own test.)
func TestControllerLossDrivesTheAbsenceSweep(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var terminals []SessionShimTerminalEvidence
	f := newReadoptFixtureWithOptions(t, readoptFixtureOptions{
		onTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			if evidence.Absent != nil {
				absent := *evidence.Absent
				evidence.Absent = &absent
			}
			terminals = append(terminals, evidence)
			return nil
		},
	})

	// A lineage this daemon cannot observe, quarantined on the same host: a
	// record naming a process that is not running and a socket nothing is
	// bound to, which is exactly what a killed shim leaves behind.
	vanished := sessionshim.Identity{OrgID: f.id.OrgID, SessionID: "session-vanished"}
	const vanishedShimID = "shim-vanished"
	const vanishedEpoch = uint64(11)
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         vanished.OrgID, SessionID: vanished.SessionID,
		ShimID: vanishedShimID, ProcessEpoch: vanishedEpoch,
		PID: deadProcess().PID, ProcessStartedAt: deadProcess().StartedAt,
		SocketPath:  filepath.Join(f.dir, "vanished.sock"),
		ProtocolMin: shimwire.ProtocolMin, ProtocolMax: shimwire.ProtocolMax,
		Phase:             shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := f.registry.Put(record); err != nil {
		t.Fatalf("registry.Put: %v", err)
	}
	f.daemon.shims.mu.Lock()
	f.daemon.upsertShimQuarantineLocked(
		sessionshim.NewQuarantinedSession(record, sessionshim.QuarantineSocketUnreachable,
			sessionShimControllerLostDetail, time.Now()))
	f.daemon.shims.mu.Unlock()

	f.daemon.releaseShimIfLive(f.id, f.controller, shimStreamEnded)

	mu.Lock()
	defer mu.Unlock()
	if len(terminals) != 1 {
		t.Fatalf("the controller-loss path reported %d terminal facts, want the vanished lineage's attestation: %+v",
			len(terminals), terminals)
	}
	got := terminals[0]
	if got.Absent == nil || !got.Absent.Complete() {
		t.Fatalf("report carried absent=%+v, want a complete attestation", got.Absent)
	}
	if got.Identity != vanished || got.ShimID != vanishedShimID || got.ProcessEpoch != vanishedEpoch {
		t.Errorf("report named %+v, want the vanished incarnation %s/%s/%d",
			got, vanished, vanishedShimID, vanishedEpoch)
	}
	// The fixture's own shim is ALIVE and its controller was just lost. It must
	// stay quarantined, charged, and un-attested — the discharge is for the
	// lineage that is gone, not for every lineage this path touches.
	projected := f.daemon.QuarantinedSessions()
	if len(projected) != 1 || projected[0].Identity() != f.id {
		t.Fatalf("quarantine projection = %+v, want exactly the live lineage retained", projected)
	}
	if !projected[0].ConsumesCapacity {
		t.Error("the live lineage stopped consuming capacity; its harness is still held")
	}
}
