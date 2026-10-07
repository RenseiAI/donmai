package daemon

import (
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/installer/servicepriority"
)

func TestComposeProcessPriorityStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		obs            processPriorityObservation
		configured     servicepriority.Mode
		haveConfigured bool
		wantMode       string
		wantConfigured string
		wantWarning    string // substring; empty means no warning
	}{
		{
			name:     "observed default, nothing saved",
			obs:      processPriorityObservation{mode: servicepriority.Default, evidence: "ps PRI=20"},
			wantMode: "default",
		},
		{
			name:           "observed background matches the saved mode",
			obs:            processPriorityObservation{mode: servicepriority.Background, evidence: "ps PRI=4"},
			configured:     servicepriority.Background,
			haveConfigured: true,
			wantMode:       "background",
			wantConfigured: "background",
		},
		{
			name:           "saved background but the daemon is not demoted",
			obs:            processPriorityObservation{mode: servicepriority.Default, evidence: "ps PRI=20"},
			configured:     servicepriority.Background,
			haveConfigured: true,
			wantMode:       "default",
			wantConfigured: "background",
			wantWarning:    "restart the daemon",
		},
		{
			name:           "saved default but the daemon is demoted",
			obs:            processPriorityObservation{mode: servicepriority.Background, evidence: "SCHED_IDLE, nice 19"},
			configured:     servicepriority.Default,
			haveConfigured: true,
			wantMode:       "background",
			wantConfigured: "default",
			wantWarning:    "restart the daemon",
		},
		{
			name:        "failed observation reports unknown with the reason",
			obs:         processPriorityObservation{warning: "could not read the live process priority: boom"},
			wantMode:    "unknown",
			wantWarning: "boom",
		},
		{
			name:           "failed observation never claims a mismatch",
			obs:            processPriorityObservation{warning: "could not read the live process priority: boom"},
			configured:     servicepriority.Background,
			haveConfigured: true,
			wantMode:       "unknown",
			wantConfigured: "background",
			wantWarning:    "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := composeProcessPriorityStatus(tc.obs, tc.configured, tc.haveConfigured)
			if got.Mode != tc.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.ConfiguredMode != tc.wantConfigured {
				t.Errorf("ConfiguredMode = %q, want %q", got.ConfiguredMode, tc.wantConfigured)
			}
			if tc.wantWarning == "" && got.Warning != "" {
				t.Errorf("Warning = %q, want none", got.Warning)
			}
			if tc.wantWarning != "" && !strings.Contains(got.Warning, tc.wantWarning) {
				t.Errorf("Warning = %q, want it to contain %q", got.Warning, tc.wantWarning)
			}
			if got.Evidence != tc.obs.evidence {
				t.Errorf("Evidence = %q, want %q", got.Evidence, tc.obs.evidence)
			}
		})
	}
}

// TestProcessPriorityCache_ProbesOnce pins that the probe (a subprocess on
// macOS) runs once however many /status and /doctor requests arrive. Re-probing
// per request added ~150 ms to every /status call and made a pinned status test
// fail every run on macOS.
func TestProcessPriorityCache_ProbesOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	cache := &processPriorityCache{probe: func() (processPriorityObservation, bool) {
		calls.Add(1)
		return processPriorityObservation{mode: servicepriority.Background, evidence: "probe"}, true
	}}
	for range 5 {
		obs, supported := cache.get()
		if !supported || obs.mode != servicepriority.Background {
			t.Fatalf("get() = (%+v, %v), want the probed background observation", obs, supported)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times, want 1", got)
	}
}

func TestProcessPriorityCache_UnsupportedIsCachedToo(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	cache := &processPriorityCache{probe: func() (processPriorityObservation, bool) {
		calls.Add(1)
		return processPriorityObservation{}, false
	}}
	for range 3 {
		if _, supported := cache.get(); supported {
			t.Fatal("get() reported supported, want unsupported")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe ran %d times, want 1", got)
	}
}

// procStatLine builds a /proc/<pid>/stat line with the given command name,
// nice and scheduling policy, and zeroes for every other numeric field.
func procStatLine(comm string, nice, policy int) string {
	fields := make([]string, 0, 52)
	fields = append(fields, "4242", "("+comm+")", "S") // fields 1-3
	for i := 4; i <= 52; i++ {
		switch i {
		case procStatNiceField:
			fields = append(fields, strconv.Itoa(nice))
		case procStatPolicyField:
			fields = append(fields, strconv.Itoa(policy))
		default:
			fields = append(fields, "0")
		}
	}
	return strings.Join(fields, " ") + "\n"
}

func TestObserveProcStatPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		stat         string
		readErr      error
		wantMode     servicepriority.Mode
		wantEvidence string
		wantWarning  bool
	}{
		{name: "idle policy is background", stat: procStatLine("donmai", 19, schedIdle), wantMode: servicepriority.Background, wantEvidence: "SCHED_IDLE, nice 19"},
		{name: "ordinary policy is default", stat: procStatLine("donmai", 0, schedOther), wantMode: servicepriority.Default, wantEvidence: "SCHED_OTHER, nice 0"},
		{name: "nice alone does not make background", stat: procStatLine("donmai", 19, schedOther), wantMode: servicepriority.Default, wantEvidence: "SCHED_OTHER, nice 19"},
		{name: "batch policy is not background", stat: procStatLine("donmai", 5, schedBatch), wantMode: servicepriority.Default, wantEvidence: "SCHED_BATCH, nice 5"},
		{name: "command name with spaces and parens", stat: procStatLine("do) (nm ai", 19, schedIdle), wantMode: servicepriority.Background, wantEvidence: "SCHED_IDLE, nice 19"},
		{name: "unknown policy number", stat: procStatLine("donmai", 0, 99), wantMode: servicepriority.Default, wantEvidence: "policy 99, nice 0"},
		{name: "read error", readErr: errors.New("no /proc"), wantWarning: true},
		{name: "truncated line", stat: "4242 (donmai) S 1 2 3\n", wantWarning: true},
		{name: "no command field", stat: "garbage", wantWarning: true},
		{name: "non-numeric nice", stat: strings.Replace(procStatLine("donmai", 19, schedIdle), " 19 ", " x ", 1), wantWarning: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := observeProcStatPriority(func() ([]byte, error) { return []byte(tc.stat), tc.readErr })
			if tc.wantWarning {
				if got.mode != "" || got.warning == "" {
					t.Fatalf("observation = %+v, want a warning and no mode", got)
				}
				return
			}
			if got.warning != "" || got.mode != tc.wantMode || got.evidence != tc.wantEvidence {
				t.Fatalf("observation = %+v, want mode %q evidence %q", got, tc.wantMode, tc.wantEvidence)
			}
		})
	}
}
