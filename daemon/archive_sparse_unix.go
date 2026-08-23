//go:build darwin || linux

package daemon

import (
	"errors"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func copySparseData(source, destination *os.File, size int64) error {
	if size == 0 {
		return destination.Truncate(0)
	}
	if source.Fd() > math.MaxInt || destination.Fd() > math.MaxInt {
		return errors.New("archive copy file descriptor out of range")
	}
	sourceFD := int(source.Fd())
	position := int64(0)
	for position < size {
		data, err := unix.Seek(sourceFD, position, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
			return errors.New("archive copy cannot preserve sparse extents on this filesystem")
		}
		if err != nil {
			return err
		}
		hole, err := unix.Seek(sourceFD, data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if hole > size {
			hole = size
		}
		if _, err := source.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := destination.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(destination, source, hole-data); err != nil {
			return err
		}
		position = hole
	}
	return destination.Truncate(size)
}
