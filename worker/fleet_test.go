package worker

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// helperEnvVar is read by TestHelperProcess to decide whether it should
// run as a fleet child rather than as a real test.
const helperEnvVar = "GO_WANT_WORKER_HELPER"

// helperModeEnvVar controls the helper's SIGTERM behaviour:
//   - ""           : default, exit on SIGTERM
//   - "ignoreterm" : ignore SIGTERM; only SIGKILL can stop the child
const helperModeEnvVar = "WORKER_HELPER_MODE"

// newHelperFleet constructs a Fleet whose children re-execute the
// current test binary as a helper process. TestMain detects the
// GO_WANT_WORKER_HELPER env var and short-circuits to runHelperChild,
// so the child never actually runs the test suite; the -test.run filter
// here is defensive (matches nothing) in case a future code path lets
// m.Run execute.
func newHelperFleet(mode string) *Fleet {
	f := NewFleet(os.Args[0], []string{"-test.run=^$"})
	env := append([]string{}, os.Environ()...)
	env = append(env, helperEnvVar+"=1")
	if mode != "" {
		env = append(env, helperModeEnvVar+"="+mode)
	}
	f.Env = env
	return f
}

// waitForState polls Status until the predicate returns true or the
// timeout elapses, returning the final snapshot either way.
func waitForState(t *testing.T, f *Fleet, timeout time.Duration, pred func([]WorkerProcess) bool) []WorkerProcess {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var snap []WorkerProcess
	for {
		snap = f.Status()
		if pred(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setFleetPIDTempFile redirects FleetPIDPath to a per-test file so the
// fleet tests never touch the user's real ~/.config directory.
func setFleetPIDTempFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.pids")
	t.Setenv(fleetPIDEnv, path)
	return path
}

func TestFleet_StartN(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 3); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background(), 2*time.Second) })

	snap := f.Status()
	if len(snap) != 3 {
		t.Fatalf("Status len = %d, want 3", len(snap))
	}
	for i, w := range snap {
		if w.PID == 0 {
			t.Errorf("snap[%d].PID = 0", i)
		}
		if w.State != WorkerStateRunning {
			t.Errorf("snap[%d].State = %q, want running", i, w.State)
		}
		if w.StartedAt.IsZero() {
			t.Errorf("snap[%d].StartedAt is zero", i)
		}
	}
}

func TestFleet_ScaleUp(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background(), 2*time.Second) })

	if err := f.Scale(ctx, 4); err != nil {
		t.Fatalf("Scale up: %v", err)
	}
	snap := f.Status()
	if len(snap) != 4 {
		t.Fatalf("Status len = %d, want 4", len(snap))
	}
	for i, w := range snap {
		if w.State != WorkerStateRunning {
			t.Errorf("snap[%d].State = %q, want running", i, w.State)
		}
	}
}

func TestFleet_ScaleDown(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 4); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background(), 2*time.Second) })

	startPIDs := pidSet(f.Status())

	// Let children install SIGTERM handlers before Scale sends SIGTERM.
	time.Sleep(200 * time.Millisecond)

	if err := f.Scale(ctx, 2); err != nil {
		t.Fatalf("Scale down: %v", err)
	}

	snap := f.Status()
	if len(snap) != 2 {
		t.Fatalf("Status len = %d, want 2", len(snap))
	}
	for _, w := range snap {
		if w.State != WorkerStateRunning {
			t.Errorf("remaining entry state = %q, want running", w.State)
		}
	}
	// Ensure the 2 remaining PIDs are a subset of the original 4.
	remaining := pidSet(snap)
	for pid := range remaining {
		if _, ok := startPIDs[pid]; !ok {
			t.Errorf("remaining pid %d was not in start set", pid)
		}
	}
}

func TestFleet_GracefulStop(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give children a moment to install their SIGTERM handler before we
	// signal them; otherwise SIGTERM arrives before signal.Notify and
	// kills the child via the runtime default handler.
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	if err := f.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("Stop took %v, expected to return well under graceful window", elapsed)
	}

	if got := len(f.Status()); got != 0 {
		t.Errorf("Status len after Stop = %d, want 0", got)
	}
}

func TestFleet_SigkillPath(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("ignoreterm")
	ctx := context.Background()

	if err := f.Start(ctx, 1); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the child time to install signal.Ignore before we SIGTERM it.
	time.Sleep(200 * time.Millisecond)
	graceful := 200 * time.Millisecond
	start := time.Now()
	if err := f.Stop(context.Background(), graceful); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < graceful {
		t.Errorf("Stop returned in %v, expected at least graceful window %v", elapsed, graceful)
	}
	// Upper bound: grace (200ms) + SIGKILL reap (<5s cap) + scheduler slack.
	if elapsed > 5*time.Second {
		t.Errorf("Stop took %v, expected under 5s", elapsed)
	}

	if got := len(f.Status()); got != 0 {
		t.Errorf("Status len after Stop = %d, want 0", got)
	}
}

func TestFleet_PIDFileRoundTrip(t *testing.T) {
	path := setFleetPIDTempFile(t)

	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantPIDs := pidSlice(f.Status())
	gotPIDs, err := ReadFleetPIDs()
	if err != nil {
		t.Fatalf("ReadFleetPIDs: %v", err)
	}
	sort.Ints(wantPIDs)
	sort.Ints(gotPIDs)
	if !reflect.DeepEqual(wantPIDs, gotPIDs) {
		t.Errorf("pid file = %v, want %v", gotPIDs, wantPIDs)
	}

	if err := f.Stop(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pid file still exists after Stop (stat err = %v)", err)
	}

	// ReadFleetPIDs must tolerate a missing file.
	left, err := ReadFleetPIDs()
	if err != nil {
		t.Errorf("ReadFleetPIDs after remove: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("ReadFleetPIDs after remove = %v, want empty", left)
	}
}

func TestFleet_ConcurrentStatusAndScale(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background(), 2*time.Second) })

	// Warm-up so children install signal handlers before any Scale-down
	// in this test sends SIGTERM.
	time.Sleep(200 * time.Millisecond)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader goroutines hammering Status().
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = f.Status()
			}
		}()
	}

	// A single writer goroutine scaling up and down.
	wg.Add(1)
	go func() {
		defer wg.Done()
		targets := []int{4, 2, 3, 2}
		for _, n := range targets {
			if err := f.Scale(ctx, n); err != nil {
				t.Errorf("Scale(%d): %v", n, err)
				return
			}
		}
	}()

	// Let the race detector chew on it for a bit then stop readers.
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Final state should have 2 running workers (last Scale target).
	snap := waitForState(t, f, 2*time.Second, func(s []WorkerProcess) bool {
		n := 0
		for _, w := range s {
			if w.State == WorkerStateRunning {
				n++
			}
		}
		return n == 2
	})
	running := 0
	for _, w := range snap {
		if w.State == WorkerStateRunning {
			running++
		}
	}
	if running != 2 {
		t.Errorf("final running count = %d, want 2 (snap=%+v)", running, snap)
	}
}

func TestFleet_StartInvalidBinary(t *testing.T) {
	setFleetPIDTempFile(t)
	// Use a path that definitely will not exec.
	f := NewFleet("/nonexistent/agentfactory-fleet-binary", nil)
	err := f.Start(context.Background(), 1)
	if err == nil {
		t.Fatal("expected spawn error, got nil")
	}
	// Wrap prefix sanity.
	if got := err.Error(); !strings.HasPrefix(got, "fleet:") {
		t.Errorf("err = %q, want to start with 'fleet:'", got)
	}
}

func TestFleet_NegativeCounts(t *testing.T) {
	setFleetPIDTempFile(t)
	f := NewFleet(os.Args[0], nil)
	if err := f.Start(context.Background(), -1); err == nil {
		t.Error("Start(-1) returned nil, want error")
	}
	if err := f.Scale(context.Background(), -1); err == nil {
		t.Error("Scale(-1) returned nil, want error")
	}
}

func TestFleet_ScaleNoop(t *testing.T) {
	setFleetPIDTempFile(t)
	f := newHelperFleet("")
	ctx := context.Background()

	if err := f.Start(ctx, 2); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Stop(context.Background(), 2*time.Second) })

	// Scale to same size — should be a no-op and still rewrite PID file.
	if err := f.Scale(ctx, 2); err != nil {
		t.Fatalf("Scale noop: %v", err)
	}
	if got := len(f.Status()); got != 2 {
		t.Errorf("Status len = %d, want 2", got)
	}
}

func TestFleet_SetLogger(_ *testing.T) {
	f := NewFleet("/bin/true", nil)
	// Nil must not panic and must leave a usable logger.
	f.SetLogger(nil)
}

func TestFleet_PIDHelpers_Errors(t *testing.T) {
	// Garbage data in the PID file should surface a parse error.
	path := setFleetPIDTempFile(t)
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
	if _, err := ReadFleetPIDs(); err == nil {
		t.Error("ReadFleetPIDs(garbage) returned nil, want parse error")
	}
}

func TestFleet_PIDPathDefault(t *testing.T) {
	// With the override unset, FleetPIDPath should derive from the
	// user config dir and end with agentfactory/fleet.pids.
	t.Setenv(fleetPIDEnv, "")
	got, err := FleetPIDPath()
	if err != nil {
		t.Fatalf("FleetPIDPath: %v", err)
	}
	want := filepath.Join("agentfactory", "fleet.pids")
	if len(got) < len(want) || got[len(got)-len(want):] != want {
		t.Errorf("FleetPIDPath = %q, want suffix %q", got, want)
	}
}

func TestFleet_RemoveMissingPIDFile(t *testing.T) {
	setFleetPIDTempFile(t)
	// No file written yet — Remove must be a no-op.
	if err := RemoveFleetPIDFile(); err != nil {
		t.Errorf("RemoveFleetPIDFile on missing = %v, want nil", err)
	}
}

func TestFleet_WriteThenRead_Empty(t *testing.T) {
	setFleetPIDTempFile(t)
	if err := WriteFleetPIDs(nil); err != nil {
		t.Fatalf("WriteFleetPIDs(nil): %v", err)
	}
	pids, err := ReadFleetPIDs()
	if err != nil {
		t.Fatalf("ReadFleetPIDs: %v", err)
	}
	if len(pids) != 0 {
		t.Errorf("ReadFleetPIDs = %v, want empty", pids)
	}
}

func TestFleet_WritePIDs_MkdirFailure(t *testing.T) {
	// Create a plain file, then point the pid path at a location that
	// would require that file to be treated as a directory. os.MkdirAll
	// surfaces an error that WriteFleetPIDs must wrap with "fleet:".
	dir := t.TempDir()
	blockerFile := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	t.Setenv(fleetPIDEnv, filepath.Join(blockerFile, "child", "fleet.pids"))

	if err := WriteFleetPIDs([]int{1, 2}); err == nil {
		t.Error("WriteFleetPIDs returned nil, want mkdir error")
	} else if !strings.HasPrefix(err.Error(), "fleet:") {
		t.Errorf("err = %q, want 'fleet:' prefix", err.Error())
	}
}

func TestFleet_ReadPIDs_OpenError(t *testing.T) {
	// Create a directory where the pid file path is expected; open()
	// will succeed (directories are openable) but Scanner will return
	// an error — or MacOS will error immediately. Either way we want
	// ReadFleetPIDs to surface a non-nil error, not panic.
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "asdir")
	if err := os.Mkdir(pidPath, 0o750); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	t.Setenv(fleetPIDEnv, pidPath)

	if _, err := ReadFleetPIDs(); err == nil {
		t.Error("ReadFleetPIDs on directory path returned nil, want error")
	}
}

func TestFleet_RemovePIDs_NonNotExistError(t *testing.T) {
	// Try to remove a pid file whose path refers to something inside a
	// non-directory parent — os.Remove should return a non-NotExist
	// error that RemoveFleetPIDFile wraps.
	dir := t.TempDir()
	blockerFile := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	t.Setenv(fleetPIDEnv, filepath.Join(blockerFile, "child.pids"))

	err := RemoveFleetPIDFile()
	// Accept either outcome: some platforms map "parent is not dir"
	// to ENOENT (which this helper treats as a no-op success), others
	// to ENOTDIR (a real error). Both code paths are exercised here.
	if err != nil && !strings.HasPrefix(err.Error(), "fleet:") {
		t.Errorf("err = %q, want 'fleet:' prefix or nil", err.Error())
	}
}

// pidSet collects the PIDs from a Status snapshot into a set for
// subset/equality checks.
func pidSet(snap []WorkerProcess) map[int]struct{} {
	out := make(map[int]struct{}, len(snap))
	for _, w := range snap {
		out[w.PID] = struct{}{}
	}
	return out
}

// pidSlice collects the live PIDs from a Status snapshot into a slice.
func pidSlice(snap []WorkerProcess) []int {
	out := make([]int, 0, len(snap))
	for _, w := range snap {
		if w.State == WorkerStateRunning {
			out = append(out, w.PID)
		}
	}
	return out
}

// TestMain makes sure that when the binary is reinvoked as a fleet helper
// child, we short-circuit out of the regular test runner and just run
// the helper. Without this, every re-exec would run the full test suite
// as a child, which would deadlock (children would try to spawn more
// children).
func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		// Running as a helper child — do NOT run the test suite.
		// Hand control to the helper shim and exit.
		runHelperChild()
		return
	}
	os.Exit(m.Run())
}

// runHelperChild executes the same behaviour as TestHelperProcess but
// from TestMain, avoiding the overhead of the test runner entirely.
func runHelperChild() {
	mode := os.Getenv(helperModeEnvVar)
	if mode == "ignoreterm" {
		// signal.Ignore installs a no-op handler for SIGTERM so the
		// runtime's default "die on SIGTERM" behaviour is suppressed.
		// Only SIGKILL can end this process.
		signal.Ignore(syscall.SIGTERM)
		// A bare select{} would trigger Go's "all goroutines asleep"
		// deadlock detector and kill the process. Loop on a timer so
		// the runtime always has at least one runnable goroutine.
		for {
			time.Sleep(time.Hour)
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	os.Exit(0)
}
