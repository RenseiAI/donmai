//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const shimPTYStatusLimit = 64 << 10

type shimPTYFacts struct {
	errno, pid, pgid, sid int
	nproc, nofile         [2]uint64
	nprocOK, nofileOK     bool
	status                [3]string // Seccomp, NoNewPrivs, Threads.
	pids                  [2]string // current, max.
}

func init() { shimPTYFailureDiagnostic = logShimPTYFailure }

func logShimPTYFailure(cause error) {
	facts := shimPTYFacts{pid: os.Getpid(), pgid: -1, sid: -1}
	switch {
	case errors.Is(cause, syscall.EPERM):
		facts.errno = int(syscall.EPERM)
	case errors.Is(cause, syscall.EAGAIN):
		facts.errno = int(syscall.EAGAIN)
	default:
		return
	}
	if value, err := unix.Getpgid(0); err == nil {
		facts.pgid = value
	}
	if value, err := unix.Getsid(0); err == nil {
		facts.sid = value
	}
	for _, limit := range []struct {
		resource int
		value    *[2]uint64
		ok       *bool
	}{{unix.RLIMIT_NPROC, &facts.nproc, &facts.nprocOK}, {unix.RLIMIT_NOFILE, &facts.nofile, &facts.nofileOK}} {
		var value unix.Rlimit
		if unix.Getrlimit(limit.resource, &value) == nil {
			*limit.value, *limit.ok = [2]uint64{value.Cur, value.Max}, true
		}
	}
	facts.status = parseShimPTYStatus(readShimPTYBounded("/proc/self/status", shimPTYStatusLimit))
	for i, names := range [2][2]string{{"/sys/fs/cgroup/pids.current", "/sys/fs/cgroup/pids/pids.current"}, {"/sys/fs/cgroup/pids.max", "/sys/fs/cgroup/pids/pids.max"}} {
		for _, name := range names {
			if value := parseShimPTYPids(readShimPTYBounded(name, 64)); value != "" {
				facts.pids[i] = value
				break
			}
		}
	}
	_, _ = fmt.Fprintln(os.Stderr, facts.line())
}

func readShimPTYBounded(name string, limit int64) []byte {
	file, err := os.Open(name)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil
	}
	return raw
}

func shimPTYNumber(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return ""
	}
	return value
}

func parseShimPTYStatus(raw []byte) [3]string {
	var values [3]string
	if len(raw) > shimPTYStatusLimit {
		return values
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for i, allowed := range [3]string{"Seccomp", "NoNewPrivs", "Threads"} {
			if key == allowed {
				values[i] = shimPTYNumber(value)
			}
		}
	}
	return values
}

func parseShimPTYPids(raw []byte) string {
	if len(raw) > 64 {
		return ""
	}
	if value := strings.TrimSpace(string(raw)); value == "max" {
		return value
	}
	return shimPTYNumber(string(raw))
}

func (f shimPTYFacts) line() string {
	out := fmt.Sprintf("shim-pty-testdiag errno=%d pid=%d pgid=%d sid=%d", f.errno, f.pid, f.pgid, f.sid)
	if f.nprocOK {
		out += fmt.Sprintf(" nproc_cur=%d nproc_max=%d", f.nproc[0], f.nproc[1])
	}
	if f.nofileOK {
		out += fmt.Sprintf(" nofile_cur=%d nofile_max=%d", f.nofile[0], f.nofile[1])
	}
	for i, name := range [3]string{"seccomp", "no_new_privs", "threads"} {
		if f.status[i] != "" {
			out += " " + name + "=" + f.status[i]
		}
	}
	// These fixed paths report the visible cgroup mount root, which may be
	// broader than this helper's own nested cgroup.
	for i, name := range [2]string{"mount_pids_current", "mount_pids_max"} {
		if f.pids[i] != "" {
			out += " " + name + "=" + f.pids[i]
		}
	}
	return out
}
