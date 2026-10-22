package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const archiveAllocationGrowthSupported = true

// growFileAllocation allocates length bytes past the end of path without
// changing its size or content: the shape speculative preallocation and
// delayed-allocation reservations take on Linux filesystems.
func growFileAllocation(path string, length int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	fd, err := archiveFileDescriptor(file.Fd())
	if err == nil {
		err = unix.Fallocate(fd, unix.FALLOC_FL_KEEP_SIZE, info.Size(), length)
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("grow allocation of %s: %w", path, err)
	}
	return nil
}
