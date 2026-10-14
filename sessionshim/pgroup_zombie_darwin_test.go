//go:build darwin

package sessionshim

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// assertZombieGroupUnproved pins the darwin half of the cross-platform reap
// agreement: a group whose only member is a zombie — exited but not yet
// waited by its parent — is unproved through the injected-EPERM path, never
// gone. Linux never reaches the count for such a group (a zombie still
// answers kill there, so the probe itself reports alive); the agreement is
// that neither platform reports a zombie-only group as reaped. The parent's
// wait is what moves the proof: once waited, the same group is gone.
func assertZombieGroupUnproved(t *testing.T, eperm func(int, syscall.Signal) error) {
	t.Helper()

	//nolint:gosec // G204: fixed test-only argv
	zombie := exec.Command("/bin/sh", "-c", "exit 0")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	zombiePgid := zombie.Process.Pid
	t.Cleanup(func() { _ = zombie.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", zombiePgid)
		if err == nil && kp != nil && kp.Proc.P_stat == darwinZombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie", zombiePgid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gone, err := processGroupGone(zombiePgid, eperm); gone || err == nil {
		t.Fatalf("processGroupGone(zombie-only group, EPERM) = (%v,%v), want (false,unproved)", gone, err)
	}
	// The parent's wait is what moves the proof: once waited, the same
	// group is gone.
	if err := zombie.Wait(); err != nil {
		t.Fatalf("wait the zombie harness: %v", err)
	}
	if gone, err := processGroupGone(zombiePgid, eperm); err != nil || !gone {
		t.Fatalf("processGroupGone(waited group, EPERM) = (%v,%v), want (true,nil)", gone, err)
	}
}
