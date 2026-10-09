//go:build darwin

package sessionshim

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProcessGroupMemberCountSkipsZombies pins darwin's member count: a group
// whose only member has exited but not been waited (a zombie) has no live
// member, and the same group with a running member does.
func TestProcessGroupMemberCountSkipsZombies(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G204: fixed test-only argv
	running := exec.Command("/bin/sleep", "60")
	running.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-running.Process.Pid, syscall.SIGKILL)
		_ = running.Wait()
	})
	if live, err := processGroupHasLiveMember(running.Process.Pid); err != nil || !live {
		t.Fatalf("processGroupHasLiveMember(running group) = (%v,%v), want (true,nil)", live, err)
	}

	//nolint:gosec // G204: fixed test-only argv
	exited := exec.Command("/bin/sh", "-c", "exit 0")
	exited.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	pid := exited.Process.Pid
	t.Cleanup(func() { _ = exited.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err == nil && kp != nil && kp.Proc.P_stat == darwinZombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie: (%v,%v)", pid, kp, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if live, err := processGroupHasLiveMember(pid); err != nil || live {
		t.Fatalf("processGroupHasLiveMember(zombie-only group) = (%v,%v), want (false,nil)", live, err)
	}
}

// TestProcessGroupMemberCountNeverTurnsAReadErrorIntoGone pins the darwin
// count's error handling: only an empty answer or ESRCH proves a group has no
// member. Any other sysctl failure is unproved, so the reap proof it feeds can
// never claim a group gone that it failed to read.
func TestProcessGroupMemberCountNeverTurnsAReadErrorIntoGone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"empty answer", nil, false},
		{"no such group", unix.ESRCH, false},
		{"invalid argument", unix.EINVAL, true},
		{"io error", unix.EIO, true},
		{"no such sysctl", unix.ENOENT, true},
	} {
		live, err := liveGroupMember(4242, nil, tc.err)
		if live {
			t.Fatalf("%s: liveGroupMember reported a live member from an empty answer", tc.name)
		}
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: liveGroupMember error = %v, want error %v", tc.name, err, tc.wantErr)
		}
	}
}
