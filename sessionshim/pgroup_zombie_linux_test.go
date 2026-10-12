//go:build linux

package sessionshim

import (
	"syscall"
	"testing"
)

// assertZombieGroupUnproved is a no-op on Linux: a zombie still answers
// kill(-pgid, 0) there, so a zombie-only group reads as alive at the probe
// and never reaches the member count. The cross-platform agreement —
// neither platform reports a zombie-only group as reaped — holds without a
// branch to pin.
func assertZombieGroupUnproved(t *testing.T, eperm func(int, syscall.Signal) error) {
	t.Helper()
}
