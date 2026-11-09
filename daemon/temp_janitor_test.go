package daemon

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/tempown"
)

// TestDaemonStartRunsTempJanitorOverTheTempRoot pins the caller: Start runs
// the configured janitor once per root, and the root is the temp folder the
// daemon's own probes and spawned workers create artefacts under.
func TestDaemonStartRunsTempJanitorOverTheTempRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var roots []string
	d := newTestDaemonForSweep(t, Options{
		TempJanitor: func(_ context.Context, opts tempown.SweepOptions) tempown.SweepReport {
			roots = append(roots, opts.Root)
			return tempown.SweepReport{}
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(roots) != 1 || roots[0] != filepath.Clean(tmp) {
		t.Fatalf("janitor roots = %q, want [%q]", roots, filepath.Clean(tmp))
	}
}

func TestDaemonTempJanitorLoopTicksAndStopsWithContext(t *testing.T) {
	var calls atomic.Int32
	d := newTestDaemonForSweep(t, Options{
		TempJanitor: func(context.Context, tempown.SweepOptions) tempown.SweepReport {
			calls.Add(1)
			return tempown.SweepReport{}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.runTempJanitorLoop(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("janitor loop did not stop with its context")
	}
	if calls.Load() < 2 {
		t.Fatalf("janitor loop ran %d passes, want at least 2", calls.Load())
	}
}
