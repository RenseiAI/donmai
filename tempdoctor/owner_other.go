//go:build !darwin && !linux

package tempdoctor

import "os"

// ownedByCurrentUser cannot be proven without POSIX ownership, so tool
// caches are never removed on these platforms.
func ownedByCurrentUser(os.FileInfo) bool { return false }
