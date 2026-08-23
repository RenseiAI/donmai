//go:build darwin

package workarea

import "golang.org/x/sys/unix"

func renameNoReplace(source, destination string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCL)
}
