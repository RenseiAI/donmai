//go:build unix

package mcpheaders

import (
	"os"

	"golang.org/x/sys/unix"
)

var openBearerFile = openBearerFileNoFollow

func openBearerFileNoFollow(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path) //nolint:gosec // G115: successful unix.Open returns a nonnegative OS descriptor representable as uintptr.
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}
