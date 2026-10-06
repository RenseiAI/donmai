//go:build !unix

package daemon

import (
	"os"
)

// sessionTmpOwnerUID cannot prove directory ownership without a
// uid-carrying stat, so on non-unix platforms every reuse check refuses:
// only a freshly created directory (inherently ours) is ever trusted, and
// the advisory value cannot validate to a creatable path there anyway.
func sessionTmpOwnerUID(os.FileInfo) (int, error) {
	return 0, errSessionTmpOwnershipUnavailable
}
