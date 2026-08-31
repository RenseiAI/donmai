//go:build !windows

package codex

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// processAliveOS reports whether pid is a currently running process.
// os.FindProcess always succeeds on unix regardless of pid validity (unix
// processes are found lazily via signal delivery), so liveness is proven
// with a signal-0 probe: it delivers no signal, but still fails with ESRCH
// for a pid that does not exist.
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// processLooksLikeCodexOS shells out to the standard POSIX `ps` utility
// (portable across macOS and Linux, unlike /proc or lsof) to read the
// running command's own name for pid, and reports whether it contains
// binaryHint. This is the sweep's THIRD independent gate before it will
// ever terminate a live process: a PID recorded in an orphan's manifest
// that has since been reused by an unrelated process fails this check and
// is left alone.
func processLooksLikeCodexOS(pid int, binaryHint string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output() //nolint:gosec // G204: pid is an int from our own manifest, ps is a fixed argv.
	if err != nil {
		return false
	}
	comm := strings.ToLower(strings.TrimSpace(filepath.Base(strings.TrimSpace(string(out)))))
	hint := strings.ToLower(strings.TrimSpace(filepath.Base(binaryHint)))
	if hint == "" {
		hint = "codex"
	}
	return comm != "" && strings.Contains(comm, hint)
}

// readOwnedManifestBytes opens dir and the owner manifest inside it through
// fd-relative resolution, so the ownership verdict and the bytes returned
// are provably about the SAME inodes — see
// readVerifiedDonmaiOwnerManifest's doc comment for the threat that verdict
// closes (an unprivileged local user planting a directory, and a manifest
// naming an arbitrary PID, under a shared world-writable os.TempDir()).
//
// Why fds rather than a stat of a path: verifying an os.Lstat FileInfo and
// then re-opening the manifest by name is TWO independent path resolutions
// with a window between them, and on a world-writable Root a local user can
// re-point either name in that window — so the bytes that come back need
// not be the bytes that were vetted. Opening the directory once with
// O_NOFOLLOW|O_DIRECTORY, fstat'ing that descriptor, then openat'ing the
// manifest relative to it removes the window: every check, and the read
// itself, addresses an inode this process already holds open.
//
// Both the directory and the manifest file must be owned by this process's
// own uid and grant no group/other access at all (matching codexHomeMode's
// own 0700, and codexConfigMode's 0600 for the file).
func readOwnedManifestBytes(dir string) ([]byte, error) {
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open artifact directory: %w", err)
	}
	defer func() { _ = unix.Close(dirFD) }()
	if err := verifyOwnedDescriptor(dirFD, "artifact directory", unix.S_IFDIR); err != nil {
		return nil, err
	}
	manifestFD, err := unix.Openat(dirFD, donmaiOwnerManifestName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open owner manifest: %w", err)
	}
	manifest := os.NewFile(uintptr(manifestFD), filepath.Join(dir, donmaiOwnerManifestName))
	defer func() { _ = manifest.Close() }()
	if err := verifyOwnedDescriptor(manifestFD, "owner manifest", unix.S_IFREG); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(manifest, donmaiOwnerManifestMaxBytes))
}

// verifyOwnedDescriptor fstats an already-open descriptor and requires it to
// be wantType, owned by this process's own uid, and to grant no group or
// other access. Taking a descriptor rather than a path is the whole point:
// the answer cannot be invalidated by a rename between the check and the
// use.
func verifyOwnedDescriptor(fd int, what string, wantType uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", what, err)
	}
	mode := uint32(st.Mode) //nolint:gosec // G115: st.Mode is a mode word (16 or 32 bits depending on platform); widening is lossless.
	if mode&unix.S_IFMT != wantType {
		return fmt.Errorf("%s is not the expected file type (mode %#o)", what, mode)
	}
	if st.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: os.Getuid() is a small non-negative uid on any real unix system.
		return fmt.Errorf("%s is owned by uid %d, not this process's uid %d", what, st.Uid, os.Getuid())
	}
	if perm := mode & 0o777; perm&0o077 != 0 {
		return fmt.Errorf("%s mode %04o grants group or other access", what, perm)
	}
	return nil
}
