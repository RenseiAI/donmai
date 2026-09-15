//go:build !windows

package codex

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestOwnedGroupStopEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                                                                 string
		reaped, missingAnchor, zombie, inspectionFails                       bool
		signalErr                                                            error
		exitsOnSignal, losesAnchor, inspectionFailsAfterSignal, killRequired bool
		wantErr                                                              bool
		wantSignals                                                          []syscall.Signal
	}{
		{name: "already-quiescent", zombie: true},
		{name: "TERM-joins-writer", exitsOnSignal: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "KILL-before-reap", killRequired: true, wantSignals: []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}},
		{name: "reaped-leader-reused-group", reaped: true, wantErr: true},
		{name: "missing-anchor-foreign-group", missingAnchor: true, wantErr: true},
		{name: "inspection-EPERM", inspectionFails: true, wantErr: true},
		{name: "TERM-EPERM-live", signalErr: syscall.EPERM, wantErr: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "TERM-ESRCH-live-is-not-proof", signalErr: syscall.ESRCH, wantErr: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "TERM-EPERM-zombie", signalErr: syscall.EPERM, exitsOnSignal: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "TERM-ESRCH-zombie", signalErr: syscall.ESRCH, exitsOnSignal: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "EPERM-then-inspection-failure", signalErr: syscall.EPERM, inspectionFailsAfterSignal: true, wantErr: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
		{name: "anchor-lost-before-KILL", losesAnchor: true, wantErr: true, wantSignals: []syscall.Signal{syscall.SIGTERM}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The synthetic PID is consumed only by the injected operations below.
			cmd := &exec.Cmd{Process: &os.Process{Pid: 42}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
			if tc.reaped {
				cmd.ProcessState = &os.ProcessState{}
			}
			state := ownedGroupState{anchored: !tc.missingAnchor, writersRunning: !tc.zombie}
			var signals []syscall.Signal
			inspect := func(group int) (ownedGroupState, error) {
				if group != 42 {
					t.Fatalf("inspected foreign group %d", group)
				}
				if tc.reaped {
					t.Fatal("inspected reusable number after reap")
				}
				if tc.inspectionFails || (tc.inspectionFailsAfterSignal && len(signals) > 0) {
					return ownedGroupState{}, syscall.EPERM
				}
				return state, nil
			}
			signal := func(group int, sig syscall.Signal) error {
				if group != -42 || !state.anchored || tc.reaped {
					t.Fatalf("foreign signal: group=%d signal=%v", group, sig)
				}
				if sig != syscall.SIGTERM && sig != syscall.SIGKILL {
					t.Fatalf("unexpected signal %v", sig)
				}
				signals = append(signals, sig)
				if tc.exitsOnSignal || (tc.killRequired && sig == syscall.SIGKILL) {
					state.writersRunning = false
				}
				if tc.losesAnchor {
					state.anchored = false
				}
				return tc.signalErr
			}
			boundary, err := newCodexConfigBoundary(t.TempDir(), false)
			if err != nil {
				t.Fatal(err)
			}
			boundary.stopWriter = func() error { return stopOwnedProcessGroup(cmd, time.Nanosecond, inspect, signal) }
			err = boundary.remove()
			if (err != nil) != tc.wantErr {
				t.Fatalf("remove error=%v, wantErr=%v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(signals, tc.wantSignals) {
				t.Fatalf("signals=%v, want %v", signals, tc.wantSignals)
			}
			_, statErr := os.Stat(boundary.configPath)
			if tc.wantErr && statErr != nil {
				t.Fatalf("uncertain writer lost config: %v", statErr)
			}
			if !tc.wantErr && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("quiescent ephemeral home survives: %v", statErr)
			}
		})
	}
}

func TestOwnedGroupStateRequiresOwnedAnchor(t *testing.T) {
	for _, tc := range []struct {
		name, table                string
		anchored, running, wantErr bool
	}{
		{"owned-live", "42 7 42 S\n43 42 42 S\n99 1 99 S", true, true, false},
		{"zombie-leader-live-descendant", "42 7 42 Z\n43 1 42 S", true, true, false},
		{"zombies-only", "42 7 42 Z\n43 1 42 Z", true, false, false},
		{"same-number-foreign-parent", "42 99 42 S", false, true, false},
		{"leader-missing", "43 1 42 S", false, true, false},
		{"leader-left-group", "42 7 99 S\n43 1 42 S", false, true, false},
		{"malformed", "42 7 42", false, false, true},
		{"invalid-id", "42 x 42 S", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := parseOwnedProcessGroupState(tc.table, 42, 7)
			if (err != nil) != tc.wantErr || state.anchored != tc.anchored || state.writersRunning != tc.running {
				t.Fatalf("state=%+v err=%v", state, err)
			}
		})
	}
}
