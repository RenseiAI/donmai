//go:build windows

package codex

import (
	"os"
	"os/exec"
	"time"
)

func syscallSIGTERM() os.Signal              { return os.Interrupt }
func configureOwnedProcessGroup(_ *exec.Cmd) {}

type ownedProcess struct {
	cmd     *exec.Cmd
	request chan time.Duration
	stopped chan struct{}
}

func newOwnedProcess(cmd *exec.Cmd) *ownedProcess {
	return &ownedProcess{cmd: cmd, request: make(chan time.Duration, 1), stopped: make(chan struct{})}
}

func (p *ownedProcess) stop(grace time.Duration) error {
	select {
	case p.request <- grace:
	default:
	}
	<-p.stopped
	return nil
}

func (p *ownedProcess) wait() error {
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case grace := <-p.request:
		_ = p.cmd.Process.Signal(syscallSIGTERM())
		select {
		case err = <-done:
		case <-time.After(grace):
			_ = p.cmd.Process.Kill()
			err = <-done
		}
	}
	close(p.stopped)
	return err
}
