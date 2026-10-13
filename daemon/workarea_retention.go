// Package daemon workarea_retention.go — retention sweep wiring retained
// roots to the archive registry.
//
// The retention budget expires retained roots (session directories left
// behind after their session ended) and drops archives past their maximum
// age. RunRetentionPass resolves the daemon's authored retention settings
// and runs one bounded pass: roots older than the maximum age, then least
// recently used first while retained roots exceed the disk budget, then
// archive drops. Each transition carries a receipt naming the policy and
// the measured free-space change.
//
// The pass never makes a session wait: leased, parked, adopted and
// unprovable roots are untouched, and keeper failures degrade to keeping
// the root.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/runtime/worktree"
)

// RetentionSweepInterval is the period between retention passes on a
// running daemon. Expiry is age- and budget-driven, not latency-driven:
// a daily pass bounds the work while keeping the budget tight.
const RetentionSweepInterval = 24 * time.Hour

// RetentionPassReport summarizes one retention sweep: the per-transition
// receipts and the measured free-space change.
type RetentionPassReport struct {
	// Expired counts retained roots the pass disposed of (archived or
	// destroyed).
	Expired int
	// ArchivesDropped counts archives the pass dropped.
	ArchivesDropped int
	// BytesReclaimed is the measured free-space change across the pass.
	BytesReclaimed int64
	// At is when the pass completed.
	At time.Time
}

// RunRetentionPass runs one bounded retention pass over the worktree
// parent and archive registry: it expires retained roots per the daemon's
// authored retention settings and drops archives past their maximum age.
// A nil registry or nil manager is a no-op returning a zero report.
func RunRetentionPass(ctx context.Context, manager *worktree.Manager, registry *WorkareaArchiveRegistry, settings RetentionSettings) (*RetentionPassReport, error) {
	report := &RetentionPassReport{}
	if manager == nil || registry == nil {
		return report, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	policy := worktree.RetentionPolicy{
		MaxAgeDays: settings.MaxAgeDays, MaxDiskBytes: settings.MaxBytes,
		ArchiveMaxAgeDays: settings.ArchiveMaxAgeDays,
	}
	result, err := manager.ExpireRetainedRoots(ctx, policy, func(_ context.Context, archiveID, _ string) error {
		if err := registry.DropArchive(archiveID); err != nil {
			return fmt.Errorf("retention: drop archive: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, receipt := range result.Receipts {
		switch receipt.Disposition {
		case worktree.RetentionArchived, worktree.RetentionDestroyed:
			report.Expired++
		case worktree.RetentionArchiveDropped:
			report.ArchivesDropped++
		}
		report.BytesReclaimed += receipt.BytesReclaimed
	}
	report.At = time.Now().UTC()
	slog.Info("retention sweep complete",
		"expired", report.Expired,
		"archivesDropped", report.ArchivesDropped,
		"bytesReclaimed", report.BytesReclaimed,
	)
	return report, nil
}

// RunRetentionSweeper runs the retention sweep until ctx is cancelled,
// starting with one pass and repeating every interval. A sweep failure is
// reported through onError (when non-nil) and never stops the loop: the
// next interval retries.
func RunRetentionSweeper(ctx context.Context, manager *worktree.Manager, registry *WorkareaArchiveRegistry, settings RetentionSettings, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = RetentionSweepInterval
	}
	sweep := func() {
		if _, err := RunRetentionPass(ctx, manager, registry, settings); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
