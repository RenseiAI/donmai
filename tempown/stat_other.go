//go:build !darwin && !linux

package tempown

import (
	"os"
	"time"
)

func changedAt(info os.FileInfo) time.Time { return info.ModTime() }

// ownedExclusively cannot be proven without POSIX ownership, so the janitor
// never removes anything on these platforms.
func ownedExclusively(os.FileInfo) bool { return false }
