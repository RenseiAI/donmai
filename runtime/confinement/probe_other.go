//go:build !darwin

package confinement

import "golang.org/x/sys/unix"

// xattrShowCompression has no meaning off macOS.
const xattrShowCompression = 0

// getxattr reads one extended attribute. Options exist only on macOS.
func getxattr(path, name string, dest []byte, _ int) (int, error) {
	return unix.Getxattr(path, name, dest)
}
