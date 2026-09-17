//go:build unix

package mcpheaders

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var openBearerFile = openBearerFileNoFollow

func openBearerFileNoFollow(path string) (*os.File, os.FileInfo, error) {
	// os.Root.OpenFile routes these native flags through the standard-library
	// Unix opener, which adds close-on-exec while returning the owned *os.File.
	// O_NOFOLLOW authenticates the opened pathname object and
	// O_NONBLOCK prevents a substituted FIFO/device from hanging before fstat.
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, nil, err
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	closeRootErr := root.Close()
	if err != nil || closeRootErr != nil {
		if file != nil {
			_ = file.Close()
		}
		return nil, nil, errors.Join(err, closeRootErr)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}
