//go:build linux

package sessionshim

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcessIdentity.StartedAt is documented as Unix nanoseconds, and that is what
// every consumer of the identity transports: the authenticated hello, the
// registry record, the tombstone, and the adopted-session projection a daemon
// serves. Field 22 (starttime) of /proc/<pid>/stat is not that — it is clock
// ticks since boot, a number in the millions where a nanosecond timestamp is a
// number in the quintillions.
//
// Returning the raw ticks is self-consistent INSIDE this package, because
// Alive() only ever compares a recorded value against a freshly read one, and
// that is why the mismatch survived. It is not self-consistent anywhere else:
// an observer that cross-checks the reported start against the process table
// (`ps -o lstart=`) cannot reconcile ticks with a timestamp, and two hosts on
// different platforms report the same instant in incomparable units. So the
// ticks are converted here, through the host's boot instant and its tick rate,
// into the unit the contract names.
const (
	// procStatStartTimeIndex is the index of starttime (field 22 of
	// /proc/<pid>/stat) once the parenthesised comm field has been consumed:
	// the first field after comm is state (field 3), so field N sits at N-3.
	procStatStartTimeIndex = 19

	// auxvClockTick is AT_CLKTCK, the auxiliary-vector entry carrying the
	// number of starttime ticks in one second.
	auxvClockTick = 17

	// fallbackClockTicks is USER_HZ, the value AT_CLKTCK has on every
	// mainstream Linux build. It is the answer when the auxiliary vector
	// cannot be read at all — a wrong tick rate would be visible as a
	// timestamp off by a factor, whereas refusing to answer would disable
	// adoption on a host whose /proc is merely unusual.
	fallbackClockTicks = 100
)

// procStatPath and procSelfAuxvPath are variables, not constants, so tests can
// point the boot-clock reader at fixtures. Production never reassigns them.
var (
	procStatPath     = "/proc/stat"
	procSelfAuxvPath = "/proc/self/auxv"
)

// bootClock is the host's boot instant paired with the rate at which
// /proc/<pid>/stat counts ticks. Together they turn ticks-since-boot into a
// wall-clock instant.
type bootClock struct {
	unixNano       int64
	ticksPerSecond int64
}

// hostBootClock resolves the host boot clock once per process.
//
// The caching is load-bearing, not an optimisation. /proc/stat's btime is a
// derived value — the kernel reports (wall clock - time since boot) — so a
// re-read after the system clock is stepped can move it by a second. Alive()
// asks for EQUALITY against a previously recorded identity, so a btime that
// shifted mid-process would make a live process look dead. Reading it once
// fixes the frame of reference for the life of the process, which is exactly
// the property the equality comparison needs.
var (
	bootClockOnce  sync.Once
	bootClockValue bootClock
	bootClockErr   error
)

func hostBootClock() (bootClock, error) {
	bootClockOnce.Do(func() {
		bootClockValue, bootClockErr = readBootClock()
	})
	return bootClockValue, bootClockErr
}

// processStartTime reports when pid started, in Unix nanoseconds.
func processStartTime(pid int) (int64, error) {
	// Read the pid's stat FIRST: a vanished pid must report os.ErrNotExist
	// even on a host where the boot clock is unreadable, because that is the
	// difference between "this session is over" and "this host cannot verify
	// anything", and the two are handled very differently upstream.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("sessionshim: read /proc/%d/stat: %w", pid, err)
	}
	clock, err := hostBootClock()
	if err != nil {
		return 0, err
	}
	return startTimeUnixNano(data, pid, clock)
}

// startTimeUnixNano converts the body of a /proc/<pid>/stat file into Unix
// nanoseconds against the supplied boot clock. Pure: every input is an
// argument, so the conversion is testable without a live process.
func startTimeUnixNano(stat []byte, pid int, clock bootClock) (int64, error) {
	ticks, err := parseStartTimeTicks(stat, pid)
	if err != nil {
		return 0, err
	}
	// The upper bound is not decoration: the remainder term below multiplies
	// by a second's worth of nanoseconds, so a tick rate finer than one tick
	// per nanosecond would overflow — and would carry no information a
	// nanosecond timestamp could hold anyway.
	if clock.ticksPerSecond <= 0 || clock.ticksPerSecond > int64(time.Second) {
		return 0, fmt.Errorf("sessionshim: unusable clock tick rate %d", clock.ticksPerSecond)
	}
	if clock.unixNano <= 0 {
		return 0, fmt.Errorf("sessionshim: unusable boot instant %d", clock.unixNano)
	}
	// Split into whole seconds plus a remainder instead of the direct
	// ticks*(1e9/ticksPerSecond): the direct form silently loses precision
	// whenever the tick rate does not divide a second evenly, and
	// ticks*1e9 overflows int64 on a host up for a few years. The remainder
	// is bounded by the tick rate, so neither term can overflow.
	seconds := ticks / clock.ticksPerSecond
	remainder := ticks % clock.ticksPerSecond
	// No clamp to a positive tick count is needed or wanted. The previous
	// implementation forced a zero starttime up to 1 tick so that Record
	// validation (processStartedAt > 0) would accept it; with the boot
	// instant as the base, a zero starttime is simply the boot instant
	// itself — already positive, and truthful for the kernel threads that
	// report it.
	return clock.unixNano + seconds*int64(time.Second) + remainder*int64(time.Second)/clock.ticksPerSecond, nil
}

// parseStartTimeTicks extracts field 22 (starttime) from a /proc/<pid>/stat body.
func parseStartTimeTicks(stat []byte, pid int) (int64, error) {
	// comm (field 2) is parenthesised and may itself contain spaces and
	// parentheses, so fields are counted from the LAST ')'.
	commEnd := strings.LastIndexByte(string(stat), ')')
	if commEnd < 0 || commEnd+2 >= len(stat) {
		return 0, fmt.Errorf("sessionshim: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(stat[commEnd+2:]))
	if len(fields) <= procStatStartTimeIndex {
		return 0, fmt.Errorf("sessionshim: /proc/%d/stat has %d fields after comm, need > %d", pid, len(fields), procStatStartTimeIndex)
	}
	ticks, err := strconv.ParseInt(fields[procStatStartTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sessionshim: parse /proc/%d/stat starttime: %w", pid, err)
	}
	if ticks < 0 {
		return 0, fmt.Errorf("sessionshim: /proc/%d/stat starttime %d is negative", pid, ticks)
	}
	return ticks, nil
}

// readBootClock resolves the boot instant and the tick rate from /proc.
func readBootClock() (bootClock, error) {
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		return bootClock{}, fmt.Errorf("sessionshim: read %s: %w", procStatPath, err)
	}
	seconds, err := parseBootTimeSeconds(data)
	if err != nil {
		return bootClock{}, err
	}
	return bootClock{
		unixNano:       seconds * int64(time.Second),
		ticksPerSecond: readClockTicks(),
	}, nil
}

// parseBootTimeSeconds extracts the btime line — the boot instant in whole Unix
// seconds — from a /proc/stat body.
func parseBootTimeSeconds(stat []byte) (int64, error) {
	for _, line := range strings.Split(string(stat), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("sessionshim: parse %s btime: %w", procStatPath, err)
		}
		if seconds <= 0 {
			return 0, fmt.Errorf("sessionshim: %s btime %d is not a usable boot instant", procStatPath, seconds)
		}
		return seconds, nil
	}
	return 0, fmt.Errorf("sessionshim: %s has no btime line", procStatPath)
}

// readClockTicks reports AT_CLKTCK from the auxiliary vector, falling back to
// USER_HZ when it cannot be read.
func readClockTicks() int64 {
	data, err := os.ReadFile(procSelfAuxvPath)
	if err != nil {
		return fallbackClockTicks
	}
	if ticks, ok := auxvValue(data, auxvClockTick); ok && ticks > 0 {
		return ticks
	}
	return fallbackClockTicks
}

// auxvValue scans an auxiliary vector for one entry type.
//
// The vector is a sequence of (type, value) pairs of native-endian unsigned
// longs, terminated by a type of AT_NULL (0).
func auxvValue(auxv []byte, entryType uint64) (int64, bool) {
	word := bits.UintSize / 8
	word64 := word == 8
	read := func(b []byte) uint64 {
		if word64 {
			return binary.NativeEndian.Uint64(b)
		}
		return uint64(binary.NativeEndian.Uint32(b))
	}
	for offset := 0; offset+2*word <= len(auxv); offset += 2 * word {
		key := read(auxv[offset:])
		if key == 0 {
			return 0, false // AT_NULL terminates the vector
		}
		if key == entryType {
			value := read(auxv[offset+word:])
			if value > math.MaxInt64 {
				return 0, false
			}
			return int64(value), true
		}
	}
	return 0, false
}
