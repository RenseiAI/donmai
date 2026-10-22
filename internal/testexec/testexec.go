// Package testexec writes executable test fixtures (fake CLIs, hook scripts,
// stand-in binaries) without racing concurrent process spawns.
//
// Why this exists: Linux refuses execve of a file that any process still holds
// open for writing (ETXTBSY, "text file busy"). A test that writes a script and
// immediately runs it is exposed to this whenever another goroutine forks in
// the window while the write descriptor is open: the forked child inherits the
// descriptor until its own exec closes it, so the script's execve fails even
// though the writer already closed its copy. Renaming the file into place does
// not help, because the inherited descriptor follows the inode.
//
// The race is closed at its source instead. Go's fork path takes
// syscall.ForkLock exclusively while it forks, so holding the read side across
// open, write and close guarantees no child process can exist that inherited
// the writer. Once the lock is released, nothing holds the file open for
// writing and it can be executed immediately.
package testexec

import (
	"os"
	"syscall"
)

// WriteFile writes data to path and sets its mode to perm, like os.WriteFile
// followed by os.Chmod, with the descriptor's whole lifetime guarded against
// concurrent forks. Use it for every test fixture that will later be executed.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	// os.WriteFile applies perm only on creation and subject to umask.
	return os.Chmod(path, perm) //nolint:gosec // callers pass the intended fixture mode, usually owner-executable
}
