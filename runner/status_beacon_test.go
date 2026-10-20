package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// beaconStep extracts the beaconStep key from a /progress body.
func beaconStep(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		WorkerID   string `json:"workerId"`
		BeaconStep string `json:"beaconStep"`
		BeaconAt   string `json:"beaconAt"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("beacon body is not JSON: %v", err)
	}
	return decoded.BeaconStep
}

// waitBeacons waits until the platform double has recorded at least n
// beacon posts, failing the test on timeout.
func waitBeacons(t *testing.T, platform *recordingPlatformServer, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := platform.beaconPosts(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d beacons; got %d", n, len(platform.beaconPosts()))
	return nil
}

// beaconSteps returns every recorded beaconStep in order.
func beaconSteps(t *testing.T, bodies [][]byte) []string {
	t.Helper()
	steps := make([]string, 0, len(bodies))
	for _, b := range bodies {
		steps = append(steps, beaconStep(t, b))
	}
	return steps
}

// newTestBeaconReporter builds a reporter pointed at the test double,
// mirroring the runner's own wiring (session, worker, platform URL,
// double's HTTP client, real clock).
func newTestBeaconReporter(t *testing.T, qw QueuedWork, interval time.Duration, platform *recordingPlatformServer) *statusBeaconReporter {
	t.Helper()
	r := newStatusBeaconReporterWith(qw, interval, time.Now, nil, platform.Client(), nil)
	if r == nil {
		t.Fatal("reporter is nil for valid work")
	}
	return r
}

// TestRun_PostsSpawningStreamingAndTerminalBeacons drives a one-turn
// scripted session and asserts the beacon cadence: a spawning beacon at
// run start, a streaming beacon once the provider's init event lands, and
// a terminal completed beacon when the run ends.
func TestRun_PostsSpawningStreamingAndTerminalBeacons(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)
	platform.beaconNotify = make(chan struct{}, 64)

	res, _ := runScriptedSession(t, scriptedSession{
		platform:              platform,
		workType:              "qa",
		statusBeaconInterval:  5 * time.Millisecond,
		turns:                 []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
		stepHeartbeatInterval: 24 * time.Hour, // silence the liveness beat; beacons only
	})

	bodies := waitBeacons(t, platform, 3)
	steps := beaconSteps(t, bodies)
	if steps[0] != StatusBeaconSpawning {
		t.Fatalf("first beacon = %q; want %q (spawn-time report)", steps[0], StatusBeaconSpawning)
	}
	foundStreaming := false
	for _, s := range steps[1:] {
		if s == StatusBeaconStreaming {
			foundStreaming = true
			break
		}
	}
	if !foundStreaming {
		t.Fatalf("no streaming beacon after the init event: %q", steps)
	}
	if last := steps[len(steps)-1]; last != "completed" {
		t.Fatalf("terminal beacon = %q; want completed (res.Status=%q FailureMode=%q)", last, res.Status, res.FailureMode)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (FailureMode=%q Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	// Every beacon authenticates as the session's worker and carries an
	// RFC3339 stamp: decode one fully.
	var decoded struct {
		WorkerID   string `json:"workerId"`
		BeaconStep string `json:"beaconStep"`
		BeaconAt   string `json:"beaconAt"`
	}
	if err := json.Unmarshal(bodies[0], &decoded); err != nil {
		t.Fatalf("beacon body is not JSON: %v", err)
	}
	if decoded.WorkerID != "worker-1" {
		t.Errorf("workerId = %q; want worker-1 (session owner)", decoded.WorkerID)
	}
	if _, err := time.Parse(time.RFC3339, decoded.BeaconAt); err != nil {
		t.Errorf("beaconAt %q not RFC3339: %v", decoded.BeaconAt, err)
	}
}

// TestRun_BeaconOnlyPostsCarryNoDisplayMessage pins the platform contract:
// a beacon post must never carry a display message, or the companion route
// would stream it to the activity buffer and forward a permanent comment —
// one comment per step change.
func TestRun_BeaconOnlyPostsCarryNoDisplayMessage(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)

	runScriptedSession(t, scriptedSession{
		platform:              platform,
		workType:              "qa",
		statusBeaconInterval:  5 * time.Millisecond,
		turns:                 []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
		stepHeartbeatInterval: 24 * time.Hour,
	})

	for _, body := range waitBeacons(t, platform, 1) {
		if strings.Contains(string(body), `"message"`) {
			t.Fatalf("beacon body carries a message key (would forward a comment): %s", body)
		}
	}
}

// TestRun_StepChangeTriggerAndBoundedRate pins the two halves of the
// cadence contract: repeated reports of the same step post once (the
// reporter dedupes), while distinct steps inside one interval coalesce to
// the latest (at most one POST per interval).
func TestRun_StepChangeTriggerAndBoundedRate(t *testing.T) {
	t.Parallel()

	t.Run("repeat step posts once", func(t *testing.T) {
		t.Parallel()
		platform := newRecordingPlatformServer(t)
		qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-REPEAT")}
		qw.SessionID = "sess-beacon-repeat"
		qw.WorkerID = "worker-1"
		qw.PlatformURL = platform.URL
		r := newTestBeaconReporter(t, qw, 24*time.Hour, platform)
		ctx := context.Background()
		r.Report(ctx, "streaming")
		r.Report(ctx, "streaming")
		r.Report(ctx, "streaming")
		// Async worker: wait for the one post, then prove no second
		// arrives (the repeats coalesce into the parked duplicate,
		// which the timer drops as already-posted).
		waitBeacons(t, platform, 1)
		time.Sleep(100 * time.Millisecond)
		r.Close(ctx, "streaming") // already posted: no second POST
		time.Sleep(100 * time.Millisecond)
		if got := len(platform.beaconPosts()); got != 1 {
			t.Fatalf("repeat step posted %d times; want 1", got)
		}
	})

	t.Run("burst coalesces to latest within one interval", func(t *testing.T) {
		t.Parallel()
		platform := newRecordingPlatformServer(t)
		qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-BURST")}
		qw.SessionID = "sess-beacon-burst"
		qw.WorkerID = "worker-1"
		qw.PlatformURL = platform.URL
		r := newTestBeaconReporter(t, qw, 200*time.Millisecond, platform)
		defer r.Close(context.Background(), "completed")
		ctx := context.Background()
		r.Report(ctx, "streaming") // enqueued immediately (interval elapsed)
		r.Report(ctx, "steering")  // parks as pending
		r.Report(ctx, "backstop")  // supersedes steering
		// Async worker: the first post lands shortly; the coalesced
		// latest wins the timer slot.
		bodies := waitBeacons(t, platform, 2)
		steps := beaconSteps(t, bodies)
		if steps[0] != "streaming" || steps[1] != "backstop" {
			t.Fatalf("coalesced steps = %q; want [streaming backstop] (latest wins)", steps)
		}
	})
}

// TestRun_TerminalBeaconFiresDespiteRateGate pins the terminal guarantee:
// Close posts the terminal step even when the interval gate would
// otherwise swallow it, so the descriptor never keeps a mid-run phase.
func TestRun_TerminalBeaconFiresDespiteRateGate(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-TERMINAL")}
	qw.SessionID = "sess-beacon-terminal"
	qw.WorkerID = "worker-1"
	qw.PlatformURL = platform.URL
	r := newTestBeaconReporter(t, qw, 24*time.Hour, platform)
	ctx := context.Background()
	r.Report(ctx, "streaming")
	r.Close(ctx, "completed")
	bodies := waitBeacons(t, platform, 2)
	steps := beaconSteps(t, bodies)
	if steps[len(steps)-1] != "completed" {
		t.Fatalf("terminal beacon = %q; want completed", steps[len(steps)-1])
	}
}

// TestRun_BeaconFailureNeverFailsTheSession pins the failure-tolerance
// contract end to end: with the progress endpoint answering 500 on every
// post, the run still completes and carries no beacon-shaped error.
func TestRun_BeaconFailureNeverFailsTheSession(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)
	platform.mu.Lock()
	platform.beaconFail = true
	platform.mu.Unlock()

	res, _ := runScriptedSession(t, scriptedSession{
		platform:              platform,
		workType:              "qa",
		statusBeaconInterval:  5 * time.Millisecond,
		turns:                 []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
		stepHeartbeatInterval: 24 * time.Hour,
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed despite the beacon outage (FailureMode=%q Error=%q)",
			res.Status, res.FailureMode, res.Error)
	}
	if strings.Contains(res.Error, "beacon") || strings.Contains(res.Error, "progress") {
		t.Fatalf("Error carries beacon text %q; a beacon outage must leave no trace on the envelope", res.Error)
	}
}

// TestRun_FailedSessionPostsFailedTerminalBeacon pins the terminal step
// for the failure path: a session that fails posts "failed", not the
// mid-run phase it died in.
func TestRun_FailedSessionPostsFailedTerminalBeacon(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)

	res, _ := runScriptedSession(t, scriptedSession{
		platform:              platform,
		workType:              "qa",
		statusBeaconInterval:  5 * time.Millisecond,
		turns:                 []verdictScriptTurn{{crash: true}},
		stepHeartbeatInterval: 24 * time.Hour,
	})
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed", res.Status)
	}
	bodies := waitBeacons(t, platform, 2)
	steps := beaconSteps(t, bodies)
	if last := steps[len(steps)-1]; last != "failed" {
		t.Fatalf("terminal beacon = %q; want failed (steps=%q)", last, steps)
	}
}

// TestTerminalBeaconStep pins the Result→step mapping: the Status word
// when set, "failed" when a failure is recorded but the status has not
// caught up, teardown for a result with nothing terminal on it.
func TestTerminalBeaconStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		res  *Result
		want string
	}{
		{"completed", &Result{Result: agent.Result{Status: "completed"}}, "completed"},
		{"failed", &Result{Result: agent.Result{Status: "failed"}}, "failed"},
		{"stopped", &Result{Result: agent.Result{Status: "stopped"}}, "stopped"},
		{"failure without status", &Result{Result: agent.Result{FailureMode: "spawn"}}, "failed"},
		{"error without status", &Result{Result: agent.Result{Error: "boom"}}, "failed"},
		{"empty result", &Result{}, "teardown"},
		{"nil result", nil, "teardown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := terminalBeaconStep(tc.res); got != tc.want {
				t.Fatalf("terminalBeaconStep = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestNewStatusBeaconReporter_DisabledAndDegenerate pins the constructor
// gates: a negative interval disables beacons (nil reporter, nil-safe
// Report/Close), work without a session id or platform URL degrades
// to nil rather than a reporter that can never post, and a local-transport
// session stays quiet on its stub platform URL — the fence
// TestLocalTransportRunLoopModeBoundary guards would otherwise see beacon
// traffic as unexpected network.
func TestNewStatusBeaconReporter_DisabledAndDegenerate(t *testing.T) {
	t.Parallel()

	var nilReporter *statusBeaconReporter
	// Nil-safe: these must not panic.
	nilReporter.Report(context.Background(), "streaming")
	nilReporter.Close(context.Background(), "completed")

	// The negative-interval kill switch must be pinned with work that could
	// otherwise post: a valid session id + platform URL means only the
	// interval gate decides, so flipping the gate (e.g. `< 0` to `< -1`)
	// surfaces as a non-nil reporter.
	platform := newRecordingPlatformServer(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-OFF")}
	qw.SessionID = "sess-beacon-off"
	qw.WorkerID = "worker-1"
	qw.PlatformURL = platform.URL
	if got := newStatusBeaconReporterWith(qw, -1, time.Now, nil, platform.Client(), nil); got != nil {
		t.Fatal("negative interval must disable beacons (nil reporter)")
	}
	// Zero interval with valid work enables with the default cadence.
	if got := newStatusBeaconReporterWith(qw, 0, time.Now, nil, platform.Client(), nil); got == nil {
		t.Fatal("zero interval with valid work must enable beacons")
	} else {
		got.Close(context.Background(), "completed")
	}
	empty := QueuedWork{}
	if got := newStatusBeaconReporter(empty, 0, time.Now, nil); got != nil {
		t.Fatal("work without session/platform must degrade to nil, not an unpostable reporter")
	}
	local := QueuedWork{QueuedWork: queuedWorkBase("BEACON-LOCAL")}
	local.SessionID = "sess-beacon-local"
	local.WorkerID = "worker-1"
	local.PlatformURL = "http://127.0.0.1:7734/api/daemon/local"
	local.runtimeTransport = RuntimeTransportLocal
	if got := newStatusBeaconReporter(local, 0, time.Now, nil); got != nil {
		t.Fatal("local-transport work must stay beacon-quiet (nil reporter), not post to the stub URL")
	}
}

// TestRun_BeaconFailedPostBacksOff pins the failure backoff: with the
// progress endpoint answering 500, rapid step changes after the first
// failed post emit no further POST attempts until the interval elapses —
// a down endpoint is retried at most once per interval, not once per step.
// The double counts every /progress attempt (success or 500) separately
// from the successful-post bodies waitBeacons reads.
func TestRun_BeaconFailedPostBacksOff(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/progress") {
			attempts.Add(1)
			_, _ = io.ReadAll(r.Body)
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(failing.Close)
	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-BACKOFF")}
	qw.SessionID = "sess-beacon-backoff"
	qw.WorkerID = "worker-1"
	qw.PlatformURL = failing.URL
	r := newStatusBeaconReporterWith(qw, 24*time.Hour, time.Now, nil, failing.Client(), nil)
	if r == nil {
		t.Fatal("reporter is nil for valid work")
	}
	ctx := context.Background()
	r.Report(ctx, "streaming") // attempt 1: fails, consumes the interval slot
	r.Report(ctx, "steering")  // must not retry: interval not elapsed
	r.Report(ctx, "backstop")  // must not retry: interval not elapsed
	// Async worker: wait for the single failed attempt to land, then
	// prove the gated retries never follow. The deadline waits past any
	// coalescing timer a regression might arm on the gated path.
	deadline := time.Now().Add(10 * time.Second)
	for attempts.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("beacon attempts after a failed post = %d; want 1 (backoff holds the interval slot)", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("beacon attempts after quiet wait = %d; want still 1 (no deferred retry)", got)
	}
	r.Close(ctx, "completed") // terminal bypasses the gate: attempt 2
	deadline = time.Now().Add(15 * time.Second)
	for attempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("beacon attempts = %d; want 2 (1 failed mid-run + terminal bypass)", got)
	}
}

// TestRun_TerminalBeaconWithRateGateAtRunLevel pins the run-level wiring:
// with a 24h interval every mid-run step after the first parks, so only
// the deferred Close can deliver the terminal step. Replacing that Close
// with a gated Report would lose the terminal beacon after the process
// exits — this test goes RED on that mutant.
func TestRun_TerminalBeaconWithRateGateAtRunLevel(t *testing.T) {
	t.Parallel()

	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		platform := newRecordingPlatformServer(t)
		res, _ := runScriptedSession(t, scriptedSession{
			platform:              platform,
			workType:              "qa",
			statusBeaconInterval:  24 * time.Hour,
			turns:                 []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
			stepHeartbeatInterval: 24 * time.Hour,
		})
		if res.Status != "completed" {
			t.Fatalf("Status = %q; want completed", res.Status)
		}
		bodies := waitBeacons(t, platform, 2)
		steps := beaconSteps(t, bodies)
		if last := steps[len(steps)-1]; last != "completed" {
			t.Fatalf("terminal beacon = %q; want completed (steps=%q)", last, steps)
		}
		terminals := 0
		for _, s := range steps {
			switch s {
			case "completed", "failed", "stopped":
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("terminal-word beacons = %d (%q); want exactly 1", terminals, steps)
		}
	})

	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		platform := newRecordingPlatformServer(t)
		res, _ := runScriptedSession(t, scriptedSession{
			platform:              platform,
			workType:              "qa",
			statusBeaconInterval:  24 * time.Hour,
			turns:                 []verdictScriptTurn{{crash: true}},
			stepHeartbeatInterval: 24 * time.Hour,
		})
		if res.Status != "failed" {
			t.Fatalf("Status = %q; want failed", res.Status)
		}
		bodies := waitBeacons(t, platform, 2)
		steps := beaconSteps(t, bodies)
		if last := steps[len(steps)-1]; last != "failed" {
			t.Fatalf("terminal beacon = %q; want failed (steps=%q)", last, steps)
		}
		terminals := 0
		for _, s := range steps {
			switch s {
			case "completed", "failed", "stopped":
				terminals++
			}
		}
		if terminals != 1 {
			t.Fatalf("terminal-word beacons = %d (%q); want exactly 1", terminals, steps)
		}
	})
}

// TestReporter_TerminalIsLast pins the ordering guarantee: a pending
// mid-run step must never land after the terminal step, and a Report
// after Close is dropped. Deleting the timer stop / pending clear or the
// closed flag leaves this RED.
func TestReporter_TerminalIsLast(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)
	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-ORDER")}
	qw.SessionID = "sess-beacon-order"
	qw.WorkerID = "worker-1"
	qw.PlatformURL = platform.URL
	r := newTestBeaconReporter(t, qw, 500*time.Millisecond, platform)
	ctx := context.Background()
	r.Report(ctx, "streaming")         // enqueued immediately
	r.Report(ctx, "steering")          // parks as pending (timer 500ms)
	time.Sleep(100 * time.Millisecond) // timer not yet fired
	r.Close(ctx, "completed")          // stops the timer, drops pending, posts terminal last
	r.Report(ctx, "steering")          // after Close: dropped
	// Wait past the original timer fire time: a mutant that deletes
	// the timer stop / pending clear (or the closed check in the
	// callback) posts the stale steering step after the terminal.
	time.Sleep(800 * time.Millisecond)
	bodies := platform.beaconPosts()
	steps := beaconSteps(t, bodies)
	if len(steps) != 2 || steps[0] != "streaming" || steps[1] != "completed" {
		t.Fatalf("steps = %q; want [streaming completed] (pending dropped, terminal last)", steps)
	}
}

// TestReporter_HungEndpointNeverBlocksCaller pins the non-blocking
// guarantee with a hung progress endpoint and a client with no Timeout
// (which would block forever on a synchronous post). Report and the
// provider event consumer return in milliseconds; Close returns within
// its bounded drain deadline. A 2s sleep at the top of the post path
// leaves every other beacon suite green but fails this one.
func TestReporter_HungEndpointNeverBlocksCaller(t *testing.T) {
	t.Parallel()
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/progress") {
			// Hang past the worker's per-post timeout (5s) and the
			// Report bound (2s), proving the caller never waits on the
			// endpoint. Eight seconds keeps server Close fast: the
			// handler exits on its own shortly after the client gives
			// up, instead of leaking a 30s sleep past Cleanup.
			select {
			case <-time.After(8 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hanging.Close)
	// No Timeout: without the worker's own deadline this client blocks
	// until the endpoint answers.
	noTimeout := &http.Client{}
	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-HUNG")}
	qw.SessionID = "sess-beacon-hung"
	qw.WorkerID = "worker-1"
	qw.PlatformURL = hanging.URL
	r := newStatusBeaconReporterWith(qw, 30*time.Second, time.Now, nil, noTimeout, nil)
	if r == nil {
		t.Fatal("reporter is nil for valid work")
	}
	ctx := context.Background()
	start := time.Now()
	r.Report(ctx, "streaming")
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Report blocked %v on a hung endpoint; want <2s (async mailbox)", took)
	}
	// The provider event hot path shares Report: an InitEvent must not
	// stall event draining on a hung endpoint.
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	start = time.Now()
	h.runner.observeEvent(agent.InitEvent{SessionID: "hung-init"}, obs, t.TempDir(), QueuedWork{}, r)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("observeEvent blocked %v on a hung endpoint; want <2s", took)
	}
	start = time.Now()
	r.Close(ctx, "completed")
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("Close blocked %v on a hung endpoint; want within the bounded drain", took)
	}
}

// TestRun_HungBeaconEndpointStillCompletes pins failure tolerance at the
// run level: with /progress hanging 8s per post, the scripted session
// still completes — the worker's timeout, not the endpoint, bounds the
// wait. Before the async rework this run took 3x the hang per post.
func TestRun_HungBeaconEndpointStillCompletes(t *testing.T) {
	t.Parallel()
	platform := newRecordingPlatformServer(t)
	platform.mu.Lock()
	platform.beaconHang = 15 * time.Second
	platform.mu.Unlock()
	start := time.Now()
	res, _ := runScriptedSession(t, scriptedSession{
		platform:              platform,
		workType:              "qa",
		statusBeaconInterval:  5 * time.Millisecond,
		turns:                 []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
		stepHeartbeatInterval: 24 * time.Hour,
	})
	took := time.Since(start)
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed despite the hung beacon endpoint", res.Status)
	}
	if took > 30*time.Second {
		t.Fatalf("run took %v with a hung beacon endpoint; want bounded (<30s)", took)
	}
}

// TestRun_SteeringAndBackstopBeacons pins the two step reports the
// cadence tests never touch: deleting either Report stays green without
// these. Both drive the production run loop.
func TestRun_SteeringAndBackstopBeacons(t *testing.T) {
	t.Run("steering", func(t *testing.T) {
		platform := newRecordingPlatformServer(t)
		res, _ := runScriptedSession(t, scriptedSession{
			platform:              platform,
			workType:              "development",
			repository:            followUpRepository,
			pulls:                 map[int]string{7: pullAtSessionCommit},
			statusBeaconInterval:  5 * time.Millisecond,
			stepHeartbeatInterval: 24 * time.Hour,
			turns: []verdictScriptTurn{
				{text: "Implementation done.\nWORK_RESULT: passed"},
				{text: "Committing now."},
				{text: "Opened " + followUpPR},
			},
		})
		if !res.SteeringTriggered {
			t.Fatal("SteeringTriggered = false; want the pull-request nudge (steering beacon precondition)")
		}
		bodies := waitBeacons(t, platform, 3)
		steps := beaconSteps(t, bodies)
		found := false
		for _, s := range steps {
			if s == StatusBeaconSteering {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no steering beacon in %q", steps)
		}
	})

	t.Run("backstop", func(t *testing.T) {
		platform := newRecordingPlatformServer(t)
		res, _ := runScriptedSession(t, scriptedSession{
			platform:              platform,
			workType:              "development",
			repository:            followUpRepository,
			backstop:              true,
			skipSteering:          true,
			statusBeaconInterval:  5 * time.Millisecond,
			stepHeartbeatInterval: 24 * time.Hour,
			turns: []verdictScriptTurn{{
				manifest: passedManifest,
				text:     "WORK_RESULT:passed",
				files:    map[string]string{"fix.go": "package fix\n"},
			}},
		})
		_ = res
		bodies := waitBeacons(t, platform, 3)
		steps := beaconSteps(t, bodies)
		found := false
		for _, s := range steps {
			if s == StatusBeaconBackstop {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no backstop beacon in %q", steps)
		}
	})
}
