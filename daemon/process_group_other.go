//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package daemon

import (
	"errors"
	"os/exec"
	"time"
)

var errSessionProcessExited = errors.New("session process already exited")

const (
	sessionTerminationGrace   = 2 * time.Second
	processGroupPostWaitGrace = 2 * time.Second
)

type processGroupWaitResult string

const (
	processGroupGone       processGroupWaitResult = "gone"
	processGroupTimedOut   processGroupWaitResult = "timeout"
	processGroupPermission processGroupWaitResult = "permission-denied"
	processGroupUnknown    processGroupWaitResult = "unknown"
)

func configureSessionProcessGroup(_ *exec.Cmd) {}

func terminateSessionProcessGroup(_ *exec.Cmd) error {
	return errors.New("session process-group termination is unsupported on this operating system")
}

func killSessionProcessGroup(_ *exec.Cmd) error {
	return errors.New("session process-group kill is unsupported on this operating system")
}

func waitSessionProcessGroup(_ *exec.Cmd, _ time.Duration) processGroupWaitResult {
	return processGroupUnknown
}
