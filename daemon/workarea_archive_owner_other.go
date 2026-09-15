//go:build !darwin && !linux

package daemon

import "os"

// Fail closed on platforms where the standard library does not expose a local
// filesystem owner that can be compared to the process authority.
func archiveRootOwnedByCurrentUser(os.FileInfo) bool { return false }
