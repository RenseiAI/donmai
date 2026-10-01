//go:build !windows

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
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

func waitForOwnedGrace(t *testing.T, p *ownedProcess, want time.Duration) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if time.Duration(p.grace.Load()) == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("requested grace = %s, want %s", time.Duration(p.grace.Load()), want)
}

func TestOwnedProcessInFlightGraceOnlyTightens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantKill bool
	}{
		{name: "zero-request", wantKill: true},
		{name: "inspection-failure"},
		{name: "anchor-loss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &exec.Cmd{Process: &os.Process{Pid: 42}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
			owner := newOwnedProcess(cmd)
			active := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			state := ownedGroupState{anchored: true, writersRunning: true}
			inspections := 0
			inspect := func(group int) (ownedGroupState, error) {
				if group != 42 || cmd.ProcessState != nil {
					t.Fatalf("inspected foreign or reaped group %d", group)
				}
				inspections++
				if inspections == 2 {
					close(active) // after TERM and after the grace clock starts
					<-release
					switch tc.name {
					case "inspection-failure":
						return ownedGroupState{}, syscall.EPERM
					case "anchor-loss":
						state.anchored = false
					}
				}
				return state, nil
			}
			var signals []syscall.Signal
			signal := func(group int, sig syscall.Signal) error {
				if group != -42 || !state.anchored || cmd.ProcessState != nil {
					t.Fatalf("foreign or unanchored signal: group=%d signal=%v", group, sig)
				}
				signals = append(signals, sig)
				if sig == syscall.SIGKILL {
					state.writersRunning = false
				}
				return nil
			}
			finished := make(chan error, 1)
			go func() {
				err := stopOwnedProcessGroupWithUpdates(cmd, 5*time.Second, owner.request, func() time.Duration {
					return time.Duration(owner.grace.Load())
				}, nil, inspect, signal)
				owner.stopErr = err
				close(owner.stopped)
				finished <- err
			}()
			select {
			case <-active:
			case <-time.After(time.Second):
				t.Fatal("owned group did not enter active grace")
			}
			stops := make(chan error, 3)
			go func() { stops <- owner.stop(2 * time.Second) }()
			waitForOwnedGrace(t, owner, 2*time.Second)
			deadline := time.Now().Add(time.Second)
			for len(owner.request) != 1 && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			if len(owner.request) != 1 {
				t.Fatal("first request did not fill wake channel")
			}
			go func() { stops <- owner.stop(0) }()
			waitForOwnedGrace(t, owner, 0)
			go func() { stops <- owner.stop(10 * time.Second) }()
			close(release)
			var err error
			select {
			case err = <-finished:
			case <-time.After(time.Second):
				t.Fatal("zero grace was lost behind a full wake channel")
			}
			for range 3 {
				if stopErr := <-stops; (stopErr != nil) != (err != nil) {
					t.Fatalf("stop result %v differs from owner result %v", stopErr, err)
				}
			}
			if (err == nil) != tc.wantKill {
				t.Fatalf("cleanup error = %v, wantKill=%t", err, tc.wantKill)
			}
			wantSignals := []syscall.Signal{syscall.SIGTERM}
			if tc.wantKill {
				wantSignals = append(wantSignals, syscall.SIGKILL)
			}
			if !reflect.DeepEqual(signals, wantSignals) {
				t.Fatalf("signals = %v, want %v", signals, wantSignals)
			}
			if got := time.Duration(owner.grace.Load()); got != 0 {
				t.Fatalf("longer concurrent request extended zero grace to %s", got)
			}
		})
	}
}

func TestOwnedProcessLateLongerRequestCannotExtendGrace(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 42}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	owner := newOwnedProcess(cmd)
	active := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	state := ownedGroupState{anchored: true, writersRunning: true}
	inspections := 0
	inspect := func(group int) (ownedGroupState, error) {
		if group != 42 {
			t.Fatalf("inspected foreign group %d", group)
		}
		inspections++
		if inspections == 2 {
			close(active)
			<-release
		}
		return state, nil
	}
	var signals []syscall.Signal
	signal := func(group int, sig syscall.Signal) error {
		if group != -42 || !state.anchored || cmd.ProcessState != nil {
			t.Fatalf("foreign or reaped signal: group=%d signal=%v", group, sig)
		}
		signals = append(signals, sig)
		if sig == syscall.SIGKILL {
			state.writersRunning = false
		}
		return nil
	}
	finished := make(chan error, 1)
	go func() {
		err := stopOwnedProcessGroupWithUpdates(cmd, 80*time.Millisecond, owner.request, func() time.Duration {
			return time.Duration(owner.grace.Load())
		}, nil, inspect, signal)
		owner.stopErr = err
		close(owner.stopped)
		finished <- err
	}()
	select {
	case <-active:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("owned group did not enter grace")
	}
	stopDone := make(chan error, 2)
	go func() { stopDone <- owner.stop(10 * time.Second) }()
	waitForOwnedGrace(t, owner, 10*time.Second)
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("owned cleanup: %v", err)
		}
	case <-time.After(time.Second):
		// Ensure the synthetic owner can finish even when the assertion finds
		// an extended grace; no test process group is ever signalled here.
		go func() { stopDone <- owner.stop(0) }()
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
		t.Fatal("late longer request extended the already-running grace")
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("late stop: %v", err)
	}
	if !reflect.DeepEqual(signals, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("signals = %v", signals)
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

func TestCheckHostSessionLoginCancellationShortensActiveGroupGrace(t *testing.T) {
	privateRoot, err := os.MkdirTemp("", "codex-owned-cancellation-")
	if err != nil {
		t.Fatal(err)
	}
	group := 0
	t.Cleanup(func() {
		// Inspect only our recorded group. Never signal from a test cleanup.
		deadline := time.Now().Add(17 * time.Second)
		for {
			if group == 0 {
				t.Logf("retained private fixture: exact owned group was not observed: %s", privateRoot)
				return
			}
			state, inspectErr := ownedProcessGroupState(group)
			if inspectErr == nil && !state.writersRunning {
				_ = os.RemoveAll(privateRoot)
				return
			}
			if time.Now().After(deadline) {
				t.Logf("retained private fixture: owned quiescence unproved: %s", privateRoot)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	t.Setenv("TMPDIR", privateRoot)
	syntheticHostAuth(t, syntheticChatGPTAuth)
	// The launcher exits after printing valid status while an owned writer
	// ignores TERM. Cancellation arrives after the owner's normal grace began.
	pidFile := filepath.Join(privateRoot, "owned-child.pid")
	readyFile := filepath.Join(privateRoot, "owned-child.ready")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	binary := syntheticStatusBinary(t, "READY="+quote(readyFile)+"\n(trap '' TERM; : > \"$READY\"; i=0; while [ \"$i\" -lt 15 ]; do i=$((i+1)); sleep 1; done) >/dev/null 2>&1 &\nprintf '%s\\n' \"$!\" > "+quote(pidFile)+"\ni=0\nwhile [ ! -f \"$READY\" ] && [ \"$i\" -lt 100 ]; do i=$((i+1)); sleep 0.01; done\n[ -f \"$READY\" ] || exit 7\nprintf 'Logged in using ChatGPT\\n' >&2\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	graceStarted := make(chan struct{})
	var processOwner *ownedProcess
	done := make(chan error, 1)
	go func() {
		done <- checkHostSessionLogin(ctx, binary, func(cmd *exec.Cmd) hostStatusOwner {
			owner := newOwnedProcess(cmd)
			processOwner = owner
			owner.graceStarted = graceStarted
			return owner
		})
	}()
	select {
	case <-graceStarted:
	case <-time.After(2 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("synthetic owner did not finish after cancellation")
		}
		t.Fatal("synthetic status did not enter owned-group grace")
	}
	group = processOwner.cmd.Process.Pid
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || childPID <= 0 || group <= 0 {
		t.Fatal("invalid exact owned fixture identity")
	}
	started := time.Now()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled login status accepted")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled status held caller through owned-group grace: %s", elapsed)
	}
	state, inspectErr := ownedProcessGroupState(group)
	if inspectErr != nil || state.writersRunning {
		t.Fatalf("owned child %d/group %d remains active or unproved: state=%+v err=%v", childPID, group, state, inspectErr)
	}
	t.Logf("caller rejected promptly; exact owned child %d/group %d quiescent", childPID, group)
	entries, err := os.ReadDir(privateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), codexHomePrefix) {
			t.Fatal("quiescent owned group left private auth home")
		}
	}
}
