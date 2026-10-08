//go:build linux

package confinement

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// allowAnyTracer lets any process of this user attach to the decoy, so a
// host attach policy (Yama) cannot stand in for the boundary: without a
// boundary the confined probe's attach succeeds, and with one only the
// boundary refuses it.
func allowAnyTracer() error {
	return unix.Prctl(unix.PR_SET_PTRACER, unix.PR_SET_PTRACER_ANY, 0, 0, 0)
}

// ptraceSeize attaches to pid without stopping it. The probe exits soon
// after, which detaches.
func ptraceSeize(pid int) error {
	return unix.PtraceSeize(pid)
}

// nestedRemount tries to regain write access to the read-only leaf from a
// nested user and mount namespace — the one way an unprivileged process can
// mount — by remounting it read-write there and creating file through it.
// The child is this executable re-run in remount mode; any failure to
// start it is a refusal, and the effect is judged on the host.
func nestedRemount(leaf, file string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self) //nolint:gosec // G204: the probe re-executes itself.
	cmd.Env = append(os.Environ(), probeRemountEnv+"="+leaf+"\n"+file)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	return cmd.Run()
}

// runNestedRemountChild runs inside the nested namespaces: remount the
// leaf read-write (a mount locked by the outer boundary refuses), then
// create the file through it.
func runNestedRemountChild(target string) error {
	leaf, file, ok := strings.Cut(target, "\n")
	if !ok {
		return errors.New("malformed remount target")
	}
	_ = unix.Mount(leaf, leaf, "", unix.MS_REMOUNT|unix.MS_BIND, "")
	return createFile(file)
}
