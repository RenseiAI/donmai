//go:build darwin

package daemon

import (
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func archiveRootRenameNoReplace(root *os.Root, source, destination string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	fd := directory.Fd()
	if fd > uintptr(math.MaxInt) {
		return fmt.Errorf("archive root: directory descriptor overflows int")
	}
	return unix.RenameatxNp(int(fd), source, int(fd), destination, unix.RENAME_EXCL)
}
