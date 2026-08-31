//go:build !windows

package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// verifyManifestDirectoryOwnershipOS requires info's directory to be owned by
// this process's own unix uid and to grant no group/other access at all
// (matching codexHomeMode's own 0700) — see verifyManifestDirectoryOwnership's
// doc comment for the threat this closes: an unprivileged local user planting
// a directory (and a manifest naming an arbitrary PID) under a shared,
// world-writable os.TempDir().
func verifyManifestDirectoryOwnershipOS(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read unix ownership metadata")
	}
	if stat.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: os.Getuid() is a small non-negative uid on any real unix system.
		return fmt.Errorf("owned by uid %d, not this process's uid %d", stat.Uid, os.Getuid())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("mode %04o grants group or other access", info.Mode().Perm())
	}
	return nil
}
