package daemon

// Provenance: shim-discovery-deadline-2026-09-02 — grep a build for this marker
// to prove it carries the live-process discovery extension and the stop that
// follows it.
//
// # THE STRAND THIS UNDOES
//
// Measured live on a loaded host, four interactive launches inside two minutes:
// one launch's worker (a harness whose first run is slow to bootstrap) had not
// published its discovery record when the launch clock expired ~31s after spawn,
// so the daemon logged "launched worker never published a discovery record",
// failed the accept — and LEFT THE PROCESS RUNNING. It was not a launch that
// produced nothing: the worker went on to run the entire prompt un-adopted, send
// its messages and exit, ending as a defunct entry under the daemon that spawned
// it. The identical launch shape adopted in ~30s when it ran alone, and the same
// model adopted fine two seconds earlier under the same load, so the bound was
// the binding constraint, not the harness.
//
// Two independent defects, both covered here:
//
//  1. The wait gave up at a bound sized for an ordinary cold start even though
//     the launched process was demonstrably still alive and still working. While
//     the pid lives there is something to wait FOR, so the wait now runs to a
//     longer bound derived from the same launch timeout, and a pid that DIES
//     ends it immediately instead of burning the rest of that budget.
//  2. Giving up abandoned a live process. Nothing else would ever stop it: the
//     launch never reached trackLaunchedShim, so no adopted-set pass can see it,
//     and a worker that never published a record has not armed sessionshim's own
//     orphan clock either. The accept-work error was therefore false about the
//     host from the moment it was returned.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// fakeShimLaunchProcess is a scripted stand-in for the OS-backed launch control.
//
// It models the one property that makes the real one reap-aware: a probe of a
// process that has exited REAPS it, so "not alive" and "reaped" are the same
// observation rather than two steps a caller could forget to pair.
type fakeShimLaunchProcess struct {
	mu sync.Mutex
	// aliveUntil is when the scripted process exits. Zero means it never does.
	aliveUntil time.Time
	// aliveErr scripts an UNPROBEABLE process: neither alive nor gone.
	aliveErr error
	stopErr  error
	probes   int
	stops    int
	reaps    int
}

func (p *fakeShimLaunchProcess) Alive() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	if p.aliveErr != nil {
		return false, p.aliveErr
	}
	if p.aliveUntil.IsZero() || time.Now().Before(p.aliveUntil) {
		return true, nil
	}
	p.reaps++
	return false, nil
}

func (p *fakeShimLaunchProcess) StopAndReap() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	if p.stopErr != nil {
		return p.stopErr
	}
	p.reaps++
	return nil
}

func (p *fakeShimLaunchProcess) counts() (probes, stops, reaps int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes, p.stops, p.reaps
}

// launchProcessCapture records the identity of the process a REAL launch
// spawned, while delegating every operation to the ordinary OS-backed control.
//
// It exists because the launched pid is otherwise unobservable to a test: the
// daemon deliberately releases the process (§D1), so nothing in the returned
// handle or the failed accept's error names it — and the whole question these
// end-to-end tests ask is what happened to THAT pid.
type launchProcessCapture struct {
	mu       sync.Mutex
	captured sessionshim.ProcessIdentity
	delegate shimLaunchProcess
}

func (c *launchProcessCapture) identity() sessionshim.ProcessIdentity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}

func (c *launchProcessCapture) control() shimLaunchProcess {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delegate
}

func (c *launchProcessCapture) Alive() (bool, error) { return c.control().Alive() }

func (c *launchProcessCapture) StopAndReap() error { return c.control().StopAndReap() }

// captureLaunchProcess installs the capture seam for one test.
func (f *shimSpawnFixture) captureLaunchProcess(t *testing.T) *launchProcessCapture {
	t.Helper()
	capture := &launchProcessCapture{}
	f.daemon.shims.mu.Lock()
	f.daemon.shims.launchProcess = func(started sessionshim.ProcessIdentity) shimLaunchProcess {
		capture.mu.Lock()
		capture.captured = started
		capture.delegate = newShimLaunchProcess(started)
		capture.mu.Unlock()
		return capture
	}
	f.daemon.shims.mu.Unlock()
	return capture
}

// TestAwaitShimRecordWaitsWhileTheLaunchedProcessLives drives the discovery wait
// through every disposition the live-process extension has to distinguish, on
// test-scaled bounds. The SHAPE is what is pinned — a longer bound while the pid
// lives, an immediate end when it dies, an unchanged happy path — never the
// production-sized durations.
func TestAwaitShimRecordWaitsWhileTheLaunchedProcessLives(t *testing.T) {
	started := sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111}
	tests := []struct {
		name string
		// launchBound is the ordinary launch clock (ctx) this wait runs on.
		launchBound time.Duration
		// publishAfter is when a matching discovery record lands. Zero: never.
		publishAfter time.Duration
		// aliveFor is how long the launched process lives. Zero: forever.
		aliveFor time.Duration
		// liveBound is the TOTAL discovery budget while the process is alive.
		liveBound         time.Duration
		wantRecord        bool
		wantAbandonedLive bool
		// maxElapsed asserts the wait ENDED on the disposition under test rather
		// than by outlasting some other bound.
		maxElapsed time.Duration
	}{
		{
			// The measured strand: the record lands well after the launch clock
			// (and after the short post-deadline grace poll) while the worker is
			// still alive and still bootstrapping. Pre-fix this failed the accept.
			name:         "a record that lands after the launch bound while the pid is alive is adopted",
			launchBound:  30 * time.Millisecond,
			publishAfter: 400 * time.Millisecond,
			liveBound:    4 * time.Second,
			wantRecord:   true,
			maxElapsed:   3 * time.Second,
		},
		{
			// A dead pid is a definite failure NOW: there is nothing left to
			// publish the record, so spending the rest of the live bound would
			// only delay an answer that cannot change.
			name:        "a pid that dies during the extended wait ends it immediately",
			launchBound: 30 * time.Millisecond,
			aliveFor:    100 * time.Millisecond,
			liveBound:   30 * time.Second,
			maxElapsed:  5 * time.Second,
		},
		{
			// The same rule inside the ORDINARY launch clock: a worker that dies
			// during its own cold start (a bad environment, a missing binary)
			// answers the accept now rather than after a launch bound sized for a
			// worker that is still working.
			name:        "a pid that dies inside the launch bound ends the wait immediately",
			launchBound: 30 * time.Second,
			aliveFor:    50 * time.Millisecond,
			liveBound:   120 * time.Second,
			maxElapsed:  10 * time.Second,
		},
		{
			// The live bound is a bound, not a suggestion: a worker that is alive
			// but has still published nothing is abandoned — and classified so the
			// caller knows a live process is what it is walking away from.
			name:              "the live bound expiring with the pid alive reports an abandoned live process",
			launchBound:       30 * time.Millisecond,
			liveBound:         400 * time.Millisecond,
			wantAbandonedLive: true,
			maxElapsed:        4 * time.Second,
		},
		{
			// Requirement: the happy path's latency is untouched. A record inside
			// the launch clock still returns on the ordinary poll, at the ordinary
			// poll interval — the live bound never delays a record that arrives.
			name:         "a record inside the launch bound adopts at the ordinary poll interval",
			launchBound:  4 * time.Second,
			publishAfter: 10 * time.Millisecond,
			liveBound:    30 * time.Second,
			wantRecord:   true,
			maxElapsed:   time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
			dir := t.TempDir()
			registry, err := sessionshim.NewRegistry(dir)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			id := sessionshim.Identity{OrgID: "org-live", SessionID: "session-live"}
			launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
			if tc.publishAfter > 0 {
				rec := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch, started, "shim-live")
				putErr := make(chan error, 1)
				timer := time.AfterFunc(tc.publishAfter, func() { putErr <- registry.Put(rec) })
				t.Cleanup(func() {
					if !timer.Stop() {
						if err := <-putErr; err != nil {
							t.Errorf("Put late record: %v", err)
						}
					}
				})
			}
			process := &fakeShimLaunchProcess{}
			if tc.aliveFor > 0 {
				process.aliveUntil = time.Now().Add(tc.aliveFor)
			}

			ctx, cancel := context.WithTimeout(context.Background(), tc.launchBound)
			defer cancel()
			start := time.Now()
			rec, err := awaitShimRecord(ctx, shimDiscoveryWait{
				registry: registry, id: id, launch: launch, started: started,
				process: process, liveBound: tc.liveBound,
			})
			elapsed := time.Since(start)

			if elapsed > tc.maxElapsed {
				t.Fatalf("the wait took %s, want it decided within %s", elapsed, tc.maxElapsed)
			}
			switch {
			case tc.wantRecord && err != nil:
				t.Fatalf("awaitShimRecord did not adopt the record: %v", err)
			case tc.wantRecord && rec.PID != started.PID:
				t.Fatalf("awaitShimRecord returned %+v, want the launch's own record", rec)
			case !tc.wantRecord && err == nil:
				t.Fatalf("awaitShimRecord returned a record (%+v) for a launch that published none", rec)
			}
			if got := errors.Is(err, errShimDiscoveryAbandonedLiveProcess); got != tc.wantAbandonedLive {
				t.Fatalf("errors.Is(err, errShimDiscoveryAbandonedLiveProcess) = %v, want %v (err: %v)",
					got, tc.wantAbandonedLive, err)
			}
			_, stops, reaps := process.counts()
			if stops != 0 {
				t.Fatalf("the discovery wait stopped the process itself %d times; stopping is the caller's decision", stops)
			}
			if tc.aliveFor > 0 && reaps == 0 {
				t.Fatal("a process that exited during the wait was never reaped by the probe that observed it")
			}
		})
	}
}

// TestAwaitShimRecordWithoutAProcessControlKeepsTheOldBound pins the no-seam
// default: a wait handed no liveness control (or no live bound) cannot know
// whether anything is still running, so it must give up exactly where it always
// did rather than wait longer on an unknowable process.
func TestAwaitShimRecordWithoutAProcessControlKeepsTheOldBound(t *testing.T) {
	withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-no-control", SessionID: "session-no-control"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}

	for _, tc := range []struct {
		name    string
		process shimLaunchProcess
		bound   time.Duration
	}{
		{name: "no process control", process: nil, bound: 30 * time.Second},
		{name: "no live bound", process: &fakeShimLaunchProcess{}, bound: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := awaitShimRecord(ctx, shimDiscoveryWait{
				registry: registry, id: id, launch: launch,
				started: sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111},
				process: tc.process, liveBound: tc.bound,
			})
			if err == nil {
				t.Fatal("awaitShimRecord succeeded for a launch that never published a record")
			}
			if errors.Is(err, errShimDiscoveryAbandonedLiveProcess) {
				t.Fatalf("a wait with no liveness knowledge claimed the process was alive: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("the wait took %s; with no liveness control it must end on the launch bound", elapsed)
			}
		})
	}
}

// TestAwaitShimRecordAbandonsALiveProcessHoldingAForeignRecord covers the one
// give-up that is NOT a timeout: a record for this identity exists past the
// launch bound but belongs to a different incarnation. Waiting cannot change
// whose record it is, so the wait ends — and because THIS launch's process is
// still alive, it ends as an abandoned live process, which is what makes the
// caller stop it.
func TestAwaitShimRecordAbandonsALiveProcessHoldingAForeignRecord(t *testing.T) {
	withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-foreign", SessionID: "session-foreign"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
	started := sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111}
	foreign := validShimDiscoveryRecord(dir, id, launch.ProcessEpoch,
		sessionshim.ProcessIdentity{PID: started.PID, StartedAt: started.StartedAt + 1}, "shim-foreign")
	// Late, so this exercises the extended wait rather than the ordinary poll:
	// inside the launch clock a record present for this identity is adopted as it
	// always was, and it is Dial that then refuses a stale incarnation.
	putErr := make(chan error, 1)
	timer := time.AfterFunc(150*time.Millisecond, func() { putErr <- registry.Put(foreign) })
	t.Cleanup(func() {
		if !timer.Stop() {
			if err := <-putErr; err != nil {
				t.Errorf("Put foreign record: %v", err)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	process := &fakeShimLaunchProcess{}
	start := time.Now()
	_, err = awaitShimRecord(ctx, shimDiscoveryWait{
		registry: registry, id: id, launch: launch, started: started,
		process: process, liveBound: 30 * time.Second,
	})
	if err == nil {
		t.Fatal("awaitShimRecord adopted a record from a different incarnation")
	}
	if !errors.Is(err, errShimDiscoveryAbandonedLiveProcess) {
		t.Fatalf("a live process abandoned over a foreign record was not classified as abandoned-live: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the wait took %s; a foreign record is decidable immediately", elapsed)
	}
}

// TestAwaitShimRecordLogsAnUnprobeableProcessOnceParLaunch pins the log volume,
// which is a correctness property here rather than tidiness: processIsGone runs
// at shimRecordPollInterval (25ms), so a probe failing for a persistent reason
// would emit thousands of identical lines across one live bound and bury the two
// lines that carry the actual disposition.
func TestAwaitShimRecordLogsAnUnprobeableProcessOnceParLaunch(t *testing.T) {
	withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
	dir := t.TempDir()
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := sessionshim.Identity{OrgID: "org-unprobeable", SessionID: "session-unprobeable"}
	launch := sessionshim.Launch{Identity: id, RegistryDir: dir, ProcessEpoch: 1}
	process := &fakeShimLaunchProcess{aliveErr: errors.New("kern.proc.pid: device error")}

	buf, restore := captureSlog(t)
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = awaitShimRecord(ctx, shimDiscoveryWait{
		registry: registry, id: id, launch: launch,
		started: sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111},
		process: process, liveBound: 400 * time.Millisecond,
	})
	if !errors.Is(err, errShimDiscoveryAbandonedLiveProcess) {
		t.Fatalf("an unprobeable process must be treated as alive and abandoned as such, got: %v", err)
	}
	probes, _, _ := process.counts()
	if probes < 5 {
		t.Fatalf("the wait probed only %d times; this test needs many polls to be meaningful", probes)
	}
	if got := strings.Count(string(buf.snapshot()), "could not probe the launched process"); got != 1 {
		t.Fatalf("the unprobeable-process warning was logged %d times across %d probes, want exactly 1", got, probes)
	}
}

// TestStopAbandonedShimLaunchStopsAndReaps pins the daemon-side half: an
// abandoned live launch is stopped and reaped, and a stop that fails is reported
// rather than swallowed into a false "the spawn was aborted".
func TestStopAbandonedShimLaunchStopsAndReaps(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org-stop", SessionID: "session-stop"}
	tests := []struct {
		name      string
		stopErr   error
		wantStops int
		wantReaps int
	}{
		{name: "a live abandoned launch is stopped and reaped", wantStops: 1, wantReaps: 1},
		{name: "a stop that fails still reports one attempt", stopErr: errors.New("kill: operation not permitted"), wantStops: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New(Options{SkipRegistration: true})
			process := &fakeShimLaunchProcess{stopErr: tc.stopErr}
			d.stopAbandonedShimLaunch(id, sessionshim.ProcessIdentity{PID: 4242, StartedAt: 111},
				process, errShimDiscoveryAbandonedLiveProcess)
			_, stops, reaps := process.counts()
			if stops != tc.wantStops {
				t.Fatalf("StopAndReap called %d times, want %d", stops, tc.wantStops)
			}
			if reaps != tc.wantReaps {
				t.Fatalf("reaps = %d, want %d", reaps, tc.wantReaps)
			}
		})
	}
}

// TestSessionShimLiveDiscoveryTimeoutDerivesFromTheLaunchTimeout pins the bound's
// derivation rather than a literal: an embedder that configures a tighter or
// looser launch timeout gets a live bound in the same proportion, and a test
// fixture's short launch timeout scales the extension down with it.
func TestSessionShimLiveDiscoveryTimeoutDerivesFromTheLaunchTimeout(t *testing.T) {
	tests := []struct {
		name      string
		configure time.Duration
		want      time.Duration
	}{
		{name: "default", want: shimLiveDiscoveryExtensionFactor * defaultShimLaunchTimeout},
		{name: "configured", configure: 10 * time.Second, want: shimLiveDiscoveryExtensionFactor * 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := SessionShimConfig{LaunchTimeout: tc.configure}
			if got := cfg.liveDiscoveryTimeout(); got != tc.want {
				t.Fatalf("liveDiscoveryTimeout() = %s, want %s", got, tc.want)
			}
			if cfg.liveDiscoveryTimeout() <= cfg.launchTimeout() {
				t.Fatalf("the live bound (%s) must exceed the launch bound (%s) or it can never extend anything",
					cfg.liveDiscoveryTimeout(), cfg.launchTimeout())
			}
		})
	}
	// The production default is the number an operator reads in an incident: 30s
	// of ordinary launch clock, 120s of total budget while the worker is alive.
	if got := (SessionShimConfig{}).liveDiscoveryTimeout(); got != 120*time.Second {
		t.Fatalf("the default live discovery bound is %s, want 120s", got)
	}
}

// TestLaunchSessionShimStopsAWorkerThatNeverPublishesADiscoveryRecord is the
// end-to-end proof through the real accept path, with a real spawned worker
// process: a worker that stays alive without ever publishing is abandoned at the
// live bound, STOPPED, and reaped, and the accept still fails.
//
// The pure-function tests above cannot see this: they prove awaitShimRecord
// classifies the disposition, not that launchSessionShim acts on it. The
// measured incident was precisely a correct classification followed by no
// action.
func TestLaunchSessionShimStopsAWorkerThatNeverPublishesADiscoveryRecord(t *testing.T) {
	withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
	f := newShimSpawnFixture(t)
	// 200ms of launch clock, so liveDiscoveryTimeout() derives 800ms — long
	// enough to be a real extension, short enough for a test.
	f.daemon.opts.SessionShim.LaunchTimeout = 200 * time.Millisecond
	// TEST-ONLY: the spawned worker sleeps this long WITHOUT ever calling
	// sessionshim.StartFromEnv, reproducing a bootstrap so slow no discovery
	// record is ever published while the process stays alive. Far longer than
	// the live bound above, so only the daemon's own stop can end it.
	f.daemon.spawner.opts.BaseEnv[envDaemonShimHelperNeverPublishMS] = "60000"
	process := f.captureLaunchProcess(t)

	spec := f.interactiveSpec("never-publishes")
	start := time.Now()
	handle, err := f.daemon.spawner.AcceptWork(spec)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("AcceptWork succeeded (%+v) for a worker that never published a discovery record", handle)
	}
	if !strings.Contains(err.Error(), "discovery record") {
		t.Fatalf("AcceptWork failed with %v, want the discovery-record failure", err)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("AcceptWork gave up after %s, before even the launch bound", elapsed)
	}
	launched := process.identity()
	if launched.PID <= 0 {
		t.Fatal("no launched process identity was captured; the launch never reached the discovery wait")
	}
	if alive, aliveErr := launched.Alive(); alive || aliveErr != nil {
		t.Fatalf("the abandoned worker %s is still alive after the accept failed (err: %v)", launched, aliveErr)
	}
	assertShimChildReaped(t, launched.PID)
}

// TestLaunchSessionShimAbandonsImmediatelyWhenTheWorkerDies is the other end of
// the same seam: a worker that exits without publishing ends the wait on the
// spot rather than holding the accept for its whole launch clock, and leaves no
// defunct process behind.
func TestLaunchSessionShimAbandonsImmediatelyWhenTheWorkerDies(t *testing.T) {
	withShortShimDiscoveryGrace(t, 2, 5*time.Millisecond)
	f := newShimSpawnFixture(t)
	// A deliberately LONG launch clock: if the daemon did not notice the exit it
	// would hold this accept for the full ten seconds (and then extend it), so
	// the elapsed assertion below is what discriminates.
	f.daemon.opts.SessionShim.LaunchTimeout = 10 * time.Second
	// TEST-ONLY: exit almost immediately, without publishing anything.
	f.daemon.spawner.opts.BaseEnv[envDaemonShimHelperNeverPublishMS] = "1"
	process := f.captureLaunchProcess(t)

	spec := f.interactiveSpec("dies-before-publishing")
	start := time.Now()
	handle, err := f.daemon.spawner.AcceptWork(spec)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("AcceptWork succeeded (%+v) for a worker that exited without publishing", handle)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("AcceptWork took %s for a worker that exited immediately; the dead pid must end the wait "+
			"rather than serve out the launch clock", elapsed)
	}
	launched := process.identity()
	if launched.PID <= 0 {
		t.Fatal("no launched process identity was captured; the launch never reached the discovery wait")
	}
	assertShimChildReaped(t, launched.PID)
}
