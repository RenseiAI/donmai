// Package worktree — retention_disk.go: measured free-space accounting for
// the retention budget.
//
// The retention budget measures the free-space change of each transition,
// not directory sizes: copy-on-write clones and reflinks make directory
// walks over-report what a removal actually frees. Each receipt's
// BytesReclaimed is the filesystem free-space delta across the transition,
// clamped at zero so concurrent writers cannot produce a negative receipt.
package worktree

import (
	"math"
	"syscall"
)

// freeSpace reports the bytes available to unprivileged processes on the
// filesystem holding path. An error leaves the receipt at zero reclaimed
// rather than failing the transition: the disposal already happened, and
// a stat failure must not turn it into a pass failure.
func freeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Clamp the block size into the int64 domain before widening:
	// Statfs_t.Bsize is a fixed-width unsigned field whose width varies
	// by platform, and the G115 overflow check flags a direct widening
	// to uint64 on narrow platforms. The clamp keeps the product in
	// the int64 domain the receipt reports.
	bsize := uint64(stat.Bsize)
	if bsize > uint64(math.MaxInt64) {
		bsize = uint64(math.MaxInt64)
	}
	if stat.Bavail > 0 && bsize > uint64(math.MaxInt64)/stat.Bavail {
		return uint64(math.MaxInt64), nil
	}
	return stat.Bavail * bsize, nil
}

// reclaimedBytes returns the free-space change across a transition: the
// after sample minus the before sample. A failed before sample, a failed
// after sample, or a negative delta (concurrent writers filled the disk
// during the transition) reports zero rather than a guess.
func reclaimedBytes(path string, before uint64, beforeErr error) int64 {
	if beforeErr != nil {
		return 0
	}
	after, err := freeSpace(path)
	if err != nil {
		return 0
	}
	if after < before {
		return 0
	}
	delta := after - before
	const maxInt64 = int64(^uint64(0) >> 1)
	if delta > uint64(maxInt64) {
		return maxInt64
	}
	return int64(delta)
}
