//go:build !darwin && !linux

package tempdoctor

import "os"

// childCountHint has no filesystem child count to read on this platform;
// large directories are reported as lower bounds.
func childCountHint(string) func(string, os.FileInfo) (int64, bool) { return nil }
