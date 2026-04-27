//go:build windows

package process

// Daemonize is not supported on Windows.
// It always returns ErrUnsupported.
func Daemonize() (isChild bool, childPID int, err error) {
	return false, 0, ErrUnsupported
}
