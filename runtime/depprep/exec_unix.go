//go:build unix

package depprep

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// isolateProcessGroup starts the child as the leader of its own process
// group and kills the whole group on cancellation, as the runner's
// dependency install does.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel runs while the leader is still unreaped, so its PID is still
	// the group id; looking it up after Wait would miss surviving children.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
