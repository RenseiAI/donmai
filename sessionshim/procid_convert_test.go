package sessionshim

import (
	"fmt"
	"strings"
	"testing"
)

// fakeProcPIDStat renders a /proc/<pid>/stat line whose field 22 (starttime)
// carries ticks. After the comm, field 3 (state) is index 0, so starttime is
// index 19: one state field, eighteen filler fields, then ticks.
func fakeProcPIDStat(pid int, comm string, ticks string) string {
	filler := strings.Repeat("0 ", 18)
	return fmt.Sprintf("%d (%s) R %s%s 0 0", pid, comm, filler, ticks)
}

func TestParseProcPIDStatStartTicks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		pid   int
		comm  string
		ticks string
		want  int64
	}{
		{name: "plain comm", pid: 1234, comm: "mycmd", ticks: "123456", want: 123456},
		{name: "comm with spaces and parens", pid: 42, comm: "my (tricky) name with spaces", ticks: "789", want: 789},
		{name: "comm with nested parens", pid: 7, comm: "a(b(c))d", ticks: "100", want: 100},
		{name: "zero ticks biases to one", pid: 99, comm: "zero", ticks: "0", want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseProcPIDStatStartTicks([]byte(fakeProcPIDStat(tc.pid, tc.comm, tc.ticks)), tc.pid)
			if err != nil {
				t.Fatalf("parseProcPIDStatStartTicks: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseProcPIDStatStartTicks = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseProcPIDStatStartTicksErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		pid  int
	}{
		{name: "no comm paren", data: "1234 mycmd R 1 2 3", pid: 1234},
		{name: "truncated after comm", data: "1234 (mycmd) R 1 2", pid: 1234},
		{name: "non-numeric starttime", data: fakeProcPIDStat(1234, "mycmd", "notanumber"), pid: 1234},
		{name: "empty", data: "", pid: 1234},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseProcPIDStatStartTicks([]byte(tc.data), tc.pid); err == nil {
				t.Fatalf("parseProcPIDStatStartTicks(%q) returned no error", tc.data)
			}
		})
	}
}

func TestParseProcStatBootTimeUnixNano(t *testing.T) {
	t.Parallel()
	data := "cpu  1 2 3 4 5 6 7\n" +
		"btime 1710000000\n" +
		"uptime 1234.56\n"
	got, err := parseProcStatBootTimeUnixNano([]byte(data))
	if err != nil {
		t.Fatalf("parseProcStatBootTimeUnixNano: %v", err)
	}
	if want := int64(1710000000) * 1e9; got != want {
		t.Fatalf("parseProcStatBootTimeUnixNano = %d, want %d", got, want)
	}
}

func TestParseProcStatBootTimeUnixNanoErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "no btime line", data: "cpu 1 2 3\nuptime 1.0\n"},
		{name: "malformed btime line", data: "btime\n"},
		{name: "btime extra fields", data: "btime 1710000000 extra\n"},
		{name: "btime non-numeric", data: "btime notanumber\n"},
		{name: "btime zero", data: "btime 0\n"},
		{name: "btime negative", data: "btime -5\n"},
		{name: "empty", data: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseProcStatBootTimeUnixNano([]byte(tc.data)); err == nil {
				t.Fatalf("parseProcStatBootTimeUnixNano(%q) returned no error", tc.data)
			}
		})
	}
}

func TestProcStartTimeUnixNano(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		ticks int64
		clkHz int64
		boot  int64
		want  int64
	}{
		{
			name:  "whole and fractional seconds",
			ticks: 123456, clkHz: 100, boot: 1710000000 * 1e9,
			want: 1710000000*1e9 + 1234*1e9 + 56*1e9/100,
		},
		{
			name:  "single tick is ten milliseconds at 100Hz",
			ticks: 1, clkHz: 100, boot: 1710000000 * 1e9,
			want: 1710000000*1e9 + 10*1e6,
		},
		{
			name:  "tick rate scales",
			ticks: 500, clkHz: 250, boot: 1710000000 * 1e9,
			want: 1710000000*1e9 + 2*1e9,
		},
		{
			name:  "exact division",
			ticks: 100, clkHz: 100, boot: 1700000000 * 1e9,
			want: 1700000000*1e9 + 1e9,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := procStartTimeUnixNano(tc.ticks, tc.clkHz, tc.boot)
			if err != nil {
				t.Fatalf("procStartTimeUnixNano: %v", err)
			}
			if got != tc.want {
				t.Fatalf("procStartTimeUnixNano(%d, %d, %d) = %d, want %d",
					tc.ticks, tc.clkHz, tc.boot, got, tc.want)
			}
		})
	}
}

func TestProcStartTimeUnixNanoErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		ticks int64
		clkHz int64
		boot  int64
	}{
		{name: "zero ticks", ticks: 0, clkHz: 100, boot: 1710000000 * 1e9},
		{name: "negative ticks", ticks: -5, clkHz: 100, boot: 1710000000 * 1e9},
		{name: "zero tick rate", ticks: 100, clkHz: 0, boot: 1710000000 * 1e9},
		{name: "negative tick rate", ticks: 100, clkHz: -100, boot: 1710000000 * 1e9},
		{name: "zero boot", ticks: 100, clkHz: 100, boot: 0},
		{name: "negative boot", ticks: 100, clkHz: 100, boot: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := procStartTimeUnixNano(tc.ticks, tc.clkHz, tc.boot); err == nil {
				t.Fatalf("procStartTimeUnixNano(%d, %d, %d) returned no error",
					tc.ticks, tc.clkHz, tc.boot)
			}
		})
	}
}

// TestProcStatConversionEndToEnd pins the acceptance conversion with fake
// inputs: ticks from a fake /proc/<pid>/stat line divided by CLK_TCK plus the
// boot time from a fake /proc/stat btime line yields Unix nanoseconds.
func TestProcStatConversionEndToEnd(t *testing.T) {
	t.Parallel()
	const pid = 4242
	const clkHz = 100
	const bootSec = 1710000000

	statLine := fakeProcPIDStat(pid, "fake (cmd) name", "123456")
	ticks, err := parseProcPIDStatStartTicks([]byte(statLine), pid)
	if err != nil {
		t.Fatalf("parseProcPIDStatStartTicks: %v", err)
	}
	boot, err := parseProcStatBootTimeUnixNano([]byte(fmt.Sprintf("cpu 1 2\nbtime %d\n", bootSec)))
	if err != nil {
		t.Fatalf("parseProcStatBootTimeUnixNano: %v", err)
	}
	got, err := procStartTimeUnixNano(ticks, clkHz, boot)
	if err != nil {
		t.Fatalf("procStartTimeUnixNano: %v", err)
	}
	want := int64(bootSec)*1e9 + 1234*1e9 + 56*1e9/100
	if got != want {
		t.Fatalf("end-to-end conversion = %d, want %d (ticks=%d / %d Hz + boot %d)", got, want, ticks, clkHz, boot)
	}
	// The converted value is wall-clock magnitude, not raw ticks: it must
	// exceed any plausible tick count by orders of magnitude.
	if got < 1e18 {
		t.Fatalf("end-to-end conversion = %d, want wall-clock nanoseconds (>= 1e18)", got)
	}
}
