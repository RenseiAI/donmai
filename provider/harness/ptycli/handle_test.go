package ptycli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// requireShell skips the test on platforms/environments this pty-spawning
// package's tests cannot run on (mirrors the guard in
// provider/harness/agycli's handle_test.go).
func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// collectEvents drains a Handle's Events channel until it closes or the
// deadline fires.
func collectEvents(t *testing.T, h *Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out collecting events; got %d so far: %#v", len(out), out)
			return out
		}
	}
}

func TestSpawn_SuccessExit_EmitsInitThenSuccessResult(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "exit 0"}, agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	evs := collectEvents(t, h)
	if len(evs) != 2 {
		t.Fatalf("expected exactly 2 events (Init, Result), got %d: %#v", len(evs), evs)
	}
	if _, ok := evs[0].(agent.InitEvent); !ok {
		t.Errorf("event[0] = %#v, want InitEvent", evs[0])
	}
	res, ok := evs[1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event[1] = %#v, want ResultEvent", evs[1])
	}
	if !res.Success {
		t.Errorf("ResultEvent.Success = false, want true (clean exit 0): %#v", res)
	}
}

func TestSpawn_NonZeroExit_EmitsFailureResult(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "exit 7"}, agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	evs := collectEvents(t, h)
	if len(evs) != 2 {
		t.Fatalf("expected exactly 2 events, got %d: %#v", len(evs), evs)
	}
	res, ok := evs[1].(agent.ResultEvent)
	if !ok {
		t.Fatalf("event[1] = %#v, want ResultEvent", evs[1])
	}
	if res.Success {
		t.Errorf("ResultEvent.Success = true, want false (exit 7)")
	}
	if res.ErrorSubtype != "nonzero_exit" {
		t.Errorf("ErrorSubtype = %q, want %q", res.ErrorSubtype, "nonzero_exit")
	}
}

func TestHandle_SessionID_AlwaysEmpty(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "exit 0"}, agent.Spec{
		Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if got := h.SessionID(); got != "" {
		t.Errorf("SessionID() = %q, want empty", got)
	}
}

func TestHandle_Inject_ReturnsErrUnsupported(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "sleep 5"}, agent.Spec{
		Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	err = h.Inject(context.Background(), "hello")
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("Inject error = %v, want wrapping agent.ErrUnsupported", err)
	}
}

func TestHandle_Stop_Idempotent(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "sleep 30"}, agent.Spec{
		Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Errorf("second Stop (must be idempotent): %v", err)
	}

	// The events channel must still close cleanly after Stop.
	collectEvents(t, h)
}

func TestHandle_InteractiveSession_AndEmitMarker(t *testing.T) {
	t.Parallel()
	requireShell(t)

	h, err := Spawn(context.Background(), "sh", []string{"-c", "sleep 5"}, agent.Spec{
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 100, Rows: 40},
	}, ptyNoticeManifest())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	var ic agent.InteractiveCapable = h
	sess := ic.InteractiveSession()
	if sess == nil {
		t.Fatal("InteractiveSession() returned nil")
	}

	if err := h.EmitMarker(agent.MarkerApprovalPending); err != nil {
		t.Errorf("EmitMarker(%s): %v", agent.MarkerApprovalPending, err)
	}
	if err := h.EmitMarker(agent.MarkerApprovalResolved); err != nil {
		t.Errorf("EmitMarker(%s): %v", agent.MarkerApprovalResolved, err)
	}

	scr, _, err := sess.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if scr.Cols != 100 || scr.Rows != 40 {
		t.Errorf("Snapshot geometry = %dx%d, want 100x40", scr.Cols, scr.Rows)
	}
}

func TestSpawn_MissingBinary_WrapsErrSpawnFailed(t *testing.T) {
	t.Parallel()

	_, err := Spawn(context.Background(), "this-binary-does-not-exist-ptycli-test", nil, agent.Spec{
		Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest())
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("error = %v, want wrapping agent.ErrSpawnFailed", err)
	}
}

func TestSpawnWithCleanup_OwnsResourceAcrossEveryTerminalPath(t *testing.T) {
	requireShell(t)

	t.Run("child exit", func(t *testing.T) {
		var calls atomic.Int32
		h, err := SpawnWithCleanup(context.Background(), "sh", []string{"-c", "exit 0"}, agent.Spec{
			Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
		}, ptyNoticeManifest(), func() error { calls.Add(1); return nil })
		if err != nil {
			t.Fatal(err)
		}
		collectEvents(t, h)
		if got := calls.Load(); got != 1 {
			t.Fatalf("cleanup calls = %d, want 1", got)
		}
		if err := h.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("cleanup calls after Stop = %d, want 1", got)
		}
	})

	t.Run("explicit stop", func(t *testing.T) {
		var calls atomic.Int32
		h, err := SpawnWithCleanup(context.Background(), "sh", []string{"-c", "sleep 30"}, agent.Spec{
			Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
		}, ptyNoticeManifest(), func() error { calls.Add(1); return nil })
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		collectEvents(t, h)
		if got := calls.Load(); got != 1 {
			t.Fatalf("cleanup calls = %d, want 1", got)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		h, err := SpawnWithCleanup(ctx, "sh", []string{"-c", "sleep 30"}, agent.Spec{
			Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
		}, ptyNoticeManifest(), func() error { calls.Add(1); return nil })
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		collectEvents(t, h)
		if got := calls.Load(); got != 1 {
			t.Fatalf("cleanup calls = %d, want 1", got)
		}
	})

	t.Run("spawn failure", func(t *testing.T) {
		var calls atomic.Int32
		_, err := SpawnWithCleanup(context.Background(), "this-binary-does-not-exist-ptycli-cleanup-test", nil, agent.Spec{
			Interactive: &agent.InteractiveSpec{},
		}, ptyNoticeManifest(), func() error { calls.Add(1); return nil })
		if !errors.Is(err, agent.ErrSpawnFailed) {
			t.Fatalf("error = %v, want ErrSpawnFailed", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("cleanup calls = %d, want 1", got)
		}
	})
}

func TestSpawnWithCleanup_ErrorMakesTerminalResultFail(t *testing.T) {
	requireShell(t)
	want := errors.New("cleanup refused")
	h, err := SpawnWithCleanup(context.Background(), "sh", []string{"-c", "exit 0"}, agent.Spec{
		Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest(), func() error { return want })
	if err != nil {
		t.Fatal(err)
	}
	events := collectEvents(t, h)
	result, ok := events[len(events)-1].(agent.ResultEvent)
	if !ok || result.Success || result.ErrorSubtype != "cleanup_failed" {
		t.Fatalf("terminal result = %#v, want cleanup_failed", events[len(events)-1])
	}
	if err := h.Stop(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Stop error = %v, want cleanup error", err)
	}
}

func TestSpawnWithCleanup_CanDeleteOwnedFile(t *testing.T) {
	requireShell(t)
	path := t.TempDir() + "/owned"
	if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := SpawnWithCleanup(context.Background(), "sh", []string{"-c", "exit 0"}, agent.Spec{
		Cwd: t.TempDir(), Interactive: &agent.InteractiveSpec{},
	}, ptyNoticeManifest(), func() error { return os.Remove(path) })
	if err != nil {
		t.Fatal(err)
	}
	collectEvents(t, h)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned file still exists or stat failed: %v", err)
	}
}

func TestEnvSlice_SortedDeterministic(t *testing.T) {
	t.Parallel()

	got := envSlice(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if len(got) != len(want) {
		t.Fatalf("envSlice len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("envSlice[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if got := envSlice(nil); got != nil {
		t.Errorf("envSlice(nil) = %v, want nil", got)
	}
}

func TestEnvSlice_StripsRunnerOnlyControls(t *testing.T) {
	t.Parallel()

	got := envSlice(map[string]string{
		"ATTACH_TOKEN":      "secret",
		"ATTACH_TOKEN_FILE": "/tmp/token",
		"ATTACH_URL":        "wss://relay.invalid/v1/rooms/room-1",
		"SAFE":              "kept",
	})
	want := []string{"SAFE=kept"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("envSlice leaked runner-only controls: got %v, want %v", got, want)
	}
}
