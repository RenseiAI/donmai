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
	// A concurrent append between the caller's size check and this
	// read leaves the staged copy short of the live source. Clamp
	// the read to the captured size: io.CopyN then hits EOF exactly
	// when the source grew (short read), which surfaces as an
	// error — fail closed — instead of staging a silently short
	// file. A truncate under the read likewise errors.
	if current, err := source.Stat(); err != nil || current.Size() != size {
		return errors.New("archive copy source file changed during copy")
	}
	sourceFD, err := archiveFileDescriptor(source.Fd())
	if err != nil {
		return err
	}
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

func archiveFileDescriptor(fd uintptr) (int, error) {
	if fd > math.MaxInt {
		return 0, errors.New("archive copy file descriptor out of range")
	}
	return int(fd), nil
}
