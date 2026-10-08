package seatbudget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linux enforcement: cgroups v2.
//
// A shim-owned seat's process tree always runs in a transient systemd scope
// (`systemd-run --scope --collect`), budget or not: the scope is what owns
// the seat's cgroup past a daemon restart, so every shim launch wraps — a
// budgetless seat simply renders no limit properties. A budgeted seat adds a
// CPU quota, a CPU weight share, a memory high/max ceiling and an IO weight;
// every scope carries the seat-survives-OOM policy. Exactly one placement
// exists:
//
//  1. systemd transient scope: when the host runs systemd (MinScopeSystemd
//     or newer) as PID 1 and the systemd-run helper is available, the seat
//     launches wrapped in
//     `systemd-run --scope --collect` with CPUQuota=, CPUWeight=,
//     MemoryMax=/MemoryHigh=, IOWeight= and OOMPolicy=continue properties.
//     systemd owns the cgroup lifetime, so no cleanup path can leak it.
//     When the daemon runs without the system bus (a per-user service),
//     the scope is created on the user bus (--user).
//
// There is deliberately no core pinning: the quota is the binding
// throughput limit (identical pin ranges across seats shared 2 cores of 6
// budgeted, and user services never get the cpuset controller delegated),
// while the weight divides contended CPU proportionally when seats
// overcommit. CPUSet therefore only feeds the human detail line, never a
// scope property.
//
// When systemd is unavailable the launcher does NOT pretend: the seat runs
// unconfined and the report says ModeNone with a detail line naming the
// missing backend. Confinement must never take a working seat offline for
// want of a hierarchy, and the report must never claim a confinement the
// seat did not get.

// CPUSet renders the seat's CPU count for report lines.
//
// No core pinning rides the scope, by measurement: every seat rendered
// the same first-N range (three 2-CPU seats shared cores 0-1 for 1.99
// cores of 6 budgeted), and user services never get the cpuset
// controller delegated, so the property was silently ignored there while
// the report claimed it. The CPU quota is the binding throughput limit on
// both install types; the weight divides contended CPU proportionally
// when seats overcommit. CPUSet therefore only feeds the human detail,
// never a cgroup property.
func CPUSet(cpus int) string {
	if cpus < 1 {
		cpus = 1
	}
	return strconv.Itoa(cpus) + " CPUs"
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
// seats never share a scope). A budget that carries no numeric limits
// renders no limit properties — the scope still owns the seat's cgroup, so
// the seat survives its daemon either way. Empty optional limits are
// omitted: a seat with no memory cap must not get MemoryMax=0, which
// systemd reads as "no memory at all". userScope selects the bus flag: a
// per-user daemon reaches only the user manager, so its seats need --user;
// a system service uses the system bus and must not pass it.
// OOMPolicy=continue keeps the seat alive when one child hits the memory
// ceiling: the kernel kills only the offender, while the default (stop)
// would end the whole agent session on the first child OOM.
func SystemdScopeArgs(name string, b Budget) []string {
	return SystemdScopeArgsForBus(name, b, false)
}

// SystemdScopeArgsForBus is SystemdScopeArgs parametrised by the bus: true
// wraps the seat via `systemd-run --user --scope` (per-user service
// without the system bus), false via `systemd-run --scope` (system
// service). Tests pin both spellings on any host.
//
// A zero-CPU budget renders no CPUQuota/CPUWeight: with no budget the scope
// owns the seat's cgroup and nothing else, so the seat still outlives its
// daemon while systemd applies no throughput limit to it.
func SystemdScopeArgsForBus(name string, b Budget, userScope bool) []string {
	args := []string{"systemd-run"}
	if userScope {
		args = append(args, "--user")
	}
	args = append(args, "--scope", "--collect")
	if b.CPUs >= 1 {
		args = append(args, "-p", "CPUQuota="+CPUQuotaPercent(b.CPUs),
			"-p", "CPUWeight="+CPUWeight(b.CPUs))
	}
	args = append(args, "-p", "OOMPolicy=continue")
	if mem := MemoryBytes(b.MemoryMB); mem != "" {
		args = append(args, "-p", "MemoryMax="+mem, "-p", "MemoryHigh="+mem)
	}
	if b.IOWeight > 0 {
		args = append(args, "-p", "IOWeight="+strconv.Itoa(b.IOWeight))
	}
	args = append(args, "--unit", name)
	return args
}

// SystemdUserScope reports whether this process runs under a per-user
// systemd manager: the user bus is reachable and the system bus is not
// usable from here. A per-user daemon must create its transient scopes
// with --user; probing the bus (not the install flags) is what answers
// that, so a root-owned user service and a container with a forwarded
// user bus both read correctly. The system bus socket is world-readable,
// so reachability alone cannot distinguish a per-user service from a
// system one: only root may use the system bus. userBus, systemBus are
// injected so tests pin the matrix on any host; the euid rule lives in
// SystemdUserScopeForEUID so tests pin it without changing uids.
func SystemdUserScope(userBus, systemBus bool) bool {
	return SystemdUserScopeForEUID(userBus, systemBus, os.Geteuid())
}

// SystemdUserScopeForEUID is SystemdUserScope parametrised by the
// effective uid. A non-root process with both buses reachable is still a
// per-user install: creating the scope without --user fails there with
// "Access denied", while root keeps the system bus.
func SystemdUserScopeForEUID(userBus, systemBus bool, euid int) bool {
	return userBus && !(systemBus && euid == 0)
}

// ScopeName returns the transient-scope unit name for a session. It is a
// fixed-length digest of the lifecycle identity plus the incarnation: two
// sessions whose ids share a long prefix still get distinct scopes, and a
// hostile id can never escape the unit alphabet or grow the name. The
// prefix keeps seat scopes greppable in systemctl output.
//
// The incarnation is the launch's process epoch: a relaunched session is a
// new scope, never a reuse of the previous launch's name. Pass the record's
// own epoch when deriving the name for an already-running seat — the scope
// belongs to the launch, not to the lifecycle.
func ScopeName(sessionID string) string {
	return ScopeNameForIncarnation("", sessionID, 0)
}

// ScopeNameForIncarnation is ScopeName keyed by the full launch identity:
// the organisation half, the session id, and the launch's process epoch.
// The digest covers the same unit-separator-joined correlation the registry
// uses for its own per-incarnation sidecars, so the name is fixed-length
// and collision-shaped only by the hash — never by truncation. The epoch
// suffix keeps the incarnation greppable in unit listings after the digest.
func ScopeNameForIncarnation(orgID, sessionID string, processEpoch uint64) string {
	correlation := orgID + "\x1f" + sessionID + "\x1f" + strconv.FormatUint(processEpoch, 10)
	sum := sha256.Sum256([]byte(correlation))
	return "donmai-seat-" + hex.EncodeToString(sum[:16]) + "-" + strconv.FormatUint(processEpoch, 10) + ".scope"
}

// MinScopeSystemd is the oldest systemd the transient-scope placement
// supports. Measured: systemd 249 rejects the seat scope outright
// ("Unknown assignment: OOMPolicy=continue", so the seat never starts)
// and its user manager delegates only memory and pids (a per-user scope's
// CPU quota would be silently ignored); 252 and 255 accept the scope,
// delegate cpu to user managers, and confine. An older manager therefore
// gets no placement: the seat runs unconfined and reports none, rather
// than failing to start or claiming a quota it did not get.
const MinScopeSystemd = 252

// HasSystemd reports whether PID 1 is systemd, the systemd-run helper is
// on PATH and the manager is MinScopeSystemd or newer: the precondition
// for the transient-scope placement. systemdRunBinary and pidOneComm name
// the probe inputs so the live confinement test can point them at a
// fixture (or the real host) without recompiling; production passes
// ("systemd-run", "/proc/1/comm"). The version is read once per process:
// HostPlacement runs on every spawn and status read.
func HasSystemd() bool {
	return hasSystemdAt("systemd-run", "/proc/1/comm", hostSystemdVersion)
}

// hostSystemdVersion caches the host's systemd major version (0 when
// unknown) for HasSystemd.
var hostSystemdVersion = sync.OnceValue(func() int { return SystemdVersionAt("systemd-run") })

// HasSystemdAt is HasSystemd parametrised by the helper name and the PID 1
// comm path. The cgroup-confinement suite drives it against the live host
// (asserting the real backend exists before confining anything) and
// against fixtures elsewhere; unit tests pin the matrix through it on any
// host.
func HasSystemdAt(systemdRun, pidOneComm string) bool {
	return hasSystemdAt(systemdRun, pidOneComm, func() int { return SystemdVersionAt(systemdRun) })
}

// hasSystemdAt is HasSystemdAt with the version probe injected so tests
// pin the version gate without a real systemd.
func hasSystemdAt(systemdRun, pidOneComm string, version func() int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile(pidOneComm) //nolint:gosec // G304: production passes the fixed /proc/1/comm literal; the parameter exists so tests can point the probe at a fixture
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(raw)) != "systemd" {
		return false
	}
	if _, err := exec.LookPath(systemdRun); err != nil {
		return false
	}
	return ScopeSystemdSupported(version())
}

// SystemdVersionAt runs `<systemdRun> --version` and returns the systemd
// major version, or 0 when the helper prints none.
func SystemdVersionAt(systemdRun string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, systemdRun, "--version").Output() //nolint:gosec // G204: production passes the fixed "systemd-run" literal; the parameter exists so tests can point the probe at a fixture
	if err != nil {
		return 0
	}
	return ParseSystemdVersion(string(out))
}

// ParseSystemdVersion reads the major version from `systemd-run --version`
// output ("systemd 252 (252.39-1~deb12u2)"). 0 means unknown.
func ParseSystemdVersion(out string) int {
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ScopeSystemdSupported reports whether a systemd of major version v can
// host the seat scope (see MinScopeSystemd). 0 (unknown) keeps the
// placement: every systemd-run prints its version, so unknown means a
// fixture, not an old manager.
func ScopeSystemdSupported(v int) bool {
	return v == 0 || v >= MinScopeSystemd
}

// HasSystemBus reports whether the system bus is reachable from this
// process: the user manager answers on the user bus and the system bus
// answers on its well-known socket. The daemon ships as a per-user
// service (a user-scoped unit or a launchd-style user agent), so its
// seats normally reach only the USER bus — a scope created without
// --user fails there. Both probes are plain socket dials with no new
// dependencies: systemd's own bus-discovery rules (user bus from
// XDG_RUNTIME_DIR, system bus at /run/dbus/system_bus_socket).
func HasSystemBus() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return socketAlive("/run/dbus/system_bus_socket")
}

// HasUserBus reports whether the per-user bus is reachable from this
// process.
func HasUserBus() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return socketAlive(userBusSocketPath(""))
}

// UserScopeForBus resolves the --user decision from the two bus probes:
// a host whose user bus answers but whose system bus does not is a
// per-user install and needs --user. userBus/systemBus are injected so
// tests pin the matrix on any host (see SystemdUserScope).
func UserScopeForBus() bool {
	return SystemdUserScope(HasUserBus(), HasSystemBus())
}

// CgroupV2Writable probes whether the cgroup v2 hierarchy is mounted and
// the current process could create a child cgroup. It is advisory only:
// the launcher performs NO direct cgroupfs placement (per-seat cgroups
// need a lifetime owner the spawner cannot supply for a detached worker),
// so a writable hierarchy without systemd still reports ModeNone — never
// a confinement the seat did not get. Kept beside HasSystemd so the
// status detail can name the missing backend precisely.
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

// userBusSocketPath resolves the user-bus socket: the explicit override
// when set, else XDG_RUNTIME_DIR/bus, else the well-known per-UID path.
// The UID comes from the process owner; tests override it by setting
// XDG_RUNTIME_DIR, which takes precedence over the fallback.
func userBusSocketPath(override string) string {
	if override != "" {
		return override
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "bus")
	}
	return fmt.Sprintf("/run/user/%d/bus", os.Getuid())
}

// socketAlive reports whether a unix stream exchange with path succeeds
// inside a short bound. SOCK_STREAM, not DGRAM: D-Bus listens on
// SOCK_STREAM, and probing the wrong socket type reports every live bus
// as dead (which strands per-user installs on the system bus spelling).
func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Placement is the Linux enforcement placement the launcher takes.
// Systemd-scope-only by design: without systemd there is no backend the
// launcher can own (see CgroupV2Writable), so the seat runs unconfined
// and the report says ModeNone with the reason.
type Placement string

const (
	// PlacementSystemd confines the seat in a transient systemd scope.
	PlacementSystemd Placement = "systemd"
	// PlacementCgroupFS is the systemd-less host: the v2 hierarchy may be
	// present and writable (see CgroupV2Writable), but the launcher
	// performs no direct placement there, so a seat on this placement runs
	// UNCONFINED and its report is ModeNone with the backend named.
	// The value stays on the wire so older status readers keep parsing;
	// only the meaning changed from "confined via cgroupfs" to "no
	// backend — unconfined".
	PlacementCgroupFS Placement = "cgroupfs"
	// PlacementNone runs the seat unconfined and reports ModeNone.
	PlacementNone Placement = "none"
)

// SelectPlacement picks the enforcement placement for this host: the
// transient-scope placement when systemd is present, else the named
// systemd-less placement (unconfined; see PlacementCgroupFS) when the v2
// hierarchy is writable, else none. hasSystemd and cgroupWritable are
// injected so tests pin each branch on any host.
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

// SeatLimits are the limits a seat's cgroup actually carries, read back from
// the live scope. CPUs is the whole-core share derived from the CPU quota;
// MemoryMB the ceiling in mebibytes; IOWeight the cgroup v2 IO weight. Zero
// means that controller carries no limit (a budgetless seat's scope still
// owns the cgroup, it just caps nothing). ok=false means the scope could
// not be read — reaped, unreachable bus, or no systemd — and the caller
// falls back to the launch record instead of reporting numbers it did not
// observe.
type SeatLimits struct {
	CPUs     int
	MemoryMB int
	IOWeight int
}

// ReadSeatLimits reads the limits a live transient scope carries, through
// `systemctl show` against the seat's own unit. userScope selects the bus,
// the same decision the launch used: a scope created with --user is only
// visible with --user. run is injected so tests pin the parse on any host;
// production passes the systemctl runner below.
func ReadSeatLimits(scope string, userScope bool) (SeatLimits, bool) {
	return readSeatLimitsWithRunner(scope, userScope, runSystemctlShow, runtime.GOOS)
}

// runSystemctlShow runs `systemctl [--user] show <scope>` for the three
// seat properties. The binary and property names are fixed; only the scope
// unit and the bus flag vary.
func runSystemctlShow(scope string, userScope bool) ([]byte, error) {
	args := []string{"show", scope, "--property=CPUQuotaPerSecUSec,MemoryMax,IOWeight"}
	if userScope {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", args...).Output() //nolint:gosec // G204: fixed binary and property names; only the unit name (a digest) and the bus flag vary
}

// readSeatLimitsWithRunner is ReadSeatLimits with the systemctl call
// injected so tests pin the parse without a live manager.
func readSeatLimitsWithRunner(scope string, userScope bool, run func(scope string, userScope bool) ([]byte, error), goos string) (SeatLimits, bool) {
	if goos != "linux" {
		return SeatLimits{}, false
	}
	if scope == "" || strings.ContainsAny(scope, "/ ") {
		return SeatLimits{}, false
	}
	out, err := run(scope, userScope)
	if err != nil {
		return SeatLimits{}, false
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	var limits SeatLimits
	if raw, ok := props["CPUQuotaPerSecUSec"]; ok && raw != "" && raw != "infinity" {
		if usec, err := strconv.Atoi(raw); err == nil && usec > 0 {
			// systemd renders the quota per one second of CPU time:
			// 200000 is two cores. Round UP so a fractional share never
			// reports zero CPUs for a seat that holds a quota.
			limits.CPUs = (usec + 100000 - 1) / 100000
		}
	}
	if raw, ok := props["MemoryMax"]; ok && raw != "" && raw != "infinity" {
		if bytes, err := strconv.ParseInt(raw, 10, 64); err == nil && bytes > 0 {
			limits.MemoryMB = int(bytes / (1024 * 1024))
		}
	}
	if raw, ok := props["IOWeight"]; ok && raw != "" {
		if w, err := strconv.Atoi(raw); err == nil && w > 0 {
			limits.IOWeight = w
		}
	}
	return limits, true
}

// ReportSeatLimits renders the adopted-handle evidence for one seat: the
// limits read back from the seat's live cgroup, falling back to the
// launch-time record. liveOK=false means the scope could not be read, so
// the record's own numbers stand in — never the daemon's current
// configuration, which may have changed since the launch. A seat with no
// limits in either source reports mode none with the scope named: it still
// owns its cgroup, it just caps nothing.
func ReportSeatLimits(scope string, live SeatLimits, liveOK bool, recordCPUs, recordMemoryMB, recordIOWeight int) Report {
	cpus, memoryMB, ioWeight := live.CPUs, live.MemoryMB, live.IOWeight
	if !liveOK {
		cpus, memoryMB, ioWeight = recordCPUs, recordMemoryMB, recordIOWeight
	}
	if cpus < 1 && memoryMB <= 0 {
		return Report{Mode: ModeNone, Detail: "seat scope " + scope + " carries no limits"}
	}
	if cpus < 1 {
		cpus = 1
	}
	detail := "cpus " + CPUSet(cpus) + ", quota " + CPUQuotaPercent(cpus)
	if mem := MemoryBytes(memoryMB); mem != "" {
		detail += ", memory " + mem + " bytes"
	}
	if ioWeight > 0 {
		detail += ", io weight " + strconv.Itoa(ioWeight)
	}
	return Report{Mode: ModeEnforced, CPUs: cpus, MemoryMB: memoryMB, Detail: detail + " via transient systemd scope " + scope}
}
