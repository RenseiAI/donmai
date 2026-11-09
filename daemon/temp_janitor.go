package daemon

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/RenseiAI/donmai/tempown"
)

// tempJanitorTimeout bounds one janitor pass over one root.
const tempJanitorTimeout = 30 * time.Second

// tempJanitorInterval is how often the daemon re-runs the janitor after the
// startup pass, so leftovers from workers or probes that crash mid-uptime are
// reclaimed within about an hour instead of surviving until the next restart.
const tempJanitorInterval = time.Hour

// TempJanitorFunc is the shape of one janitor pass; tempown.Sweep is the
// production implementation.
type TempJanitorFunc func(context.Context, tempown.SweepOptions) tempown.SweepReport

// tempJanitorRoots names the directories the daemon's own probes and the
// workers it spawns create temp artefacts under: the system temp folder the
// daemon itself sees. A worker whose TMPDIR is a per-session directory puts
// its artefacts there instead, and that directory is removed with the session.
func tempJanitorRoots() []string {
	return []string{filepath.Clean(os.TempDir())}
}

// sweepTempArtifacts runs one janitor pass. It removes only artefacts
// tempown recorded (owner-tagged names) whose owner is gone and that have sat
// idle past the grace period; see tempown.Sweep. A nil Options.TempJanitor
// makes this a no-op, so a daemon built for a test never reads or deletes
// anything under the real temp folder.
func (d *Daemon) sweepTempArtifacts(ctx context.Context) {
	sweep := d.opts.TempJanitor
	if sweep == nil {
		return
	}
	for _, root := range tempJanitorRoots() {
		sweep(ctx, tempown.SweepOptions{Root: root, MaxDuration: tempJanitorTimeout})
	}
}

// startTempJanitorLoop re-runs sweepTempArtifacts hourly until ctx ends. A
// nil Options.TempJanitor starts nothing.
func (d *Daemon) startTempJanitorLoop(ctx context.Context) {
	if d.opts.TempJanitor == nil {
		return
	}
	go d.runTempJanitorLoop(ctx, tempJanitorInterval)
}

func (d *Daemon) runTempJanitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.sweepTempArtifacts(ctx)
		}
	}
}
