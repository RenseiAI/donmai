package pi

// Handshake retry + startup-failure classification fixtures. These drive
// the production entry point (Spawn against a real subprocess):
// testdata/fakepi for the slow-start paths, shell stand-ins for the
// broken-extension paths. Every failure here stays fail-closed and every
// classification is asserted with errors.Is, not prose.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func requireUnixForHandshakeRetry(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("handshake-retry fixtures drive real subprocesses; unix-only")
	}
}

// buildRetryFakePi compiles testdata/fakepi and returns its path.
func buildRetryFakePi(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "fakepi")
	// #nosec G204 -- goBin resolved from PATH above; args are fixed.
	cmd := exec.Command(goBin, "build", "-o", out, "./testdata/fakepi")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fakepi: %v\n%s", err, combined)
	}
	return out
}

func retryTestProvider(t *testing.T, bin string, timeout time.Duration) *Provider {
	t.Helper()
	p, err := New(Options{
		PiBin:            bin,
		HandshakeTimeout: timeout,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

// fakePiPIDs reads the FAKEPI_PID_FILE startup record.
func fakePiPIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var out []int
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			pid, err := strconv.Atoi(ln)
			if err != nil {
				t.Fatalf("bad pid line %q: %v", ln, err)
			}
			out = append(out, pid)
		}
	}
	return out
}

// livePIDs reports the subset of pids that still have a live process behind
// them (unix kill(pid, 0)).
func livePIDs(pids []int) []int {
	var live []int
	for _, pid := range pids {
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.Signal(0)); err == nil {
				live = append(live, pid)
			}
		}
	}
	return live
}

// TestHandshakeRetry_SlowStartSucceedsOnSecondAttempt (AC-1): a handshake
func TestHandshakeRetry_SlowStartSucceedsOnSecondAttempt(t *testing.T) {
	requireUnixForHandshakeRetry(t)
	bin := buildRetryFakePi(t)
	t.Setenv("FAKEPI_HANDSHAKE_DELAY_MS", "1500")
	t.Setenv("FAKEPI_HANDSHAKE_DELAY_ONCE_FILE", filepath.Join(t.TempDir(), "delay-once"))
	p := retryTestProvider(t, bin, time.Second)
	var spawns atomic.Int32
	p.opts.afterChildStart = func() { spawns.Add(1) }

	h, err := p.Spawn(context.Background(), agent.Spec{Cwd: t.TempDir(), Prompt: "hi"})
	if err != nil {
		t.Fatalf("Spawn should succeed after the retry: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawn attempts = %d, want exactly 2 (one timeout + one retry)", n)
	}
	// The retried session is fully functional: drain to its terminal event.
	if evs := drain(t, h); len(evs) == 0 {
		t.Fatal("no events from the retried session")
	} else if _, ok := evs[len(evs)-1].(agent.ResultEvent); !ok {
		t.Fatalf("terminal = %#v, want a successful ResultEvent", evs[len(evs)-1])
	}
}

// TestHandshakeRetry_TwoTimeoutsAreSlowStart (AC-2): two bounded timeouts
func TestHandshakeRetry_TwoTimeoutsAreSlowStart(t *testing.T) {
	requireUnixForHandshakeRetry(t)
	bin := buildRetryFakePi(t)
	// No once-file: every attempt delays past the deadline.
	t.Setenv("FAKEPI_HANDSHAKE_DELAY_MS", "2500")
	pidFile := filepath.Join(t.TempDir(), "pids")
	t.Setenv("FAKEPI_PID_FILE", pidFile)
	p := retryTestProvider(t, bin, time.Second)
	var spawns atomic.Int32
	p.opts.afterChildStart = func() { spawns.Add(1) }

	h, err := p.Spawn(context.Background(), agent.Spec{Cwd: t.TempDir(), Prompt: "hi"})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("err = %v, want ErrSpawnFailed", err)
	}
	if !errors.Is(err, agent.ErrPolicyExtensionSlowStart) {
		t.Fatalf("err = %v, want the retryable slow-start classification", err)
	}
	if errors.Is(err, agent.ErrPolicyExtensionBroken) {
		t.Fatalf("err = %v, must not carry the broken classification", err)
	}
	if h != nil {
		t.Fatal("Spawn must return a nil Handle on a failed handshake")
	}
	if n := spawns.Load(); n != 2 {
		t.Fatalf("spawn attempts = %d, want exactly 2 bounded timeouts", n)
	}
	if live := livePIDs(fakePiPIDs(t, pidFile)); len(live) != 0 {
		t.Fatalf("owned child processes still alive after failed spawn: %v", live)
	}
}

// brokenFakeBin writes a shell stand-in that exercises one
func brokenFakeBin(t *testing.T, firstLine string) string {
	t.Helper()
	bin := newFakeCatalogProbeBinary(t, "cat <<'FIRST'\n"+firstLine+"FIRST\n"+
		`while IFS= read -r command; do
  case "$command" in
    *'"type":"abort"'*) exit 0 ;;
  esac
done
`)
	return bin
}

// handshakeFrame renders one extension_ui_request frame with the given
// title payload.
func handshakeFrame(title string) string {
	return `{"type":"extension_ui_request","id":"h1","method":"input","title":` +
		strconv.Quote(title) + `,"placeholder":"donmai-policy-v1"}` + "\n"
}

// TestHandshakeRetry_BrokenExtensionNeverRetries (AC-3): a child that
func TestHandshakeRetry_BrokenExtensionNeverRetries(t *testing.T) {
	requireUnixForHandshakeRetry(t)
	cases := map[string]string{
		// Right channel, wrong token: the extension rejects the handshake.
		"rejected": handshakeFrame(`{"donmai":"handshake","token":"not-the-session-token","sha":"` + strings.Repeat("0", 64) + `"}`),
		// Right channel, unparsable kind: malformed protocol data.
		"malformed": handshakeFrame(`{"donmai":"bogus-kind"}`),
	}
	// Plus the exited child: gone, not slow.
	runBroken := func(t *testing.T, fake string) {
		t.Helper()
		p, err := New(Options{
			PiBin:            fake,
			HandshakeTimeout: time.Second,
			handshakeToken:   testHandshakeToken,
			VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		var spawns atomic.Int32
		p.opts.afterChildStart = func() { spawns.Add(1) }

		h, err := p.Spawn(context.Background(), agent.Spec{Cwd: t.TempDir(), Prompt: "hi"})
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Fatalf("err = %v, want ErrSpawnFailed", err)
		}
		if !errors.Is(err, agent.ErrPolicyExtensionBroken) {
			t.Fatalf("err = %v, want the non-retryable broken classification", err)
		}
		if errors.Is(err, agent.ErrPolicyExtensionSlowStart) {
			t.Fatalf("err = %v, must not carry the retryable slow-start classification", err)
		}
		if h != nil {
			t.Fatal("Spawn must return a nil Handle on a broken extension")
		}
		if n := spawns.Load(); n != 1 {
			t.Fatalf("spawn attempts = %d, want exactly 1 (broken extensions never retry)", n)
		}
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			runBroken(t, brokenFakeBin(t, first))
		})
	}
	t.Run("exit", func(t *testing.T) {
		runBroken(t, newFakeCatalogProbeBinary(t, "exit 1\n"))
	})
}

// TestHandshakeTimeout_FloorRejectsUnsafe (AC-4): construction rejects an
func TestHandshakeTimeout_FloorRejectsUnsafe(t *testing.T) {
	requireUnixForHandshakeRetry(t)
	stub := func(context.Context, string) (string, error) { return PinnedVersion, nil }
	for _, timeout := range []time.Duration{-time.Second, time.Millisecond, 100 * time.Millisecond, MinHandshakeTimeout - time.Nanosecond} {
		if _, err := New(Options{skipProcess: true, HandshakeTimeout: timeout, VersionProbe: stub}); !errors.Is(err, agent.ErrProviderUnavailable) {
			t.Fatalf("New(HandshakeTimeout=%s) err = %v, want ErrProviderUnavailable", timeout, err)
		}
	}
	p, err := New(Options{skipProcess: true, HandshakeTimeout: MinHandshakeTimeout, VersionProbe: stub})
	if err != nil {
		t.Fatalf("New at the floor: %v", err)
	}
	if p.opts.HandshakeTimeout != MinHandshakeTimeout {
		t.Fatalf("HandshakeTimeout = %s, want the stated floor %s", p.opts.HandshakeTimeout, MinHandshakeTimeout)
	}
	p, err = New(Options{skipProcess: true, VersionProbe: stub})
	if err != nil {
		t.Fatalf("New with zero timeout: %v", err)
	}
	if p.opts.HandshakeTimeout != DefaultHandshakeTimeout {
		t.Fatalf("HandshakeTimeout = %s, want the default %s", p.opts.HandshakeTimeout, DefaultHandshakeTimeout)
	}
}

// TestHandshakeRetry_BoundedByParentDeadline (AC-4): every wait in the gate
func TestHandshakeRetry_BoundedByParentDeadline(t *testing.T) {
	requireUnixForHandshakeRetry(t)
	bin := buildRetryFakePi(t)
	// Far beyond the parent deadline: without the ctx bound this Spawn
	// would sit out the whole 5 s attempt timeout.
	t.Setenv("FAKEPI_HANDSHAKE_DELAY_MS", "10000")
	p := retryTestProvider(t, bin, 5*time.Second)
	var spawns atomic.Int32
	p.opts.afterChildStart = func() { spawns.Add(1) }

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	h, err := p.Spawn(ctx, agent.Spec{Cwd: t.TempDir(), Prompt: "hi"})
	elapsed := time.Since(start)
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("err = %v, want ErrSpawnFailed", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the parent deadline to bound the wait", err)
	}
	if h != nil {
		t.Fatal("Spawn must return a nil Handle when the parent deadline fires")
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawn attempts = %d, want exactly 1 (no retry past the parent deadline)", n)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("Spawn took %s with a 300 ms parent deadline — the gate outlived its parent", elapsed)
	}
}
