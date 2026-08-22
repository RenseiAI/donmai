package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// This file is the daemon-side half of the ADR-2026-08-17 acceptance suite.
//
// # Why a real second process
//
// The claim under test is a PRODUCTION claim: the daemon's interactive spawn
// path launches a per-session shim, hands it the launch contract, adopts it over
// shimwire, and then drives stop/input/output and terminal cleanup through that
// connection. A fake in-process launcher would prove the plumbing compiles and
// nothing about the ownership move, because the shim and the daemon would share
// a lifetime by construction — the exact coupling §D1 exists to remove.
//
// So the shim here is a genuinely separate OS process: this test binary
// re-executed in helper mode, reading the SAME launch environment the daemon
// composes for a real worker, owning a real PTY and a real harness child.
//
// # What this suite does NOT claim
//
// It does not claim the ADR's first proof obligation — the real launchd/systemd
// smoke against the INSTALLED service. A setsid-only implementation can pass
// every subprocess test here and still be reaped by a service manager; that
// fixture belongs to the smokes repo. What is proven here is everything above
// the service-manager boundary.

const envDaemonShimHelper = "DONMAI_TEST_DAEMON_SESSION_SHIM_HELPER"

// interactiveFixture is a real line-oriented interactive program: it blocks on
// terminal input and answers each line, so a round trip proves BOTH directions
// are live through the adopted connection.
const interactiveFixture = `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`

// TestMain routes this binary into shim-helper mode when the daemon's own launch
// contract is present in the environment. The daemon composes that environment;
// the helper consumes it exactly as a real worker's ptycli driver does.
func TestMain(m *testing.M) {
	if os.Getenv(envDaemonShimHelper) == "1" {
		os.Exit(runDaemonShimHelper())
	}
	os.Exit(m.Run())
}

func runDaemonShimHelper() int {
	launch, err := sessionshim.LaunchFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: launch env:", err)
		return 1
	}
	// The worker resolves the same <parent>/<sessionID> leaf the daemon
	// publishes, which is what makes the adoption-time workarea comparison a real
	// check rather than a value compared against itself (§D7).
	workarea := filepath.Join(os.Getenv("DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT"), launch.Identity.SessionID)
	shim, err := sessionshim.StartFromEnv(launch,
		ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}, workarea)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: start:", err)
		return 1
	}
	<-shim.Done()
	return 0
}

// shimSpawnFixture is a daemon configured to launch interactive sessions through
// a real shim process.
type shimSpawnFixture struct {
	daemon         *Daemon
	registry       string
	workareaParent string
	orgID          string
	events         *shimEventRecorder
}

// shimEventRecorder is the composing carrier's seat: it receives exactly what
// the daemon forwards from each adopted session. Reading output through this
// hook rather than off the controller's channel is not a convenience — the
// daemon's own consumer is the sole reader of that channel, and a test that
// raced it would be proving something no production consumer could rely on.
type shimEventRecorder struct {
	mu   sync.Mutex
	seen map[string]*strings.Builder
	seq  map[string]uint64
	gaps map[string]int
}

func newShimEventRecorder() *shimEventRecorder {
	return &shimEventRecorder{
		seen: map[string]*strings.Builder{},
		seq:  map[string]uint64{},
		gaps: map[string]int{},
	}
}

func (r *shimEventRecorder) record(id sessionshim.Identity, ev sessionshim.ControllerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	switch ev.Kind {
	case sessionshim.EventOutput:
		b, ok := r.seen[key]
		if !ok {
			b = &strings.Builder{}
			r.seen[key] = b
		}
		b.Write(ev.Data)
		if ev.Seq > r.seq[key] {
			r.seq[key] = ev.Seq
		}
	case sessionshim.EventGap:
		r.gaps[key]++
	}
}

func (r *shimEventRecorder) output(id sessionshim.Identity) (string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	if b, ok := r.seen[key]; ok {
		return b.String(), r.seq[key]
	}
	return "", r.seq[key]
}

func newShimSpawnFixture(t *testing.T) *shimSpawnFixture {
	t.Helper()
	// A Unix socket path has a short platform limit (as low as 104 bytes), and
	// t.TempDir() bakes the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	events := newShimEventRecorder()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:  true,
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   60 * time.Second,
			OnSessionEvent:  events.record,
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 4,
		//nolint:gosec // G204: os.Args[0] is this test binary; helper mode is selected by env
		WorkerCommand:     []string{os.Args[0], "-test.run", "TestMain"},
		WorktreeParentDir: dir,
		BaseEnv: map[string]string{
			envDaemonShimHelper: "1",
			"DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT": dir,
			// The helper is a race-instrumented copy of this test binary. It has
			// three goroutines of real work, so extra Ps buy nothing and cost CPU
			// every other package's process-spawn deadlines need under a parallel
			// `go test -race ./...`.
			"GOMAXPROCS": "2",
		},
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	f := &shimSpawnFixture{daemon: d, registry: dir + "/registry", workareaParent: dir, orgID: "test-org", events: events}
	t.Cleanup(func() {
		for _, id := range d.AdoptedSessionShims() {
			_ = d.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown)
		}
		d.ReleaseAdoptedSessionShims()
	})
	return f
}

// interactiveSpec is a session spec whose run mode selects shim ownership.
func (f *shimSpawnFixture) interactiveSpec(sessionID string) SessionSpec {
	return SessionSpec{
		SessionID:  sessionID,
		ProjectID:  "p1",
		Repository: "https://example.invalid/x/y",
		Mode:       interactiveRunMode,
	}
}

func (f *shimSpawnFixture) identity(sessionID string) sessionshim.Identity {
	return sessionshim.Identity{OrgID: f.orgID, SessionID: sessionID}
}

// exchange writes one line into the adopted session and waits for the harness's
// answer to reach the carrier hook, returning the highest sequence forwarded.
// It proves BOTH directions are live through the adopted connection.
func (f *shimSpawnFixture) exchange(t *testing.T, id sessionshim.Identity, token string) uint64 {
	t.Helper()
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte(token+"\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	want := "ack:" + token
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, seq := f.events.output(id)
		if strings.Contains(out, want) {
			return seq
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := f.events.output(id)
	t.Fatalf("timed out waiting for %q to reach the carrier; saw %q", want, out)
	return 0
}

// TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt is the V16 anchor for the
// SELECTION rule: an interactive session accepted by this daemon must be owned
// by a separate shim process this daemon then adopts, not by a daemon-parented
// child.
//
// Bypassing the selection in shimOwnsSession (returning false, or dropping the
// Mode check) turns this test RED at the AdoptedSessionShims assertion, because
// the session then takes the ordinary direct-child path and no shim exists.
func TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	handle, err := d.spawner.AcceptWork(f.interactiveSpec("sess-launch"))
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-launch")

	adopted := d.AdoptedSessionShims()
	if len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("AdoptedSessionShims = %+v, want exactly [%s] — the interactive spawn did not go through a shim", adopted, id)
	}

	// §D1: the daemon holds no process bookkeeping for a shim-owned session. A
	// spawner entry would mean a second owner, and the reaper attached to it
	// would end the session on the next daemon shutdown.
	if _, tracked := d.spawner.sessions[id.SessionID]; tracked {
		t.Fatal("the spawner registered a direct-child entry for a shim-owned session; the daemon must not be a second owner")
	}

	// The published PID is the HARNESS, reported by the shim — not the shim
	// process and not a daemon child. It is the value an unchanged-across-restart
	// comparison is made against (§D2).
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if handle.PID == 0 || handle.PID != entry.controller.HarnessIdentity().PID {
		t.Fatalf("handle PID = %d, want the shim-reported harness pid %d", handle.PID, entry.controller.HarnessIdentity().PID)
	}
	if handle.PID == os.Getpid() {
		t.Fatal("handle PID is this process; the harness must run under the shim, not the controller")
	}
	if !entry.controller.HarnessSurvived() {
		t.Fatal("HarnessSurvived = false immediately after launch")
	}
	if entry.controller.Generation() == 0 {
		t.Fatal("adoption did not commit a controller generation; single-controller fencing is unenforced")
	}
	if !entry.launched {
		t.Error("the launched shim is not marked as launched by this daemon")
	}
}

// TestShimOwnedSessionIsVisibleInSessionsAndCapacity pins §D7 at the two
// surfaces that decide whether more work is sent here.
func TestShimOwnedSessionIsVisibleInSessionsAndCapacity(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-visible")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	sessions := d.ActiveSessions()
	if len(sessions) != 1 || sessions[0].SessionID != "sess-visible" {
		t.Fatalf("ActiveSessions = %+v, want the shim-owned session listed", sessions)
	}
	if sessions[0].State != SessionRunning {
		t.Errorf("shim-owned session state = %q, want %q", sessions[0].State, SessionRunning)
	}
	if sessions[0].WorktreePath == "" {
		t.Error("shim-owned session has no worktree path; a local reader cannot find its .agent state")
	}

	active, interactive := d.spawnerActiveSessionCounts()
	if active != 1 || interactive != 1 {
		t.Fatalf("occupancy = (active %d, interactive %d), want (1, 1)", active, interactive)
	}
	if d.SessionShimOccupancy() != 1 {
		t.Fatalf("SessionShimOccupancy = %d, want 1", d.SessionShimOccupancy())
	}
}

// TestAdoptedSessionAcceptsInputAndProducesOutput proves both directions of the
// terminal are live through the adopted connection — the concrete meaning of
// "the session works after adoption" (§D5).
func TestAdoptedSessionAcceptsInputAndProducesOutput(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-io")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-io")

	first := f.exchange(t, id, "one")
	second := f.exchange(t, id, "two")
	if second <= first {
		t.Fatalf("host output sequence did not advance: %d then %d; the shim is the sole allocator and must be monotonic", first, second)
	}

	// Geometry is a mutating frame and must be accepted under this daemon's
	// committed generation.
	if err := d.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, 100, 40, 0, 0); err != nil {
		t.Fatalf("ResizeAdoptedSessionShim: %v", err)
	}
}

// TestStopAndTerminalCleanupAfterAdoption is the terminal half of the contract:
// a generation-fenced Stop reaches the shim, the harness is reaped, capacity is
// released, and the durable tombstone is consumed rather than left behind (§D8,
// §D10).
func TestStopAndTerminalCleanupAfterAdoption(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-stop")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-stop")
	f.exchange(t, id, "alive")

	// The control-API id is a bare session id; it must reach the shim rather than
	// falling through to a direct-child stop that would find nothing.
	if !d.StopSession(id.SessionID) {
		t.Fatal("StopSession did not route to the adopted shim")
	}

	waitFor(t, 30*time.Second, "the adopted session to reach a terminal outcome", func() bool {
		return d.SessionShimOccupancy() == 0
	})

	if got := d.ActiveSessions(); len(got) != 0 {
		t.Fatalf("ActiveSessions after terminal outcome = %+v, want empty", got)
	}

	// The tombstone was the proof of death; once the outcome is durably recorded
	// it is disposed. Both halves matter — an undisposed tombstone would keep the
	// session in reconciliation forever.
	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Both halves, and in this order: the liveness claim is withdrawn first, then
	// the proof is disposed. Asserting only that the tombstone is gone would pass
	// against a registry still advertising a live shim for a session that ended.
	waitFor(t, 15*time.Second, "the discovery record to be withdrawn", func() bool {
		_, err := registry.Get(id)
		return err != nil
	})
	waitFor(t, 15*time.Second, "the tombstone to be disposed after the outcome was recorded", func() bool {
		_, err := registry.GetTombstone(id)
		return err != nil
	})
	if proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID); !proof.Proves() {
		t.Fatal("no terminal proof retained for a session this daemon watched end; the outcome would be unresolvable")
	}
}

// TestShimOwnershipIsOffByDefaultForInteractiveSessions pins §D11's migration
// law at the selection rule: shipping this code must not change who owns a
// terminal until an operator says so.
func TestShimOwnershipIsOffByDefaultForInteractiveSessions(t *testing.T) {
	t.Parallel()

	d := New(Options{SkipRegistration: true})
	spec := SessionSpec{SessionID: "s1", Mode: interactiveRunMode}
	if d.shimOwnsSession(spec) {
		t.Fatal("shim ownership is on by default; §D11 step 1 ships the protocol with ownership OFF")
	}
	handle, err := d.launchSessionShim(spec, ProjectConfig{ID: "p"}, nil)
	if err != nil {
		t.Fatalf("launchSessionShim with ownership disabled: %v", err)
	}
	if handle != nil {
		t.Fatalf("launchSessionShim returned %+v with ownership disabled, want nil (fall through to the direct path)", handle)
	}
}

// TestOnlyInteractiveSessionsAreShimOwned pins the other half of the selection
// rule. The first delivery is interactive-only (§D11): a headless worker that
// dies with its daemon is re-dispatched, a human's terminal is not.
func TestOnlyInteractiveSessionsAreShimOwned(t *testing.T) {
	t.Parallel()

	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableOwnership: true},
	})
	cases := []struct {
		mode string
		want bool
	}{
		{mode: interactiveRunMode, want: true},
		{mode: "", want: false},
		{mode: "interview", want: false},
		{mode: "batch", want: false},
	}
	for _, tc := range cases {
		if got := d.shimOwnsSession(SessionSpec{SessionID: "s", Mode: tc.mode}); got != tc.want {
			t.Errorf("shimOwnsSession(mode=%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestLaunchFailureFailsTheAcceptClosed proves the spawner does not quietly
// demote a shim-owned session to a daemon-parented child when the launch fails.
// A silent demotion would produce exactly the terminal-dies-on-upgrade behaviour
// the ADR exists to remove, with nothing in the logs saying so.
func TestLaunchFailureFailsTheAcceptClosed(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   750 * time.Millisecond,
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that exits immediately and never publishes a record.
		WorkerCommand:     []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	handle, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: "sess-fail", ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	})
	if err == nil {
		t.Fatalf("AcceptWork = %+v, nil; a shim launch that never announced itself must fail the accept", handle)
	}
	if !strings.Contains(err.Error(), "session shim") {
		t.Errorf("error %q does not name the shim launch as the cause", err)
	}
	if got := d.SessionShimOccupancy(); got != 0 {
		t.Errorf("SessionShimOccupancy after a failed launch = %d, want 0", got)
	}
	if _, tracked := d.spawner.sessions["sess-fail"]; tracked {
		t.Error("a failed shim launch left a direct-child entry behind")
	}
}

// TestLaunchEnvironmentCarriesTheContractAndNoSecrets pins §D6's no-secret bound
// at the carrier the daemon actually writes. The launch env is visible in the
// process table, so anything secret here would be leaked by the carrier itself.
func TestLaunchEnvironmentCarriesTheContractAndNoSecrets(t *testing.T) {
	t.Parallel()

	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/reg",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 3,
	}
	pairs := envPairs(launch.Env())
	joined := strings.Join(pairs, "\n")
	for _, key := range sessionshim.EnvKeys() {
		if !strings.Contains(joined, key+"=") {
			t.Errorf("launch environment is missing %s", key)
		}
	}
	// Sorted, so a spawn environment is byte-stable across runs.
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1] > pairs[i] {
			t.Fatalf("launch environment is not sorted: %q before %q", pairs[i-1], pairs[i])
		}
	}
	// Round trip: the only producer and the only consumer must agree.
	env := launch.Env()
	got, err := sessionshim.LaunchFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LaunchFromEnv on the daemon's own overlay: %v", err)
	}
	if got.Identity != launch.Identity || got.RegistryDir != launch.RegistryDir ||
		got.ProcessEpoch != launch.ProcessEpoch || got.Orphan != launch.Orphan {
		t.Fatalf("round trip = %+v, want %+v", got, launch)
	}
}

// TestForwardedSequenceIsRecordedNotAllocated pins §D5's division of labour: the
// daemon records the highest sequence it forwarded so a LATER adoption can
// resume from it, and never allocates one itself.
func TestForwardedSequenceIsRecordedNotAllocated(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-seq")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-seq")
	f.exchange(t, id, "seq")

	waitFor(t, 20*time.Second, "the daemon to record a forwarded sequence", func() bool {
		return d.SessionShimForwardedSeq(id.OrgID, id.SessionID) > 0
	})
	if got := d.SessionShimForwardedSeq(id.OrgID, "not-a-session"); got != 0 {
		t.Errorf("forwarded sequence for an unknown session = %d, want 0", got)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// TestAdmissionRefusesWorkAgainstShimHeldCapacity closes the double-booking gap
// §D7 opens at the ADMISSION boundary rather than the advertisement one.
//
// A shim-owned session never enters the spawner's own registry by design, so a
// host that reported its occupancy honestly and then admitted against its
// direct-child count alone would still accept work it has no core to run.
func TestAdmissionRefusesWorkAgainstShimHeldCapacity(t *testing.T) {
	t.Parallel()

	held := 0
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
		ExternalOccupancy:     func() int { return held },
	})
	s.Resume()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.DrainContext(ctx)
	})

	spec := func(id string) SessionSpec {
		return SessionSpec{SessionID: id, ProjectID: "p1", Repository: "https://example.invalid/x/y"}
	}

	// Two shims already hold this host's whole envelope. Nothing may be admitted,
	// even though the spawner parents no children at all.
	held = 2
	if _, err := s.AcceptWork(spec("s1")); err == nil {
		t.Fatal("AcceptWork succeeded while shims held every slot on the host")
	}

	// One slot frees up; exactly one session fits, and the next is refused.
	held = 1
	if _, err := s.AcceptWork(spec("s2")); err != nil {
		t.Fatalf("AcceptWork with one free slot: %v", err)
	}
	if _, err := s.AcceptWork(spec("s3")); err == nil {
		t.Fatal("AcceptWork admitted a third session into a two-slot host")
	}
}
