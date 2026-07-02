package daemon

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HostInfo is the machine-telemetry block the daemon gathers once at startup
// and threads onto RegisterRequest.HostInfo. The JSON field names MUST match
// the platform register route's HostInfoBody parser verbatim
// (platform/src/app/api/workers/register/route.ts:65-76):
//
//	{ ip?, os?, osVersion?, arch?, cpuCores?, cpuModel?, memTotalMb?,
//	  daemonVersion?, startedAt? }
//
// The platform's parseHostInfo() maps these onto the worker_hosts columns:
// ip→ip_address, os→os, osVersion→os_version, arch→arch, cpuCores→cpu_cores,
// cpuModel→cpu_model, memTotalMb→mem_total_mb, daemonVersion→daemon_version
// (falls back to the top-level `version` if omitted), startedAt→daemon_started_at.
//
// Every field is omitempty: this is an additive, back-compat contract. os,
// arch, and cpuCores come from the stdlib and are effectively always present;
// cpuModel, memTotalMb, osVersion, and ip are best-effort probes that degrade
// to absent on a platform where the probe isn't trivial (never fatal).
type HostInfo struct {
	IP            string `json:"ip,omitempty"`
	OS            string `json:"os,omitempty"`
	OSVersion     string `json:"osVersion,omitempty"`
	Arch          string `json:"arch,omitempty"`
	CPUCores      int    `json:"cpuCores,omitempty"`
	CPUModel      string `json:"cpuModel,omitempty"`
	MemTotalMB    int    `json:"memTotalMb,omitempty"`
	DaemonVersion string `json:"daemonVersion,omitempty"`
	// StartedAt is the daemon process start time as an RFC3339 string. The
	// platform derives uptime = now - startedAt. Populate from
	// Daemon.StartedAt() rather than recomputing.
	StartedAt string `json:"startedAt,omitempty"`
}

// GatherHostInfo builds a best-effort HostInfo. version is the daemon build
// string (Daemon.EffectiveVersion()); startedAt is Daemon.StartedAt(). The
// always-available stdlib fields (os/arch/cpuCores) are set unconditionally;
// the platform-specific probes (cpuModel, memTotalMb, osVersion, ip) are
// best-effort and left empty on any failure. A probe panic is recovered so
// telemetry gathering can never crash registration.
func GatherHostInfo(version string, startedAt time.Time) (info *HostInfo) {
	info = &HostInfo{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUCores:      runtime.NumCPU(),
		DaemonVersion: version,
	}
	if !startedAt.IsZero() {
		info.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}

	// Guard the best-effort probes: a probe should degrade to empty, never
	// crash the daemon at startup. Recover into the already-populated info so
	// the stdlib fields survive a panic in a platform probe.
	defer func() {
		_ = recover()
	}()

	if ip := primaryOutboundIP(); ip != "" {
		info.IP = ip
	}
	if v := probeOSVersion(); v != "" {
		info.OSVersion = v
	}
	if model := probeCPUModel(); model != "" {
		info.CPUModel = model
	}
	if mb := probeMemTotalMB(); mb > 0 {
		info.MemTotalMB = mb
	}
	return info
}

// primaryOutboundIP returns the private/LAN IPv4 address the host would use for
// outbound traffic, or "" if it can't be determined. It uses a UDP "connect"
// (no packets are actually sent) to let the kernel pick the source address for
// the default route, then falls back to scanning interfaces for the first
// non-loopback private IPv4. Stdlib only.
func primaryOutboundIP() string {
	// UDP connect trick — resolves the source IP for the default route without
	// sending anything on the wire. A TEST-NET / documentation address is fine
	// as the "destination"; we never transmit.
	if conn, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		defer func() { _ = conn.Close() }()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if v4 := addr.IP.To4(); v4 != nil && !addr.IP.IsLoopback() {
				return v4.String()
			}
		}
	}
	// Fallback: first non-loopback private IPv4 across all interfaces.
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if v4 := ip.To4(); v4 != nil && ip.IsPrivate() {
				return v4.String()
			}
		}
	}
	return ""
}

// probeCPUModel returns a best-effort CPU model string. Linux reads
// /proc/cpuinfo's "model name"; darwin shells out to
// `sysctl -n machdep.cpu.brand_string`. Returns "" on any other platform or
// on failure.
func probeCPUModel() string {
	switch runtime.GOOS {
	case "linux":
		return firstProcValue("/proc/cpuinfo", "model name")
	case "darwin":
		return runSysctlString("machdep.cpu.brand_string")
	default:
		return ""
	}
}

// probeMemTotalMB returns total physical RAM in mebibytes, best-effort. Linux
// parses /proc/meminfo's "MemTotal" (reported in kB); darwin shells out to
// `sysctl -n hw.memsize` (reported in bytes). Returns 0 on failure.
func probeMemTotalMB() int {
	switch runtime.GOOS {
	case "linux":
		// MemTotal is "MemTotal:  16384000 kB".
		raw := firstProcValue("/proc/meminfo", "MemTotal")
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0
		}
		return int(kb / 1024)
	case "darwin":
		out := runSysctlString("hw.memsize")
		bytesVal, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil || bytesVal <= 0 {
			return 0
		}
		return int(bytesVal / (1024 * 1024))
	default:
		return 0
	}
}

// probeOSVersion returns a best-effort OS version string. Darwin uses
// `sysctl -n kern.osproductversion` (e.g. "14.5"); Linux reads
// /etc/os-release's VERSION_ID (falling back to PRETTY_NAME). Returns "" on
// failure.
func probeOSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		return runSysctlString("kern.osproductversion")
	case "linux":
		if v := osReleaseValue("VERSION_ID"); v != "" {
			return v
		}
		return osReleaseValue("PRETTY_NAME")
	default:
		return ""
	}
}

// firstProcValue scans a colon-delimited /proc file for the first line whose
// key (left of the ':') matches prefix, returning the trimmed value (right of
// the ':'). Returns "" if the file can't be read or the key isn't found.
func firstProcValue(path, prefix string) string {
	f, err := os.Open(path) //nolint:gosec // path is a fixed /proc literal
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if key == prefix {
			return strings.TrimSpace(line[idx+1:])
		}
	}
	return ""
}

// osReleaseValue reads /etc/os-release and returns the value for key, with any
// surrounding quotes stripped. Returns "" when the file or key is absent.
func osReleaseValue(key string) string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`)
	}
	return ""
}

// runSysctlString runs `sysctl -n <name>` with a short timeout and returns the
// trimmed stdout, or "" on any error. Used only on darwin.
func runSysctlString(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
