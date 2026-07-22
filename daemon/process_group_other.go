//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package daemon

import (
	"errors"
	"os/exec"
)

var errSessionProcessExited = errors.New("session process already exited")

func configureSessionProcessGroup(_ *exec.Cmd) {}

func killSessionProcessGroup(_ *exec.Cmd) error {
	return errors.New("session process-group kill is unsupported on this operating system")
}

func waitSessionProcessGroup(_ *exec.Cmd) {}
