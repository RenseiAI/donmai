package runner

import (
	"context"
	"encoding/json"
	"strings"
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
		ctx := context.Background()
		r.Report(ctx, "streaming") // posts immediately (interval elapsed)
		r.Report(ctx, "steering")  // parks as pending
		r.Report(ctx, "backstop")  // supersedes steering
		if got := len(platform.beaconPosts()); got != 1 {
			t.Fatalf("burst posted %d times synchronously; want 1 (rest coalesce)", got)
		}
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
// Report/Close), and work without a session id or platform URL degrades
// to nil rather than a reporter that can never post.
func TestNewStatusBeaconReporter_DisabledAndDegenerate(t *testing.T) {
	t.Parallel()

	var nilReporter *statusBeaconReporter
	// Nil-safe: these must not panic.
	nilReporter.Report(context.Background(), "streaming")
	nilReporter.Close(context.Background(), "completed")

	qw := QueuedWork{QueuedWork: queuedWorkBase("BEACON-OFF")}
	if got := newStatusBeaconReporter(qw, -1, time.Now, nil); got != nil {
		t.Fatal("negative interval must disable beacons (nil reporter)")
	}
	empty := QueuedWork{}
	if got := newStatusBeaconReporter(empty, 0, time.Now, nil); got != nil {
		t.Fatal("work without session/platform must degrade to nil, not an unpostable reporter")
	}
}
