package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const archiveAllocationGrowthSupported = true

// growFileAllocation allocates length bytes past the end of path without
// changing its size or content, through the F_PREALLOCATE reservation.
func growFileAllocation(path string, length int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	store := unix.Fstore_t{Flags: unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Length: length}
	err = unix.FcntlFstore(file.Fd(), unix.F_PREALLOCATE, &store)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("grow allocation of %s: %w", path, err)
	}
	return nil
}
