//go:build linux

package daemon

import (
	"os"

	"golang.org/x/sys/unix"
)

func archiveRootRenameNoReplace(root *os.Root, source, destination string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return unix.Renameat2(int(directory.Fd()), source, int(directory.Fd()), destination, unix.RENAME_NOREPLACE)
}
