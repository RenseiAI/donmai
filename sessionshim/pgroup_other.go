//go:build !darwin && !linux

package sessionshim

import "fmt"

// ProcessGroupGone has no implementation outside darwin and linux. Returning
// an error keeps the reap proof unproved there: a tombstone never claims a
// reap this platform cannot verify.
func ProcessGroupGone(pgid int) (bool, error) {
	return false, fmt.Errorf("%w: process group probe is unavailable on this platform (pgid %d)", ErrProcessIdentity, pgid)
}
