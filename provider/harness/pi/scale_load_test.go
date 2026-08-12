//go:build pi_scale_load

package pi

// This file is the N-instance load-validation harness for pi scale
// hardening: it spins N concurrent pi sessions against a REAL subprocess (never
// the io.Pipe-stubbed skipProcess seam the rest of this package's tests use
// for fast correctness checks) and measures spawn latency and
// inject-round-trip ("steer") latency distributions.
//
// The primary subject is testdata/fakepi — a small Go program built once per
// run that speaks the exact wire shapes handle.go expects (see that file's
// own doc comment) without needing pi/node installed anywhere. This is
// deliberate: a load harness whose only mode requires a third-party npm
// package would either not run in most environments or would silently start
// measuring node/jiti startup instead of donmai's own per-spawn overhead.
// TestScaleLoad_RealBinary_OptionalSample additionally exercises the REAL
// `pi` binary when it happens to be on PATH, and skips cleanly (via the same
// realBinaryAvailable helper real_binary_test.go uses) when it is not — the
// "real pi binary optional" half of the scope item.
//
// # Why this is build-tag gated
//
// N=100 real subprocess spawns is real work: acceptable for a local, opt-in
// run, wasteful as part of the default `make test` gate every CI push pays
// for. Gating behind `pi_scale_load` keeps it out of `go test ./...` while
// staying compiled and vet-checked via `make test-tagged` (its tag is
// registered in the Makefile's test-tagged target — see AGENTS.md's
// "Unregistered suite" guard, internal/testregistration). Run explicitly:
//
//	go test -race -tags pi_scale_load -run TestScaleLoad -v ./provider/harness/pi/...
//
// N defaults to 100 locally; DONMAI_PI_LOAD_N overrides it explicitly, and
// CI=1 (or -short) bounds it to 10 per the scope item's "bounded under CI
// tags" requirement — a CI job that wants to run this suite opts in via the
// tag AND sets CI=1 (already true of most hosted CI) to get the bounded N.
//
// # What "steer latency" measures here
//
// A literal mid-turn `steer` RPC command requires catching a session while
// h.turnInFlight is true — a narrow, inherently racy window that
// TestInject_SteerWhenInFlight_FollowUpWhenIdle already pins deterministically
// via the scripted io.Pipe seam. This harness instead measures the latency
// any caller actually experiences end-to-end: Handle.Inject issued after a
// session's first turn has settled, through to the resulting follow-up
// turn's terminal event — the same round trip runner/steering.go's
// attemptSteering and runner/loop.go's drainMemoryInjects perform in
// production. It is reported as "inject/steer round-trip latency" rather
// than "steer latency" to be precise about what is measured.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const (
	defaultLoadTestN = 100
	ciLoadTestN      = 10

	// spawnLatencyBudget is a regression guard against testdata/fakepi, not a
	// claim about the real `pi` binary. fakepi does no jiti TS compilation,
	// no npm/node startup, and no network I/O, so its own overhead is just
	// this process's fork/exec plus a handful of JSONL round trips — a
	// generous budget here catches a real regression (e.g. an accidental
	// serialization point) without flaking on ordinary CI/host jitter.
	spawnLatencyBudget = 2 * time.Second
)

// buildFakePi compiles testdata/fakepi exactly once per test binary run and
// returns its path. Skips the calling test cleanly (never fails the run) if
// the `go` toolchain is unavailable for any reason — this harness's stub
// binary is a convenience, not a hard dependency the way `make build`'s own
// compiler is.
var (
	fakePiBuildOnce sync.Once
	fakePiPath      string
	fakePiBuildErr  error
)

func buildFakePi(t *testing.T) string {
	t.Helper()
	fakePiBuildOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			fakePiBuildErr = fmt.Errorf("go toolchain not on PATH: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "donmai-fakepi-*")
		if err != nil {
			fakePiBuildErr = fmt.Errorf("mkdir temp: %w", err)
			return
		}
		out := filepath.Join(dir, "fakepi")
		// #nosec G204 -- goBin resolved from PATH above; args are fixed.
		cmd := exec.Command(goBin, "build", "-o", out, "./testdata/fakepi")
		if combined, buildErr := cmd.CombinedOutput(); buildErr != nil {
			fakePiBuildErr = fmt.Errorf("go build testdata/fakepi: %w: %s", buildErr, combined)
			return
		}
		fakePiPath = out
	})
	if fakePiBuildErr != nil {
		t.Skipf("fakepi stub binary unavailable, skipping N-instance load harness: %v", fakePiBuildErr)
	}
	return fakePiPath
}

// loadTestN resolves N per the scope item: DONMAI_PI_LOAD_N always wins;
// otherwise CI/-short bounds to ciLoadTestN, and a full local run defaults to
// defaultLoadTestN.
func loadTestN() int {
	if v := os.Getenv("DONMAI_PI_LOAD_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if os.Getenv("CI") != "" || testing.Short() {
		return ciLoadTestN
	}
	return defaultLoadTestN
}

// concurrencyWidth bounds how many sessions actually run in parallel, so a
// large N on a small box still completes in waves rather than forking N
// processes simultaneously.
func concurrencyWidth(n int) int {
	w := runtime.NumCPU() * 4
	const maxWorkers = 24
	if w > maxWorkers {
		w = maxWorkers
	}
	if w > n {
		w = n
	}
	if w < 1 {
		w = 1
	}
	return w
}

// sessionLatency is one session's measured result.
type sessionLatency struct {
	idx    int
	spawn  time.Duration
	inject time.Duration
	err    error
}

// runOneLoadSession spawns one session against binPath, waits for the first
// turn to settle, injects one follow-up, and waits for that turn to settle
// too — recording both round trips. Safe to call from any goroutine (never
// touches *testing.T's Fail/Fatal surface).
func runOneLoadSession(binPath, cwd string, i int) sessionLatency {
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return sessionLatency{idx: i, err: fmt.Errorf("mkdir cwd: %w", err)}
	}
	p, err := New(Options{
		PiBin:               binPath,
		HandshakeTimeout:    10 * time.Second,
		VersionProbeTimeout: 5 * time.Second,
	})
	if err != nil {
		return sessionLatency{idx: i, err: fmt.Errorf("New: %w", err)}
	}
	spec := agent.Spec{Cwd: cwd, Prompt: "load-test prompt " + strconv.Itoa(i)}

	spawnStart := time.Now()
	h, err := p.Spawn(context.Background(), spec)
	spawnLatency := time.Since(spawnStart)
	if err != nil {
		return sessionLatency{idx: i, err: fmt.Errorf("Spawn: %w", err)}
	}
	defer func() { _ = h.Stop(context.Background()) }()

	if _, err := waitForTerminal(h, 15*time.Second); err != nil {
		return sessionLatency{idx: i, spawn: spawnLatency, err: fmt.Errorf("initial turn: %w", err)}
	}

	injectStart := time.Now()
	if err := h.Inject(context.Background(), "steer nonce "+strconv.Itoa(i)); err != nil {
		return sessionLatency{idx: i, spawn: spawnLatency, err: fmt.Errorf("Inject: %w", err)}
	}
	if _, err := waitForTerminal(h, 15*time.Second); err != nil {
		return sessionLatency{idx: i, spawn: spawnLatency, err: fmt.Errorf("inject turn: %w", err)}
	}
	injectLatency := time.Since(injectStart)

	return sessionLatency{idx: i, spawn: spawnLatency, inject: injectLatency}
}

// waitForTerminal drains h.Events() until a ResultEvent (success) or
// ErrorEvent (failure), or timeout. It never calls into *testing.T so it is
// safe from a worker goroutine.
func waitForTerminal(h agent.Handle, timeout time.Duration) ([]agent.Event, error) {
	var out []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out, fmt.Errorf("event channel closed before a terminal event")
			}
			out = append(out, ev)
			switch e := ev.(type) {
			case agent.ResultEvent:
				return out, nil
			case agent.ErrorEvent:
				return out, fmt.Errorf("ErrorEvent: %s", e.Message)
			}
		case <-deadline:
			return out, fmt.Errorf("timed out after %s waiting for a terminal event", timeout)
		}
	}
}

// TestScaleLoad_NConcurrentSessions_SpawnAndInjectLatency is the primary
// N-instance harness: N concurrent REAL subprocess sessions against
// testdata/fakepi, bounded to concurrencyWidth(N) in flight at once,
// measuring spawn latency and inject/steer round-trip latency per session.
func TestScaleLoad_NConcurrentSessions_SpawnAndInjectLatency(t *testing.T) {
	binPath := buildFakePi(t)
	n := loadTestN()
	workers := concurrencyWidth(n)
	t.Logf("pi_scale_load: N=%d concurrency=%d binary=%s", n, workers, binPath)

	root := t.TempDir()
	results := make([]sessionLatency, n)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			cwd := filepath.Join(root, "sess-"+strconv.Itoa(i))
			results[i] = runOneLoadSession(binPath, cwd, i)
		}(i)
	}
	wg.Wait()

	var spawnLat, injectLat []time.Duration
	failures := 0
	for _, r := range results {
		if r.err != nil {
			t.Errorf("session %d failed: %v", r.idx, r.err)
			failures++
			continue
		}
		spawnLat = append(spawnLat, r.spawn)
		injectLat = append(injectLat, r.inject)
	}
	if failures > 0 {
		t.Fatalf("%d/%d sessions failed; see individual errors above", failures, n)
	}

	logDistribution(t, "spawn latency", spawnLat)
	logDistribution(t, "inject/steer round-trip latency", injectLat)

	if p95 := percentile(spawnLat, 95); p95 > spawnLatencyBudget {
		t.Errorf("p95 spawn latency %s exceeds documented budget %s (testdata/fakepi baseline — a real `pi` binary's jiti-compile + node-startup cost is additional and tracked separately, see doc.go)", p95, spawnLatencyBudget)
	}
}

// TestScaleLoad_RealBinary_OptionalSample is the "real pi binary optional"
// half of the scope item: a SMALL number of real spawns against the actual
// `pi` binary, when present on PATH, to sample real cold-start latency
// (including jiti's TS compile of the boundary extension) rather than
// fakepi's zero-compile baseline. Skips cleanly — not a failure — when `pi`
// or `node` is absent, via the same realBinaryAvailable/newRealBinaryStub
// helpers real_binary_test.go's suite already establishes.
func TestScaleLoad_RealBinary_OptionalSample(t *testing.T) {
	realBinaryAvailable(t) // t.Skip()s cleanly if `pi`/`node` are not on PATH

	stub := newRealBinaryStub(t, realBinaryModel)
	const n = 3 // real, network-optional but still real npm/node overhead — a sample, not a fleet-scale run
	var spawnLat []time.Duration
	for i := 0; i < n; i++ {
		p, err := New(Options{HandshakeTimeout: 15 * time.Second, VersionProbeTimeout: 10 * time.Second})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		spec := realBinarySpec(t.TempDir(), "real-binary load sample", stub.baseURL())
		start := time.Now()
		h, err := p.Spawn(context.Background(), spec)
		lat := time.Since(start)
		if err != nil {
			t.Fatalf("sample %d: Spawn against the real pi binary: %v", i, err)
		}
		if _, err := waitForTerminal(h, 20*time.Second); err != nil {
			t.Fatalf("sample %d: initial turn: %v", i, err)
		}
		_ = h.Stop(context.Background())
		spawnLat = append(spawnLat, lat)
	}
	logDistribution(t, "real-binary spawn latency (sample, includes jiti compile)", spawnLat)
}

// percentile returns the p-th percentile (nearest-rank) of vals. Returns 0
// for an empty input.
func percentile(vals []time.Duration, p int) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

// logDistribution reports min/p50/p95/max/mean for vals via t.Logf.
func logDistribution(t *testing.T, label string, vals []time.Duration) {
	t.Helper()
	if len(vals) == 0 {
		t.Logf("%s: no samples", label)
		return
	}
	sorted := append([]time.Duration(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	mean := sum / time.Duration(len(sorted))
	t.Logf("%s: n=%d min=%s p50=%s p95=%s max=%s mean=%s",
		label, len(sorted), sorted[0], percentile(vals, 50), percentile(vals, 95), sorted[len(sorted)-1], mean)
}
