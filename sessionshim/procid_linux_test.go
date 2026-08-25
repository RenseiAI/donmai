package sessionshim

import (
	"encoding/binary"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// statFixture renders a /proc/<pid>/stat body. comm is inserted verbatim
// between the parentheses so a caller can reproduce the awkward real-world
// case: a command name that itself contains spaces and parentheses, which is
// why the parser counts fields from the LAST ')' rather than splitting.
func statFixture(comm, starttime string) []byte {
	afterComm := []string{
		"S",        //  3 state
		"1",        //  4 ppid
		"4242",     //  5 pgrp
		"4242",     //  6 session
		"0",        //  7 tty_nr
		"-1",       //  8 tpgid
		"4194304",  //  9 flags
		"100",      // 10 minflt
		"0",        // 11 cminflt
		"7",        // 12 majflt
		"0",        // 13 cmajflt
		"5",        // 14 utime
		"9",        // 15 stime
		"0",        // 16 cutime
		"0",        // 17 cstime
		"20",       // 18 priority
		"0",        // 19 nice
		"3",        // 20 num_threads
		"0",        // 21 itrealvalue
		starttime,  // 22 starttime
		"12345678", // 23 vsize
		"512",      // 24 rss
	}
	return []byte("4242 (" + comm + ") " + strings.Join(afterComm, " ") + "\n")
}

func TestStartTimeUnixNano(t *testing.T) {
	t.Parallel()

	const boot = 1_700_000_000 * int64(time.Second)

	cases := []struct {
		name      string
		comm      string
		starttime string
		clock     bootClock
		want      int64
		wantErr   bool
	}{
		{
			name:      "ordinary comm at 100Hz",
			comm:      "donmai",
			starttime: "12345",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			// 12345 ticks / 100Hz = 123.45s after boot.
			want: 1_700_000_123_450_000_000,
		},
		{
			name:      "comm with spaces and parentheses",
			comm:      "my (odd) comm",
			starttime: "12345",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			want:      1_700_000_123_450_000_000,
		},
		{
			name:      "same ticks at 1000Hz are ten times sooner",
			comm:      "my (odd) comm",
			starttime: "12345",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 1000},
			want:      1_700_000_012_345_000_000,
		},
		{
			name:      "tick rate that does not divide a second evenly",
			comm:      "donmai",
			starttime: "1025",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 1024},
			// 1 whole second plus 1 tick: 1e9/1024 = 976562ns (truncated).
			want: 1_700_000_001_000_976_562,
		},
		{
			name:      "zero starttime is the boot instant itself",
			comm:      "kthreadd",
			starttime: "0",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			want:      boot,
		},
		{
			name:      "starttime beyond a 32-bit tick count",
			comm:      "donmai",
			starttime: "500000000000",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			// 5e9 seconds of ticks: proves the conversion does not overflow
			// the way ticks*1e9 would.
			want: boot + 5_000_000_000*int64(time.Second),
		},
		{
			name:      "negative starttime is refused",
			comm:      "donmai",
			starttime: "-1",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			wantErr:   true,
		},
		{
			name:      "non-numeric starttime is refused",
			comm:      "donmai",
			starttime: "not-a-number",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 100},
			wantErr:   true,
		},
		{
			name:      "zero tick rate is refused",
			comm:      "donmai",
			starttime: "12345",
			clock:     bootClock{unixNano: boot, ticksPerSecond: 0},
			wantErr:   true,
		},
		{
			name:      "sub-nanosecond tick rate is refused",
			comm:      "donmai",
			starttime: "12345",
			clock:     bootClock{unixNano: boot, ticksPerSecond: int64(time.Second) + 1},
			wantErr:   true,
		},
		{
			name:      "missing boot instant is refused",
			comm:      "donmai",
			starttime: "12345",
			clock:     bootClock{unixNano: 0, ticksPerSecond: 100},
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := startTimeUnixNano(statFixture(tc.comm, tc.starttime), 4242, tc.clock)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("startTimeUnixNano = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("startTimeUnixNano: %v", err)
			}
			if got != tc.want {
				t.Fatalf("startTimeUnixNano = %d, want %d (delta %d ns)", got, tc.want, got-tc.want)
			}
		})
	}
}

func TestStartTimeUnixNanoRejectsMalformedStat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		stat string
	}{
		{name: "empty", stat: ""},
		{name: "no closing paren", stat: "4242 (donmai S 1 2 3"},
		{name: "nothing after comm", stat: "4242 (donmai)"},
		{name: "too few fields after comm", stat: "4242 (donmai) S 1 4242 4242 0 -1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := startTimeUnixNano([]byte(tc.stat), 4242, bootClock{unixNano: 1, ticksPerSecond: 100}); err == nil {
				t.Fatalf("startTimeUnixNano = %d, want error", got)
			}
		})
	}
}

func TestParseBootTimeSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		stat    string
		want    int64
		wantErr bool
	}{
		{
			name: "btime among other lines",
			stat: "cpu  1 2 3 4\ncpu0 1 2 3 4\nintr 0\nbtime 1700000000\nprocesses 4242\n",
			want: 1_700_000_000,
		},
		{
			name: "btime is the last line without a trailing newline",
			stat: "cpu  1 2 3 4\nbtime 1700000000",
			want: 1_700_000_000,
		},
		{name: "no btime line", stat: "cpu  1 2 3 4\nprocesses 4242\n", wantErr: true},
		{name: "btime prefix is not btime", stat: "btimex 1700000000\n", wantErr: true},
		{name: "non-numeric btime", stat: "btime later\n", wantErr: true},
		{name: "zero btime", stat: "btime 0\n", wantErr: true},
		{name: "negative btime", stat: "btime -1\n", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBootTimeSeconds([]byte(tc.stat))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBootTimeSeconds = %d, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBootTimeSeconds: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseBootTimeSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

// auxvFixture encodes (type, value) pairs the way the kernel lays out
// /proc/self/auxv: native-endian unsigned longs, in this build's word size.
//
// Values are uint32 so the fixture widens rather than narrows on a 64-bit
// build; every auxiliary-vector key and tick rate this test needs fits.
func auxvFixture(pairs ...uint32) []byte {
	word := bits.UintSize / 8
	out := make([]byte, 0, len(pairs)*word)
	for _, v := range pairs {
		buf := make([]byte, word)
		if word == 8 {
			binary.NativeEndian.PutUint64(buf, uint64(v))
		} else {
			binary.NativeEndian.PutUint32(buf, v)
		}
		out = append(out, buf...)
	}
	return out
}

func TestAuxvValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		auxv  []byte
		want  int64
		found bool
	}{
		{
			name:  "clock tick after other entries",
			auxv:  auxvFixture(6, 4096, 11, 1000, auxvClockTick, 1000, 0, 0),
			want:  1000,
			found: true,
		},
		{
			name:  "clock tick is the first entry",
			auxv:  auxvFixture(auxvClockTick, 100, 0, 0),
			want:  100,
			found: true,
		},
		{name: "absent", auxv: auxvFixture(6, 4096, 0, 0)},
		{name: "AT_NULL stops the scan before the entry", auxv: auxvFixture(0, 0, auxvClockTick, 100)},
		{name: "empty vector", auxv: nil},
		{name: "truncated pair", auxv: auxvFixture(auxvClockTick)[:bits.UintSize/8]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := auxvValue(tc.auxv, auxvClockTick)
			if ok != tc.found {
				t.Fatalf("auxvValue found = %v, want %v", ok, tc.found)
			}
			if ok && got != tc.want {
				t.Fatalf("auxvValue = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadBootClockFromFixtures(t *testing.T) {
	// Not parallel: this rebinds the package-level /proc paths.
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	auxvPath := filepath.Join(dir, "auxv")
	if err := os.WriteFile(statPath, []byte("cpu  1 2 3\nbtime 1700000000\n"), 0o600); err != nil {
		t.Fatalf("write stat fixture: %v", err)
	}
	if err := os.WriteFile(auxvPath, auxvFixture(auxvClockTick, 1000, 0, 0), 0o600); err != nil {
		t.Fatalf("write auxv fixture: %v", err)
	}

	origStat, origAuxv := procStatPath, procSelfAuxvPath
	t.Cleanup(func() { procStatPath, procSelfAuxvPath = origStat, origAuxv })
	procStatPath, procSelfAuxvPath = statPath, auxvPath

	clock, err := readBootClock()
	if err != nil {
		t.Fatalf("readBootClock: %v", err)
	}
	if clock.unixNano != 1_700_000_000*int64(time.Second) {
		t.Fatalf("boot instant = %d, want %d", clock.unixNano, 1_700_000_000*int64(time.Second))
	}
	if clock.ticksPerSecond != 1000 {
		t.Fatalf("tick rate = %d, want 1000", clock.ticksPerSecond)
	}

	// An unreadable auxiliary vector falls back to USER_HZ rather than
	// disabling identity on the host.
	procSelfAuxvPath = filepath.Join(dir, "absent")
	clock, err = readBootClock()
	if err != nil {
		t.Fatalf("readBootClock with absent auxv: %v", err)
	}
	if clock.ticksPerSecond != fallbackClockTicks {
		t.Fatalf("tick rate = %d, want fallback %d", clock.ticksPerSecond, fallbackClockTicks)
	}

	// An unreadable /proc/stat is a hard error: fabricating a boot instant
	// would fabricate every identity derived from it.
	procStatPath = filepath.Join(dir, "absent")
	if _, err = readBootClock(); err == nil {
		t.Fatal("readBootClock with absent /proc/stat = nil error, want failure")
	}
}

// TestSelfStartTimeMatchesProcessTable is the cross-check the package could not
// perform on itself: Alive() compares this package's own output against this
// package's own output, so it stays consistent no matter what unit it invents.
// `ps -o lstart=` is an independent reader of the same kernel facts, and it
// reports a wall-clock instant — which is what the identity contract promises.
func TestSelfStartTimeMatchesProcessTable(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("no ps on PATH (%v): install procps to run the process-table cross-check", err)
	}

	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}

	// #nosec G204 -- fixed command; the only variable is this process's own pid.
	out, err := exec.Command("ps", "-p", strconv.Itoa(self.PID), "-o", "lstart=").Output()
	if err != nil {
		t.Fatalf("ps -p %d -o lstart=: %v", self.PID, err)
	}
	// ps renders lstart in the local zone, e.g. "Mon Aug 25 05:08:04 2026",
	// space-padding a single-digit day.
	reported := strings.TrimSpace(string(out))
	psStart, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", reported, time.Local)
	if err != nil {
		t.Fatalf("parse ps lstart %q: %v", reported, err)
	}

	gotSeconds := self.StartedAt / int64(time.Second)
	if gotSeconds != psStart.Unix() {
		t.Fatalf("Self().StartedAt = %d ns (%s), which is %d in Unix seconds; ps reports %q = %d. "+
			"StartedAt is documented as Unix nanoseconds",
			self.StartedAt, time.Unix(0, self.StartedAt).Format(time.RFC3339), gotSeconds, reported, psStart.Unix())
	}

	alive, err := self.Alive()
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}
	if !alive {
		t.Fatalf("Alive() = false for the running process %s", self)
	}
}
