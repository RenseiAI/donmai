package daemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SampleLoad returns a best-effort instantaneous CPU and memory utilisation
// sample as percentages in [0,100]. ok is true only when BOTH values were
// computed successfully — the platform's heartbeat route validates load.cpu
// and load.memory together (heartbeat/route.ts:127-138), and the wire struct
// omits the whole `load` key unless ok, so a partial sample must not be sent.
//
// SampleLoad is wired as HeartbeatOptions.GetLoad; it is called once per beat.
// On any platform where a stdlib-only probe isn't trivial (or on any probe
// error) it returns (0,0,false) and the heartbeat simply omits load — matching
// the omitempty/best-effort contract every other optional field uses. A probe
// panic is recovered so a sampling failure can never abort a heartbeat.
func SampleLoad() (cpuPct, memPct float64, ok bool) {
	defer func() {
		if recover() != nil {
			cpuPct, memPct, ok = 0, 0, false
		}
	}()
	switch runtime.GOOS {
	case "linux":
		return sampleLinuxLoad()
	case "darwin":
		return sampleDarwinLoad()
	default:
		return 0, 0, false
	}
}

// sampleLinuxLoad reads memory utilisation from /proc/meminfo and CPU
// utilisation from two /proc/stat samples taken ~120ms apart. The short sleep
// runs in the heartbeat goroutine and is negligible against the 15-30s beat
// cadence.
func sampleLinuxLoad() (float64, float64, bool) {
	mem, memOK := linuxMemPct()
	cpu, cpuOK := linuxCPUPct()
	if !memOK || !cpuOK {
		return 0, 0, false
	}
	return cpu, mem, true
}

// linuxMemPct computes used-memory percentage from MemTotal and MemAvailable
// in /proc/meminfo (both reported in kB). Uses MemAvailable (kernel's estimate
// of allocatable memory) rather than MemFree so cache/buffers don't read as
// "used".
func linuxMemPct() (float64, bool) {
	total := procMeminfoKB("MemTotal")
	avail := procMeminfoKB("MemAvailable")
	if total <= 0 || avail < 0 || avail > total {
		return 0, false
	}
	used := total - avail
	return clampPct(float64(used) / float64(total) * 100), true
}

// procMeminfoKB returns the kB value for a /proc/meminfo key, or -1 on error.
func procMeminfoKB(key string) int64 {
	raw := firstProcValue("/proc/meminfo", key)
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return -1
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1
	}
	return kb
}

// linuxCPUPct computes aggregate CPU utilisation from the delta of the "cpu"
// aggregate line in /proc/stat across a short window.
func linuxCPUPct() (float64, bool) {
	idle1, total1, ok1 := readProcStatCPU()
	if !ok1 {
		return 0, false
	}
	time.Sleep(120 * time.Millisecond)
	idle2, total2, ok2 := readProcStatCPU()
	if !ok2 {
		return 0, false
	}
	totalDelta := total2 - total1
	idleDelta := idle2 - idle1
	if totalDelta <= 0 {
		return 0, false
	}
	return clampPct(float64(totalDelta-idleDelta) / float64(totalDelta) * 100), true
}

// readProcStatCPU parses the aggregate "cpu" line of /proc/stat and returns the
// idle jiffies (idle + iowait) and the total of all fields.
func readProcStatCPU() (idle, total int64, ok bool) {
	f, err := os.Open("/proc/stat") //nolint:gosec // fixed /proc literal
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" label
		var sum int64
		for i, fld := range fields {
			v, err := strconv.ParseInt(fld, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			sum += v
			// Field 3 = idle, field 4 = iowait (0-indexed).
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return idle, sum, true
	}
	return 0, 0, false
}

// sampleDarwinLoad computes memory utilisation from vm_stat + hw.memsize and
// CPU utilisation from the summed per-process %CPU reported by `ps`, normalised
// by the core count. Both are shell-outs; either failing yields ok=false.
func sampleDarwinLoad() (float64, float64, bool) {
	mem, memOK := darwinMemPct()
	cpu, cpuOK := darwinCPUPct()
	if !memOK || !cpuOK {
		return 0, 0, false
	}
	return cpu, mem, true
}

// darwinMemPct derives used-memory percentage from `vm_stat` (free + inactive
// pages are treated as available) and total RAM from `sysctl hw.memsize`.
func darwinMemPct() (float64, bool) {
	total := probeMemTotalMB() // MB
	if total <= 0 {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return 0, false
	}
	pageSize := int64(4096)
	var freePages, inactivePages int64
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "page size of") {
			for _, tok := range strings.Fields(line) {
				if n, err := strconv.ParseInt(tok, 10, 64); err == nil && n > 0 {
					pageSize = n
					break
				}
			}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimRight(strings.TrimSpace(val), "."), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages free":
			freePages = n
		case "Pages inactive":
			inactivePages = n
		}
	}
	availableMB := (freePages + inactivePages) * pageSize / (1024 * 1024)
	if availableMB > int64(total) {
		return 0, false
	}
	used := int64(total) - availableMB
	return clampPct(float64(used) / float64(total) * 100), true
}

// darwinCPUPct sums the per-process %CPU column from `ps` and normalises by the
// logical core count so the result is a whole-machine 0-100 percentage.
func darwinCPUPct() (float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "%cpu=").Output()
	if err != nil {
		return 0, false
	}
	var sum float64
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		s := strings.TrimSpace(scanner.Text())
		if s == "" {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		sum += v
	}
	cores := runtime.NumCPU()
	if cores <= 0 {
		return 0, false
	}
	return clampPct(sum / float64(cores)), true
}

// SampleLoadAverage returns Unix load averages (1/5/15 min) as best-effort
// floats. ok is true only when all three values were obtained — partial
// samples are not sent, matching SampleLoad's contract of all-or-nothing.
// A panic is recovered so a sampling failure can never abort a heartbeat.
func SampleLoadAverage() (one, five, fifteen float64, ok bool) {
	defer func() {
		if recover() != nil {
			one, five, fifteen, ok = 0, 0, 0, false
		}
	}()
	switch runtime.GOOS {
	case "linux", "darwin":
		return sampleLoadAverage()
	default:
		return 0, 0, 0, false
	}
}

func sampleLoadAverage() (one, five, fifteen float64, ok bool) {
	// Prefer /proc/loadavg on Linux (cheapest, no cgo), fall back to
	// syscall on Darwin. Both are instantaneous single-read probes.
	if runtime.GOOS == "linux" {
		if v, ok := linuxLoadAverage(); ok {
			return v.one, v.five, v.fifteen, true
		}
	}
	// Darwin (and Linux fallback) — try sysctl / getloadavg via shell.
	// On systems where loadavg is unavailable, ok=false and heartbeat omits it.
	return sysctlLoadAverage()
}

type loadTriple struct{ one, five, fifteen float64 }

func linuxLoadAverage() (loadTriple, bool) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return loadTriple{}, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return loadTriple{}, false
	}
	one, err1 := strconv.ParseFloat(fields[0], 64)
	five, err2 := strconv.ParseFloat(fields[1], 64)
	fifteen, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return loadTriple{}, false
	}
	if one < 0 || five < 0 || fifteen < 0 {
		return loadTriple{}, false
	}
	return loadTriple{one: one, five: five, fifteen: fifteen}, true
}

func sysctlLoadAverage() (one, five, fifteen float64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// `sysctl -n vm.loadavg` on Darwin; `cat /proc/loadavg` already tried on Linux.
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		// Last resort: try reading the sysctl MIB via `sysctl` text, or fall back to
		// proc path again on unexpected platform.
		return 0, 0, 0, false
	}
	// Darwin vm.loadavg: "{ 1.23 0.98 0.87 }" or "1.23 0.98 0.87"
	clean := strings.Trim(strings.TrimSpace(string(out)), "{} ")
	fields := strings.Fields(clean)
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	one, err1 := strconv.ParseFloat(fields[0], 64)
	five, err2 := strconv.ParseFloat(fields[1], 64)
	fifteen, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || one < 0 || five < 0 || fifteen < 0 {
		return 0, 0, 0, false
	}
	return one, five, fifteen, true
}

// clampPct bounds a percentage to [0,100].
func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
