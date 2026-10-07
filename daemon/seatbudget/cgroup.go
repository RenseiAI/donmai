package seatbudget

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Linux enforcement: cgroups v2.
//
// A seat's process tree is placed in its own cgroup with CPU set pinning,
// a CPU quota, a memory ceiling and an IO weight. Two placements exist:
//
//  1. systemd transient scope: when the host runs systemd as PID 1 and the
//     systemd-run helper is available, the seat launches wrapped in
//     `systemd-run --scope --collect` with AllowedCPUs=, CPUQuota=,
//     MemoryMax=/MemoryHigh= and IOWeight= properties. systemd owns the
//     cgroup lifetime, so no cleanup path can leak it.
//  2. direct cgroupfs: when systemd is unavailable but the v2 hierarchy is
//     writable, the launcher creates a per-seat cgroup under the current
//     cgroup, writes the same limits, and moves the child into it.
//
// When neither is available the budget reports ModeNone with a detail line:
// the seat runs unconfined rather than failing. Confinement must never take
// a working seat offline for want of a hierarchy.

// CPUSet renders the cpuset.cpus value for a seat: the first `cpus` logical
// CPUs as a range ("0-3"). Pinning starts at CPU 0 so sequential seats pack
// onto the same cores the way the default CPU share already does. Ranges
// keep the value short on many-core hosts.
func CPUSet(cpus int) string {
	if cpus <= 1 {
		return "0"
	}
	return "0-" + strconv.Itoa(cpus-1)
}

// CPUQuotaPercent renders the cpu.max quota for cpus whole cores as a
// percent of one core: 2 CPUs -> "200%". systemd's CPUQuota= takes the same
// percent spelling, so one value serves both placements.
func CPUQuotaPercent(cpus int) string {
	if cpus < 1 {
		cpus = 1
	}
	return strconv.Itoa(cpus*100) + "%"
}

// CPUWeight renders cpu.weight from cpus. Weight is present so a host that
// overcommits (more seats × cpus than cores) still divides contended CPU
// proportionally instead of letting one seat's burst starve the others.
// The scale is linear in the seat share; the absolute numbers only matter
// relative to each other.
func CPUWeight(cpus int) string {
	if cpus < 1 {
		cpus = 1
	}
	return strconv.Itoa(cpus * 100)
}

// MemoryBytes renders memory.max from mebibytes.
func MemoryBytes(memoryMB int) string {
	if memoryMB <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(memoryMB)*1024*1024, 10)
}

// SystemdScopeArgs renders the `systemd-run --scope` argv prefix that
// confines one seat. name scopes the transient unit (per-session, so two
// seats never share a scope). Empty optional limits are omitted: a seat
// with no memory cap must not get MemoryMax=0, which systemd reads as "no
// memory at all".
func SystemdScopeArgs(name string, b Budget) []string {
	args := []string{
		"systemd-run", "--scope", "--collect",
		"-p", "AllowedCPUs=" + CPUSet(b.CPUs),
		"-p", "CPUQuota=" + CPUQuotaPercent(b.CPUs),
	}
	if mem := MemoryBytes(b.MemoryMB); mem != "" {
		args = append(args, "-p", "MemoryMax="+mem, "-p", "MemoryHigh="+mem)
	}
	if b.IOWeight > 0 {
		args = append(args, "-p", "IOWeight="+strconv.Itoa(b.IOWeight))
	}
	args = append(args, "--unit", name)
	return args
}

// ScopeName returns the transient-scope unit name for a session. The session
// id is sanitised to the systemd unit alphabet; the prefix keeps seat
// scopes greppable in systemctl output.
func ScopeName(sessionID string) string {
	var sb strings.Builder
	sb.WriteString("donmai-seat-")
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
		if sb.Len() >= 64 {
			break
		}
	}
	name := sb.String()
	if name == "donmai-seat-" {
		name += "session"
	}
	return name + ".scope"
}

// HasSystemd reports whether PID 1 is systemd and the systemd-run helper is
// on PATH: the precondition for the transient-scope placement.
func HasSystemd() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(raw)) != "systemd" {
		return false
	}
	_, err = exec.LookPath("systemd-run")
	return err == nil
}

// CgroupV2Writable reports whether the cgroup v2 hierarchy is mounted and
// the current process can create a child cgroup: the precondition for the
// direct-cgroupfs placement.
func CgroupV2Writable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	// The v2 hierarchy mounts cgroup.controllers at its root. Presence of
	// the file means v2; writability of our own cgroup dir means we can
	// create a child.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return false
	}
	self, err := selfCgroupPath()
	if err != nil {
		return false
	}
	// Probe with the real permission question: can we create a child?
	probe := filepath.Join(self, ".donmai-budget-probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// selfCgroupPath returns the absolute cgroupfs path of this process's v2
// cgroup, resolved from /proc/self/cgroup. A unified-hierarchy entry reads
// "0::/path".
func selfCgroupPath() (string, error) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[1] != "" {
			continue
		}
		rel := strings.TrimPrefix(parts[2], "/")
		if rel == "" {
			return "/sys/fs/cgroup", nil
		}
		return filepath.Join("/sys/fs/cgroup", filepath.FromSlash(rel)), nil
	}
	return "", fmt.Errorf("no unified cgroup entry in /proc/self/cgroup")
}

// Placement is the Linux enforcement placement the launcher takes.
type Placement string

const (
	// PlacementSystemd confines the seat in a transient systemd scope.
	PlacementSystemd Placement = "systemd"
	// PlacementCgroupFS confines the seat with direct cgroupfs writes.
	PlacementCgroupFS Placement = "cgroupfs"
	// PlacementNone runs the seat unconfined and reports ModeNone.
	PlacementNone Placement = "none"
)

// SelectPlacement picks the enforcement placement for this host: the
// transient-scope placement when systemd is present, else direct cgroupfs
// when the v2 hierarchy is writable, else none. hasSystemd and
// cgroupWritable are injected so tests pin each branch on any host.
func SelectPlacement(hasSystemd, cgroupWritable bool) Placement {
	switch {
	case hasSystemd:
		return PlacementSystemd
	case cgroupWritable:
		return PlacementCgroupFS
	default:
		return PlacementNone
	}
}

// HostPlacement probes this host and picks the placement.
func HostPlacement() Placement {
	return SelectPlacement(HasSystemd(), CgroupV2Writable())
}
